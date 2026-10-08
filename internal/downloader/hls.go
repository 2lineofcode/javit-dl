package downloader

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

func readChoice(ctx context.Context, reader *bufio.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	type result struct {
		line string
		err  error
	}
	choice := make(chan result, 1)
	// File input polls for cancellation; GUI readers observe the job context.
	go func() { line, err := reader.ReadString('\n'); choice <- result{line, err} }()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-choice:
		return r.line, r.err
	}
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
		attemptCtx, cancel := context.WithTimeout(ctx, d.Timeout)
		defer cancel()
		return publish(out, false, func(f *os.File) error {
			_, err := d.request(attemptCtx, a, f)
			return err
		})
	})
}

func (d *downloader) downloadAssets(ctx context.Context, assets []asset, dir string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan asset)
	d.bytes = startByteProgress(d.progress, len(assets))
	defer func() { d.bytes.stop(); d.bytes = nil }()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for i := 0; i < min(d.Concurrency, len(assets)); i++ {
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
					d.bytes.done.Add(1)
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
	uri := d.URL
	var body string
	var lines []string
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
		lines, err = playlistLines(body)
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
		if d.Auto {
			for i, v := range variants {
				if v.bandwidth > variants[idx].bandwidth {
					idx = i
				}
			}
		} else if d.ChooseVariant != nil {
			bandwidths := make([]int64, len(variants))
			for i, v := range variants {
				bandwidths[i] = v.bandwidth
			}
			idx, err = d.ChooseVariant(ctx, bandwidths)
			if err != nil {
				return err
			}
			if idx < 0 || idx >= len(variants) {
				return errors.New("invalid variant index")
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
	p, err := localPlaylist(lines, base)
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(base.String() + "\n" + body + "\n" + d.UserAgent + "\n" + d.Referer))
	dir, err := filepath.Abs(filepath.Join(d.Temp, fmt.Sprintf("%x", digest)))
	if err != nil {
		return err
	}
	out, err := filepath.Abs(d.Out)
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
	err = publish(out, d.overwrite == out || d.overwrite == d.Out, func(f *os.File) error {
		fmt.Fprintln(d.progress, "Remuxing to MP4...")
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
