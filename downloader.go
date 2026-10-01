package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type downloader struct {
	options
	client   *http.Client
	progress io.Writer
}

func newDownloader(o options, output io.Writer) *downloader {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{Timeout: o.timeout, KeepAlive: 30 * time.Second}).DialContext
	transport.ResponseHeaderTimeout = o.timeout
	transport.MaxConnsPerHost = o.concurrency
	transport.MaxIdleConnsPerHost = o.concurrency
	return &downloader{o, &http.Client{Transport: transport}, output}
}

// Publish a complete file without ever replacing an existing destination, even in a race.
// Staging on the destination filesystem makes the hard link atomic.
func publish(out string, write func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(out), ".javit-dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), out); err != nil {
		return fmt.Errorf("publish %s: %w", out, err)
	}
	return nil
}

type statusError int

func (e statusError) Error() string { return fmt.Sprintf("HTTP %d", int(e)) }

func (d *downloader) retry(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= d.retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = fn(); err == nil {
			return nil
		}
		var status statusError
		var fileError *os.PathError
		if errors.As(err, &fileError) || (errors.As(err, &status) && status != 408 && status != 429 && status < 500) {
			return err
		}
		if attempt == d.retries {
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
	req.Header.Set("User-Agent", d.userAgent)
	if d.referer != "" {
		req.Header.Set("Referer", d.referer)
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
		_, err := d.request(ctx, asset{uri: d.url}, io.MultiWriter(f, counter))
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
		attemptCtx, cancel := context.WithTimeout(ctx, d.timeout)
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

func readChoice(ctx context.Context, reader *bufio.Reader) (string, error) {
	type result struct {
		line string
		err  error
	}
	choice := make(chan result, 1)
	// ponytail: one stdin read can remain blocked after cancellation; CLI exit releases it.
	go func() { line, err := reader.ReadString('\n'); choice <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-choice:
		return r.line, r.err
	}
}

func (w *limitedWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errors.New("playlist exceeds 8 MiB")
	}
	n, err := w.w.Write(b)
	w.remaining -= n
	return n, err
}

func (d *downloader) downloadAsset(ctx context.Context, a asset, dir string) error {
	out := filepath.Join(dir, a.name)
	if info, err := os.Lstat(out); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("cached resource is not a regular file: %s", out)
		}
		if info.Size() > 0 && (!a.key || info.Size() == 16) && (a.length == 0 || info.Size() == a.length) {
			return nil
		}
		if err := os.Remove(out); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return d.retry(ctx, func() error {
		attemptCtx, cancel := context.WithTimeout(ctx, d.timeout)
		defer cancel()
		return publish(out, func(f *os.File) error {
			_, err := d.request(attemptCtx, a, f)
			return err
		})
	})
}

func (d *downloader) downloadAssets(ctx context.Context, assets []asset, dir string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan asset)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	done := 0
	for i := 0; i < min(d.concurrency, len(assets)); i++ {
		wg.Go(func() {
			for a := range jobs {
				err := d.downloadAsset(ctx, a, dir)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("resource %s: %w", a.name, err)
						cancel()
					}
				} else {
					done++
					fmt.Fprintf(d.progress, "\rResources: %d/%d", done, len(assets))
				}
				mu.Unlock()
			}
		})
	}
loop:
	for _, a := range assets {
		select {
		case <-ctx.Done():
			break loop
		case jobs <- a:
		}
	}
	close(jobs)
	wg.Wait()
	fmt.Fprintln(d.progress)
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (d *downloader) downloadHLS(ctx context.Context, input io.Reader, stderr io.Writer) error {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("ffmpeg is required for HLS; install it and add it to PATH")
	}
	uri := d.url
	var body string
	var base *url.URL
	reader := bufio.NewReader(input)
	for depth := 0; ; depth++ {
		if depth >= 8 {
			return errors.New("playlist nesting exceeds 8 levels")
		}
		body, base, err = d.fetchPlaylist(ctx, uri)
		if err != nil {
			return fmt.Errorf("fetch playlist: %w", err)
		}
		lines, err := playlistLines(body)
		if err != nil {
			return err
		}
		variants, err := parseVariants(lines, base)
		if err != nil {
			return err
		}
		if len(variants) == 0 {
			break
		}
		idx := 0
		if d.auto {
			for i, v := range variants {
				if v.bandwidth > variants[idx].bandwidth {
					idx = i
				}
			}
		} else {
			for i, v := range variants {
				fmt.Fprintf(d.progress, "[%d] BANDWIDTH=%d\n", i, v.bandwidth)
			}
			fmt.Fprint(d.progress, "Choose variant index (or use --auto): ")
			line, readErr := readChoice(ctx, reader)
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			idx, err = strconv.Atoi(strings.TrimSpace(line))
			if err != nil || idx < 0 || idx >= len(variants) {
				return errors.New("invalid variant index")
			}
		}
		if variants[idx].externalAudio {
			return errors.New("separate audio renditions are unsupported; select a muxed variant")
		}
		uri = variants[idx].uri
	}
	lines, err := playlistLines(body)
	if err != nil {
		return err
	}
	p, err := localPlaylist(lines, base)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(base.String() + "\n" + body + "\n" + d.userAgent + "\n" + d.referer))
	dir, err := filepath.Abs(filepath.Join(d.temp, fmt.Sprintf("%x", digest)))
	if err != nil {
		return err
	}
	out, err := filepath.Abs(d.out)
	if err != nil {
		return err
	}
	if rel, err := filepath.Rel(dir, out); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("output cannot be inside the job cache directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock := filepath.Join(dir, ".lock")
	if err := os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("job cache locked: %s (if no job is running, remove its stale .lock directory)", dir)
	}
	defer os.Remove(lock)
	fmt.Fprintf(d.progress, "Found %d segments; cache: %s\n", p.segments, dir)
	if err := d.downloadAssets(ctx, p.assets, dir); err != nil {
		return fmt.Errorf("%w; resume data kept at %s", err, dir)
	}
	manifest := filepath.Join(dir, "local.m3u8")
	if err := os.WriteFile(manifest, []byte(p.text), 0600); err != nil {
		return err
	}
	err = publish(out, func(f *os.File) error {
		// All network resources have been validated and saved. FFmpeg only reads this job's
		// local playlist, handles AES sequence IVs/key rotation and fMP4, then remuxes to MP4.
		cmd := exec.CommandContext(ctx, ffmpeg, "-nostdin", "-hide_banner", "-loglevel", "warning", "-xerror", "-y",
			"-protocol_whitelist", "file,crypto", "-allowed_extensions", "ALL", "-allowed_segment_extensions", "ALL", "-extension_picky", "0",
			"-i", manifest, "-map", "0:v?", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", f.Name())
		cmd.Stderr = stderr
		if err := cmd.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("ffmpeg conversion failed: %w", err)
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return errors.New("ffmpeg produced an empty output")
		}
		return ctx.Err()
	})
	if err != nil {
		return fmt.Errorf("%w; resume data kept at %s", err, dir)
	}
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintf(stderr, "Downloaded successfully; cache cleanup failed: %v\n", err)
	}
	return nil
}
