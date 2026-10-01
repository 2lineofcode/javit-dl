package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func isYouTubeURL(u *url.URL) bool {
	switch strings.ToLower(u.Hostname()) {
	case "youtube.com", "www.youtube.com", "m.youtube.com", "music.youtube.com", "youtu.be", "www.youtu.be", "youtube-nocookie.com", "www.youtube-nocookie.com":
		return true
	}
	return false
}

func downloadYouTube(ctx context.Context, o Options, output, stderr io.Writer) error {
	u, err := parseHTTPURL(o.URL)
	if err != nil {
		return err
	}
	if !isYouTubeURL(u) {
		return errors.New("youtube requires a YouTube video URL")
	}
	// A single output path represents one video; reject playlist/channel URLs.
	video := u.Path == "/watch" && u.Query().Get("v") != ""
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	video = video || (len(parts) == 2 && (parts[0] == "shorts" || parts[0] == "live" || parts[0] == "embed") && parts[1] != "")
	if strings.HasSuffix(strings.ToLower(u.Hostname()), "youtu.be") {
		video = len(parts) == 1 && parts[0] != ""
	}
	if !video {
		return errors.New("provide a single YouTube video URL; playlists and channels are unsupported")
	}
	o.Out = strings.TrimSuffix(o.Out, filepath.Ext(o.Out)) + ".mp4"
	if err := checkOutput(o.Out); err != nil {
		return err
	}
	ytdlp, err := exec.LookPath("yt-dlp")
	if err != nil {
		return errors.New("YouTube downloads require yt-dlp in PATH (macOS: brew install yt-dlp)")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return errors.New("YouTube downloads require ffmpeg in PATH (macOS: brew install ffmpeg)")
	}
	dir, err := os.MkdirTemp(filepath.Dir(o.Out), ".javit-youtube-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	args := []string{
		"--ignore-config", "--no-plugin-dirs", "--no-playlist", "--abort-on-error", "--abort-on-unavailable-fragments", "--no-overwrites", "--newline",
		"--format", "bv[vcodec^=avc1]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a]", "--merge-output-format", "mp4", "--remux-video", "mp4",
		"--output", "video.%(ext)s", "--ffmpeg-location", ffmpeg,
		"--retries", strconv.Itoa(o.Retries), "--fragment-retries", strconv.Itoa(o.Retries), "--extractor-retries", strconv.Itoa(o.Retries),
		"--socket-timeout", strconv.FormatFloat(o.Timeout.Seconds(), 'f', -1, 64),
	}
	if o.UserAgent != "" && o.UserAgent != "JavIt-DL/2.0" {
		args = append(args, "--add-headers", "User-Agent:"+o.UserAgent)
	}
	if o.Referer != "" {
		args = append(args, "--add-headers", "Referer:"+o.Referer)
	}
	args = append(args, "--", o.URL)
	cmd := exec.CommandContext(ctx, ytdlp, args...)
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, output, stderr
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 5 * time.Second
	start := time.Now()
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("YouTube download failed: %w (see yt-dlp diagnostics above)", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	media := filepath.Join(dir, "video.mp4")
	info, err := os.Lstat(media)
	if err != nil {
		return fmt.Errorf("yt-dlp output: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return errors.New("yt-dlp did not produce a nonempty MP4 file")
	}
	if err := os.Chmod(media, 0600); err != nil {
		return err
	}
	f, err := os.OpenFile(media, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := publishFile(f, o.Out); err != nil {
		return err
	}
	fmt.Fprintf(output, "Downloaded %s (%s)\n", o.Out, time.Since(start).Round(time.Millisecond))
	return nil
}
