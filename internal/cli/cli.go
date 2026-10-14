package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"kuraku-downloader/internal/downloader"

	urfave "github.com/urfave/cli/v3"
)

const usageExamples = `Download from a URL. YouTube links use yt-dlp; HLS playlists are detected
from the response body. Other responses are saved unchanged. Replace the example URLs with your own links.

Examples:
  Download a playlist or file automatically:
    ./kuraku 'https://example.com/video' -o movie.mp4 --auto

  Save to a custom path (the folder must already exist):
    ./kuraku 'https://example.com/video' -o '/home/xxx/movie.mp4' --auto

  Download a direct file:
    ./kuraku 'https://example.com/archive.zip' -o archive.zip

  Download a YouTube video automatically:
    ./kuraku 'https://www.youtube.com/watch?v=jNQXAC9IVRw' -o youtube.mp4

To discover media interactively, use extract <website-URL>.
To choose a type manually, use hls <URL>, file <URL> or youtube <URL>.
YouTube requires yt-dlp and FFmpeg; HLS requires FFmpeg. Existing files require confirmation before replacement.`

const hlsExamples = `Download a finished HLS playlist as MP4. FFmpeg must be in PATH.
Replace the example URLs with your own links.

Examples:
  Automatically choose the highest-bandwidth variant:
    ./kuraku hls 'https://example.com/master.m3u8' -o movie.mp4 --auto

  Choose a variant interactively:
    ./kuraku hls 'https://example.com/master.m3u8' -o movie.mp4

  Save to a custom path (the folder must already exist):
    ./kuraku hls 'https://example.com/master.m3u8' -o '/home/xxx/movie.mp4' --auto

  Resume a failed HLS download by repeating the same URL and options:
    ./kuraku hls 'https://example.com/master.m3u8' -o movie.mp4 --auto -c 8 -r 3 --temp m3u8_tmp

  Send headers required by the source website:
    ./kuraku hls 'https://example.com/master.m3u8' -o movie.mp4 --auto --user-agent 'Mozilla/5.0' --referer 'https://example.com/'

Existing files require confirmation before replacement. Press Ctrl+C to cancel.
Completed HLS resources are kept for resume.`

const fileExamples = `Download one HTTP(S) file unchanged. FFmpeg is not required.
Replace the example URLs with your own links.

Examples:
  Download a video file:
    ./kuraku file 'https://example.com/video.mp4' -o video.mp4

  Save any file to a custom path (the folder must already exist):
    ./kuraku file 'https://example.com/archive.zip' -o '/home/xxx/archive.zip'

  Send headers required by the source website:
    ./kuraku file 'https://example.com/video.mp4' -o video.mp4 --user-agent 'Mozilla/5.0' --referer 'https://example.com/'

Existing files require confirmation before replacement. Press Ctrl+C to cancel.
Incomplete files are removed; file downloads restart from the beginning.`

const youtubeExamples = `Download one YouTube video as H.264 + AAC MP4 using yt-dlp and FFmpeg.
Both tools must be installed in PATH. A supported JavaScript runtime such as Deno
is recommended by yt-dlp (macOS: brew install yt-dlp ffmpeg deno).

Examples:
  Download a YouTube video (also detected automatically without this command):
    ./kuraku youtube 'https://www.youtube.com/watch?v=jNQXAC9IVRw' -o youtube.mp4

  Use a short link and custom output path:
    ./kuraku youtube 'https://youtu.be/jNQXAC9IVRw' -o '/home/xxx/youtube.mp4'

Watch, Shorts, live video links and embedded video URLs are supported.
H.264/AAC must be available; other codecs are not selected.
Playlists and channels are unsupported. Existing files require confirmation before replacement.
Press Ctrl+C to cancel; temporary YouTube files are removed on failure.`

const extractExamples = `Capture media URLs in Chrome/Chromium, visibly or with --headless.
Navigate or play a video, then enter a listed media number in the terminal.
HLS playlists and direct media files use the existing downloader. DASH is detected
but cannot be downloaded. Choosing a valid media number closes the browser before
download starts; q also closes the browser.

Examples:
  Open a website and choose a captured playlist:
    ./kuraku extract 'https://example.com/watch' -o movie.mp4 --auto

  Capture in the background for 20 seconds, then choose a URL:
    ./kuraku extract 'https://example.com/watch' --headless --capture-duration 20s -o movie.mp4 --auto

  Click a custom player button during headless capture:
    ./kuraku extract 'https://example.com/watch' --headless --play-selector '.play-button' -o movie.mp4 --auto

  Use a specific browser executable:
    ./kuraku extract 'https://example.com/watch' -o movie.mp4 --browser '/path/to/chrome'

Chrome/Chromium must be installed; HLS requires FFmpeg.
Session cookies and the browser User-Agent are reused for HTTP downloads.
Headless mode tries muted video/audio playback unless --play-selector is supplied.
Headless capture lasts 5 seconds by default. Capture/download failures retry once
in a visible browser; play the video and choose a fresh media number.
Browser sessions are saved automatically per site.
Cloudflare verification in headless mode opens a visible browser for you to complete.
Existing files require confirmation before replacement. Press Ctrl+C to cancel.`

// Run parses CLI arguments excluding the program name and executes the command.
func Run(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer) error {
	return RunWithGUI(ctx, args, input, output, stderr, nil)
}

// RunWithGUI adds the desktop launcher without coupling the CLI to Wails.
func RunWithGUI(ctx context.Context, args []string, input io.Reader, output, stderr io.Writer, launch func(context.Context, map[string]any) error) error {
	o := downloader.Options{Concurrency: 8}
	var extraction downloader.ExtractOptions
	action := func(ctx context.Context, cmd *urfave.Command) error {
		if cmd.NArg() != 1 {
			return fmt.Errorf("provide exactly one HTTP(S) URL; see kuraku --help")
		}
		o.URL = cmd.Args().First()
		o.PromptOverwrite = true
		if cmd.Name == "extract" {
			extraction.AutoFilename = !cmd.IsSet("out")
			if !cmd.IsSet("user-agent") {
				o.UserAgent = ""
			}
			return downloader.Extract(ctx, o, extraction, input, output, stderr)
		}
		o.Raw = cmd.Name == "file"
		o.YouTube = cmd.Name == "youtube"
		o.Detect = cmd.Name == "kuraku"
		return downloader.Download(ctx, o, input, output, stderr)
	}
	commonFlags := []urfave.Flag{
		&urfave.StringFlag{Name: "out", Aliases: []string{"o"}, Value: "output.mp4", Local: true, Destination: &o.Out, Usage: "Output file path; parent folder must exist"},
		&urfave.IntFlag{Name: "retries", Aliases: []string{"r"}, Value: 3, Local: true, Destination: &o.Retries, Usage: "Retries after the first attempt (0-10)"},
		&urfave.DurationFlag{Name: "timeout", Value: 30 * time.Second, Local: true, Destination: &o.Timeout, Usage: "HLS request / YouTube socket timeout (file: response headers only)"},
		&urfave.StringFlag{Name: "user-agent", Value: "kuraku-downloader/2.0", Local: true, Destination: &o.UserAgent, Usage: "HTTP User-Agent"},
		&urfave.StringFlag{Name: "referer", Local: true, Destination: &o.Referer, Usage: "Optional HTTP Referer"},
	}
	hlsFlags := []urfave.Flag{
		&urfave.IntFlag{Name: "concurrency", Aliases: []string{"c"}, Value: 8, Local: true, Destination: &o.Concurrency, Usage: "Parallel HLS downloads (1-64)"},
		&urfave.StringFlag{Name: "temp", Aliases: []string{"tf"}, Value: "m3u8_tmp", Local: true, Destination: &o.Temp, Usage: "HLS resume directory; only job subdirectories are cleaned"},
		&urfave.BoolFlag{Name: "auto", Local: true, Destination: &o.Auto, Usage: "Select highest-bandwidth variant without prompting"},
	}
	extractFlags := append(append(commonFlags, hlsFlags...), &urfave.StringFlag{Name: "browser", Local: true, Destination: &extraction.Browser, Usage: "Chrome/Chromium executable path (auto-detected by default)"},
		&urfave.BoolFlag{Name: "allow-popups", Local: true, Destination: &extraction.AllowPopups, Usage: "Allow popups and new tabs for login or players (closed by default)"},
		&urfave.BoolFlag{Name: "headless", Local: true, Destination: &extraction.Headless, Usage: "Capture media without a browser window, then choose in the terminal"},
		&urfave.DurationFlag{Name: "capture-duration", Value: 5 * time.Second, Local: true, Destination: &extraction.CaptureDuration, Usage: "Headless capture time after navigation (must be positive)"},
		&urfave.StringFlag{Name: "play-selector", Local: true, Destination: &extraction.PlaySelector, Usage: "CSS selector to click once during headless capture; default tries native video/audio playback"})

	guiFlags := append([]urfave.Flag{}, extractFlags...)
	for i, flag := range guiFlags {
		switch flag := flag.(type) {
		case *urfave.StringFlag:
			copy := *flag
			switch copy.Name {
			case "out":
				copy.DefaultText = "Downloads/output.mp4"
			case "temp":
				copy.DefaultText = "system user-cache folder"
			case "user-agent":
				copy.DefaultText = "default for selected mode"
			}
			guiFlags[i] = &copy
		case *urfave.BoolFlag:
			copy := *flag
			if copy.Name == "auto" {
				copy.Value = true
				copy.Usage = "Select highest-bandwidth HLS quality; use --auto=false to choose manually"
			}
			if copy.Name == "headless" {
				copy.Usage = "Capture media in the background, then choose in the desktop app"
			}
			guiFlags[i] = &copy
		}
	}
	cmd := &urfave.Command{
		Name:        "kuraku",
		Usage:       "Download a URL, automatically detecting YouTube, HLS or files",
		ArgsUsage:   "<URL>",
		Flags:       append(commonFlags, hlsFlags...),
		Description: usageExamples,
		Reader:      input,
		Writer:      output,
		ErrWriter:   stderr,
		Action: func(ctx context.Context, cmd *urfave.Command) error {
			if len(args) == 0 {
				return urfave.ShowAppHelp(cmd)
			}
			return action(ctx, cmd)
		},
		Commands: []*urfave.Command{
			{Name: "gui", Usage: "Open the desktop interface with optional form values", ArgsUsage: " ",
				Description: "Options prefill the form; use --startdownload to begin automatically.\n\nExamples:\n  ./kuraku gui -u https://example.com/video -o movie.mp4 --auto\n  ./kuraku gui -u https://example.com/watch --mode extract --headless\n  ./kuraku gui --help",
				Flags: append(guiFlags,
					&urfave.StringFlag{Name: "url", Aliases: []string{"u"}, Local: true, Usage: "URL to prefill in the desktop form"},
					&urfave.StringFlag{Name: "mode", Value: "auto", Local: true, Usage: "Download mode: auto, hls, file, youtube, extract"},
					&urfave.BoolFlag{Name: "startdownload", Local: true, Usage: "Start downloading when the desktop form is ready; requires --url"}),
				Action: func(ctx context.Context, cmd *urfave.Command) error {
					if cmd.NArg() != 0 {
						return fmt.Errorf("use gui -u <URL>; positional arguments are not accepted")
					}
					initial, err := guiValues(cmd)
					if err != nil {
						return err
					}
					if launch == nil {
						return fmt.Errorf("GUI launcher is unavailable")
					}
					return launch(ctx, initial)
				}},
			{Name: "extract", Usage: "Open a browser and continuously capture media URLs", ArgsUsage: "<URL>", Description: extractExamples, Action: action, Flags: extractFlags},
			{
				Name: "hls", Usage: "Download an HLS playlist as MP4", ArgsUsage: "<URL>", Description: hlsExamples, Action: action,
				Flags: append(commonFlags, hlsFlags...),
			},
			{Name: "youtube", Usage: "Download a YouTube video as MP4", ArgsUsage: "<URL>", Description: youtubeExamples, Flags: commonFlags, Action: action},
			{Name: "file", Usage: "Download a direct file unchanged", ArgsUsage: "<URL>", Description: fileExamples, Flags: commonFlags, Action: action},
		},
	}
	return cmd.Run(ctx, append([]string{"kuraku"}, args...))
}

// guiValues forwards only explicit flags, preserving the desktop defaults.
func guiValues(cmd *urfave.Command) (map[string]any, error) {
	initial := map[string]any{}
	if cmd.Bool("startdownload") && strings.TrimSpace(cmd.String("url")) == "" {
		return nil, fmt.Errorf("--startdownload requires -u/--url")
	}
	mode := cmd.String("mode")
	for _, name := range []string{"browser", "headless", "capture-duration", "play-selector", "allow-popups"} {
		if cmd.IsSet(name) {
			if !cmd.IsSet("mode") {
				mode = "extract"
			}
			if mode != "extract" {
				return nil, fmt.Errorf("--%s requires --mode extract", name)
			}
		}
	}
	switch mode {
	case "auto", "hls", "file", "youtube", "extract":
	default:
		return nil, fmt.Errorf("invalid mode %q; use auto, hls, file, youtube or extract", mode)
	}
	if cmd.String("play-selector") != "" && !cmd.Bool("headless") {
		return nil, fmt.Errorf("--play-selector requires --headless")
	}
	if cmd.IsSet("mode") || mode != "auto" {
		initial["mode"] = mode
	}
	for flag, field := range map[string]string{"startdownload": "startDownload", "url": "url", "out": "out", "retries": "retries", "user-agent": "userAgent", "referer": "referer", "concurrency": "concurrency", "temp": "temp", "auto": "auto", "browser": "browser", "headless": "headless", "play-selector": "playSelector", "allow-popups": "allowPopups"} {
		if cmd.IsSet(flag) {
			initial[field] = cmd.Value(flag)
		}
	}
	for flag, field := range map[string]string{"timeout": "timeout", "capture-duration": "captureDuration"} {
		if cmd.IsSet(flag) {
			duration := cmd.Duration(flag)
			limit := 86400 * time.Second
			if flag == "capture-duration" {
				limit = 3600 * time.Second
			}
			if duration < time.Second || duration > limit || duration%time.Second != 0 {
				return nil, fmt.Errorf("--%s must be whole seconds between 1s and %s in GUI mode", flag, limit)
			}
			initial[field] = int(duration / time.Second)
		}
	}
	return initial, nil
}
