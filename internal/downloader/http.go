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
	"time"
)

type downloader struct {
	Options
	client   *http.Client
	progress io.Writer
}

func newDownloader(o Options, output io.Writer) *downloader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: o.Timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = o.Timeout
	transport.MaxConnsPerHost = o.Concurrency
	transport.MaxIdleConnsPerHost = o.Concurrency
	return &downloader{o, &http.Client{Transport: transport, Jar: o.CookieJar}, output}
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
			// ponytail: HLS refetches the playlist after this bounded peek; reuse it if single-use URLs matter.
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
		counter := &byteProgress{out: d.progress}
		_, err := d.request(ctx, asset{uri: d.URL}, io.MultiWriter(f, counter))
		if errors.Is(err, errHLS) {
			return err
		}
		fmt.Fprintf(d.progress, "\rDownloaded %d bytes\n", counter.n)
		if err != nil {
			return err
		}
		return ctx.Err()
	})
}

type byteProgress struct {
	n    int64
	last time.Time
	out  io.Writer
}

func (p *byteProgress) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	if time.Since(p.last) >= 250*time.Millisecond {
		fmt.Fprintf(p.out, "\rDownloading: %d bytes", p.n)
		p.last = time.Now()
	}
	return len(b), nil
}

func (d *downloader) fetchPlaylist(ctx context.Context, uri string) (string, *url.URL, error) {
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
