package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type options struct {
	url, out, temp, userAgent, referer string
	concurrency, retries               int
	timeout                            time.Duration
	auto, raw                          bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer) error {
	if len(args) > 0 && args[0] == "download" {
		args = args[1:]
	}
	var o options
	fs := flag.NewFlagSet("javit_dl", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.url, "u", "", "HTTP(S) playlist or file URL")
	fs.StringVar(&o.url, "url", "", "Alias for -u")
	fs.StringVar(&o.out, "o", "output.mp4", "Output path (HLS always produces MP4)")
	fs.StringVar(&o.out, "out", "output.mp4", "Alias for -o")
	fs.IntVar(&o.concurrency, "c", 8, "Parallel HLS downloads (1-64)")
	fs.IntVar(&o.concurrency, "concurrency", 8, "Alias for -c")
	fs.IntVar(&o.retries, "r", 3, "Retries after the first attempt (0-10)")
	fs.IntVar(&o.retries, "retries", 3, "Alias for -r")
	fs.StringVar(&o.temp, "tf", "m3u8_tmp", "HLS resume directory; only job subdirectories are cleaned")
	fs.StringVar(&o.temp, "temp", "m3u8_tmp", "Alias for -tf")
	fs.BoolVar(&o.auto, "auto", false, "Select highest-bandwidth variant without prompting")
	fs.BoolVar(&o.raw, "rawurl", false, "Download a single file without conversion")
	fs.DurationVar(&o.timeout, "timeout", 30*time.Second, "Timeout per HLS request (raw: response headers only)")
	fs.StringVar(&o.userAgent, "user-agent", "JavIt-DL/2.0", "HTTP User-Agent")
	fs.StringVar(&o.referer, "referer", "", "Optional HTTP Referer")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "JavIt Downloader — CLI\nUsage: javit_dl [-u URL -o PATH] [options]\n       javit_dl download [options]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if len(args) == 0 {
		fs.Usage()
		return nil
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if _, err := parseHTTPURL(o.url); err != nil {
		return err
	}
	if strings.TrimSpace(o.out) == "" {
		return errors.New("output path cannot be empty")
	}
	if o.concurrency < 1 || o.concurrency > 64 {
		return errors.New("concurrency must be between 1 and 64")
	}
	if o.retries < 0 || o.retries > 10 {
		return errors.New("retries must be between 0 and 10")
	}
	if o.timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	if strings.ContainsAny(o.userAgent+o.referer, "\r\n") {
		return errors.New("HTTP headers cannot contain newlines")
	}
	if !o.raw {
		if o.temp == "" {
			return errors.New("temporary directory cannot be empty")
		}
		ext := filepath.Ext(o.out)
		o.out = strings.TrimSuffix(o.out, ext) + ".mp4"
	}
	if _, err := os.Lstat(o.out); err == nil {
		return fmt.Errorf("output already exists: %s", o.out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent, err := os.Stat(filepath.Dir(o.out))
	if err != nil {
		return fmt.Errorf("output directory: %w", err)
	}
	if !parent.IsDir() {
		return errors.New("output parent must be a directory")
	}
	d := newDownloader(o, output)
	defer d.client.CloseIdleConnections()
	start := time.Now()
	if o.raw {
		err := publish(o.out, func(f *os.File) error { return d.fetchRaw(ctx, f) })
		if err != nil {
			return err
		}
	} else if err := d.downloadHLS(ctx, input, stderr); err != nil {
		return err
	}
	fmt.Fprintf(output, "Downloaded %s (%s)\n", o.out, time.Since(start).Round(time.Millisecond))
	return nil
}
