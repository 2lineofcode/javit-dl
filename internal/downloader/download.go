package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options configures one download. CLI defaults are supplied by the caller.
type Options struct {
	URL, Out, Temp, UserAgent, Referer string
	Concurrency, Retries               int
	Timeout                            time.Duration
	Auto, Raw, Detect, YouTube         bool
}

// Download validates options and publishes a complete raw file or HLS MP4.
func Download(ctx context.Context, o Options, input io.Reader, output, stderr io.Writer) error {
	u, err := parseHTTPURL(o.URL)
	if err != nil {
		return err
	}
	if strings.TrimSpace(o.Out) == "" {
		return errors.New("output path cannot be empty")
	}
	if o.Concurrency < 1 || o.Concurrency > 64 {
		return errors.New("concurrency must be between 1 and 64")
	}
	if o.Retries < 0 || o.Retries > 10 {
		return errors.New("retries must be between 0 and 10")
	}
	if o.Timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if strings.ContainsAny(o.UserAgent+o.Referer, "\r\n") {
		return errors.New("HTTP headers cannot contain newlines")
	}
	if o.YouTube || (o.Detect && isYouTubeURL(u)) {
		return downloadYouTube(ctx, o, output, stderr)
	}
	if !o.Raw && o.Temp == "" {
		return errors.New("temporary directory cannot be empty")
	}
	if !o.Raw && !o.Detect {
		o.Out = strings.TrimSuffix(o.Out, filepath.Ext(o.Out)) + ".mp4"
	}
	if err := checkOutput(o.Out); err != nil {
		return err
	}
	d := newDownloader(o, output)
	defer d.client.CloseIdleConnections()
	start := time.Now()
	if o.Raw || o.Detect {
		err := publish(o.Out, func(f *os.File) error { return d.fetchRaw(ctx, f) })
		if errors.Is(err, errHLS) {
			d.Detect = false
			d.Out = strings.TrimSuffix(d.Out, filepath.Ext(d.Out)) + ".mp4"
			if err := checkOutput(d.Out); err != nil {
				return err
			}
			if err := d.downloadHLS(ctx, input, stderr); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	} else if err := d.downloadHLS(ctx, input, stderr); err != nil {
		return err
	}
	fmt.Fprintf(output, "Downloaded %s (%s)\n", d.Out, time.Since(start).Round(time.Millisecond))
	return nil
}

func checkOutput(out string) error {
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("output already exists: %s", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := os.Stat(filepath.Dir(out))
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	if !parent.IsDir() {
		return errors.New("output parent must be a directory")
	}
	return nil
}
