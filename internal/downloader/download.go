package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	PromptOverwrite                    bool
	overwrite                          string
	CookieJar                          http.CookieJar
	ChooseVariant                      func(context.Context, []int64) (int, error)
}

// Download validates options and publishes a complete raw file or HLS MP4.
func Download(ctx context.Context, o Options, input io.Reader, output, stderr io.Writer) error {
	u, err := parseHTTPURL(o.URL)
	if err != nil {
		return err
	}
	if err := validateOptions(o); err != nil {
		return err
	}
	if input != nil {
		input = bufio.NewReader(input)
	}
	if o.YouTube || (o.Detect && isYouTubeURL(u)) {
		return downloadYouTube(ctx, o, input, output, stderr)
	}
	if !o.Raw && o.Temp == "" {
		return errors.New("temporary directory cannot be empty")
	}
	if !o.Raw && !o.Detect {
		o.Out = strings.TrimSuffix(o.Out, filepath.Ext(o.Out)) + ".mp4"
	}
	if err := o.prepareOutput(ctx, input, output); err != nil {
		return err
	}
	d := newDownloader(o, output)
	defer d.client.CloseIdleConnections()
	start := time.Now()
	if o.Raw || o.Detect {
		err := publish(o.Out, o.overwrite == o.Out, func(f *os.File) error { return d.fetchRaw(ctx, f) })
		if errors.Is(err, errHLS) {
			d.Detect = false
			d.Out = strings.TrimSuffix(d.Out, filepath.Ext(d.Out)) + ".mp4"
			if err := d.Options.prepareOutput(ctx, input, output); err != nil {
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
		return fmt.Errorf("output already exists: %s: %w", out, os.ErrExist)
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

func (o *Options) prepareOutput(ctx context.Context, input io.Reader, output io.Writer) error {
	err := checkOutput(o.Out)
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	if !o.PromptOverwrite || input == nil {
		return err
	}
	info, statErr := os.Lstat(o.Out)
	if statErr != nil {
		return statErr
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("cannot overwrite a directory or symlink: %s", o.Out)
	}
	if o.overwrite == o.Out {
		return nil
	}
	fmt.Fprintf(output, "Output already exists: %s. Overwrite? [y/N]: ", o.Out)
	reader, ok := input.(*bufio.Reader)
	if !ok {
		reader = bufio.NewReader(input)
	}
	answer, readErr := readChoice(ctx, reader)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return readErr
	}
	if answer = strings.ToLower(strings.TrimSpace(answer)); answer != "y" && answer != "yes" {
		return errors.New("overwrite declined; existing file kept")
	}
	o.overwrite = o.Out
	return nil
}

func validateOptions(o Options) error {
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
	return nil
}
