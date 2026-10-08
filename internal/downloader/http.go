package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type downloader struct {
	Options
	client       *http.Client
	progress     io.Writer
	bytes        *byteProgress
	playlistBody string
	playlistURL  *url.URL
}

func newDownloader(o Options, output io.Writer) *downloader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: o.Timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = o.Timeout
	transport.MaxConnsPerHost = o.Concurrency
	transport.MaxIdleConnsPerHost = o.Concurrency
	return &downloader{Options: o, client: &http.Client{Transport: transport, Jar: o.CookieJar}, progress: output}
}

var errHLS = errors.New("detected HLS playlist")

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func (d *downloader) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= d.Retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = fn(); err == nil {
			return nil
		}
		var status statusError
		var fileError *os.PathError
		if errors.Is(err, errHLS) || errors.As(err, &fileError) || (errors.As(err, &status) && status != 408 && status != 429 && status < 500) {
			return err
		}
		if attempt == d.Retries {
			break
		}
		timer := time.NewTimer(time.Duration(min(attempt+1, 4)) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func (d *downloader) request(ctx context.Context, a asset, w io.Writer) (*url.URL, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.uri, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", d.UserAgent)
	if d.Referer != "" {
		req.Header.Set("Referer", d.Referer)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if a.length > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", a.offset, a.offset+a.length-1))
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	expected := http.StatusOK
	if a.length > 0 {
		expected = http.StatusPartialContent
	}
	if resp.StatusCode != expected {
		return nil, statusError(resp.StatusCode)
	}
	if a.length > 0 {
		prefix := fmt.Sprintf("bytes %d-%d/", a.offset, a.offset+a.length-1)
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), prefix) {
			return nil, errors.New("server returned a mismatched Content-Range")
		}
	}
	var reader io.Reader = resp.Body
	if d.Detect {
		buffered := bufio.NewReader(resp.Body)
		prefix, err := buffered.Peek(11) // BOM + #EXTM3U + line ending.
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		header := strings.TrimPrefix(string(prefix), "\ufeff")
		if header == "#EXTM3U" || (strings.HasPrefix(header, "#EXTM3U") && strings.ContainsAny(header[7:8], "\r\n \t")) {
			deadline := time.AfterFunc(d.Timeout, func() { _ = resp.Body.Close() })
			defer deadline.Stop()
			var body strings.Builder
			if _, err := io.Copy(&limitedWriter{w: &body, remaining: maxPlaylist}, buffered); err != nil {
				return nil, err
			}
			if resp.ContentLength >= 0 && int64(body.Len()) != resp.ContentLength {
				return nil, io.ErrUnexpectedEOF
			}
			d.playlistBody, d.playlistURL = body.String(), resp.Request.URL
			return nil, errHLS
		}
		reader = buffered // Preserve the bytes inspected above for direct files.
	}
	limit := int64(0)
	if a.key {
		limit = 16
	} else if a.length > 0 {
		limit = a.length
	}
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	if d.bytes != nil {
		w = io.MultiWriter(w, d.bytes)
	}
	n, err := io.Copy(w, reader)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errors.New("empty download response")
	}
	if limit > 0 && n != limit {
		return nil, fmt.Errorf("expected %d bytes, received %d", limit, n)
	}
	if a.encrypted && n%16 != 0 {
		return nil, errors.New("AES-128 ciphertext must contain complete 16-byte blocks")
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return nil, io.ErrUnexpectedEOF
	}
	return resp.Request.URL, nil
}

func (d *downloader) fetchRaw(ctx context.Context, f *os.File) error {
	return d.retry(ctx, func() error {
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		counter := startByteProgress(d.progress, 0)
		_, err := d.request(ctx, asset{uri: d.URL}, io.MultiWriter(f, counter))
		counter.stop()
		if err != nil {
			return err
		}
		return ctx.Err()
	})
}

type byteProgress struct {
	n, done atomic.Int64
	total   int
	last    time.Time
	lastN   int64
	out     io.Writer
	finish  chan struct{}
	stopped chan struct{}
}

func (p *byteProgress) Write(b []byte) (int, error) {
	p.n.Add(int64(len(b)))
	return len(b), nil
}

func startByteProgress(out io.Writer, total int) *byteProgress {
	p := &byteProgress{out: out, total: total, last: time.Now(), finish: make(chan struct{}), stopped: make(chan struct{})}
	go func() {
		defer close(p.stopped)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.print(time.Now())
			case <-p.finish:
				if p.n.Load() > 0 || p.total > 0 {
					p.print(time.Now())
					fmt.Fprintln(p.out)
				}
				return
			}
		}
	}()
	return p
}

func (p *byteProgress) stop() {
	close(p.finish)
	<-p.stopped
}

func (p *byteProgress) print(now time.Time) {
	n := p.n.Load()
	speed := float64(n-p.lastN) / max(now.Sub(p.last).Seconds(), 0.001)
	if p.total > 0 {
		done := p.done.Load()
		fmt.Fprintf(p.out, "\rDownloading: %3d%% (%d/%d) · %s · %s/s   ", done*100/int64(p.total), done, p.total, formatBytes(float64(n)), formatBytes(speed))
	} else {
		fmt.Fprintf(p.out, "\rDownloading: %s · %s/s   ", formatBytes(float64(n)), formatBytes(speed))
	}
	p.last, p.lastN = now, n
}

func formatBytes(n float64) string {
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if n < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
		n /= 1024
	}
	return ""
}

func (d *downloader) fetchPlaylist(ctx context.Context, uri string) (string, *url.URL, error) {
	if d.playlistURL != nil && uri == d.URL {
		body, base := d.playlistBody, d.playlistURL
		d.playlistBody, d.playlistURL = "", nil
		return body, base, ctx.Err()
	}
	var body strings.Builder
	var finalURL *url.URL
	err := d.retry(ctx, func() error {
		body.Reset()
		attemptCtx, cancel := context.WithTimeout(ctx, d.Timeout)
		defer cancel()
		var err error
		finalURL, err = d.request(attemptCtx, asset{uri: uri}, &limitedWriter{w: &body, remaining: maxPlaylist})
		return err
	})
	return body.String(), finalURL, err
}

type limitedWriter struct {
	w         io.Writer
	remaining int
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errors.New("playlist exceeds 8 MiB")
	}
	n, err := w.w.Write(b)
	w.remaining -= n
	return n, err
}
