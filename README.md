# kuraku-downloader

The application is **kuraku-downloader**; its terminal command is **`kuraku`**.

Download YouTube videos, HLS (`.m3u8`) streams, and direct files through a desktop app or the terminal. For videos embedded in websites, use browser extraction to find and select the media to download.

## Features

- Desktop interface with download progress, quality selection, and cancellation.
- YouTube and HLS downloads saved as MP4; direct files saved unchanged.
- Browser extraction with saved login sessions per website.
- Extracted videos use the page title as the filename by default. In the GUI, disable **Use page title as filename** or enter a custom name; in the CLI, pass `-o` to keep your chosen name. Existing names receive a numbered suffix.
- Resume interrupted HLS downloads when the source and settings remain unchanged.
- The terminal asks before overwriting an existing file; replacement happens only after a successful download. The desktop app preserves existing files.

## Setup (macOS)

Building requires **Go 1.27.1+**, **Node.js/npm**, and **Xcode Command Line Tools**. Install the download tools below; Chrome is only needed for browser extraction.

```sh
brew install go node ffmpeg yt-dlp deno
# Skip if Chrome or Chromium is already installed.
brew install --cask google-chrome
# Skip if Xcode Command Line Tools are already installed.
xcode-select --install
```

From the project folder, build and launch the desktop app:

```sh
./build.sh
./kuraku gui
```

Re-run `./build.sh` after changing the source. Node.js is only needed to build the app.

To build and install into `GOBIN` (or the first `GOPATH` entry's `bin` folder):

```sh
./install.sh
kuraku gui
```

Keep that folder in your `PATH`. Plain `go install` rebuilds without the desktop build tag.

Upgrading from JavIt-DL: run `./install.sh`, then update scripts or aliases that invoke `javit_dl` to use `kuraku`. Existing browser sessions and GUI download caches keep using their legacy `JavIt-DL` folders when present; fresh installations use `kuraku-downloader` under the system config/cache folders. If both folders exist, `kuraku-downloader` takes precedence. Existing files and old binaries are not removed.

The default HTTP User-Agent is now `kuraku-downloader/2.0`. To resume an old HLS job that used the previous default, add `--user-agent 'JavIt-DL/2.0'` and keep its original URL, cache folder, and other settings (or enter that User-Agent in the GUI). Browser extraction still uses Chrome's User-Agent.

For a terminal-only build, use `go build -trimpath -o kuraku .` instead.

## Setup (Linux)

Building requires **Go 1.27.1+** and **Node.js 20.19+ or 22.12+** (Node.js 24 is used in CI). For the desktop app, install GTK3 and WebKitGTK development libraries. On Ubuntu 22.04/24.04 or Debian with WebKitGTK 4.1:

```sh
sudo apt update
sudo apt install build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev ffmpeg
./build.sh
./kuraku gui
```

`build.sh` detects WebKitGTK 4.1 and adds Wails' `webkit2_41` build tag automatically; WebKitGTK 4.0 is also supported when 4.1 is unavailable. `./install.sh` uses the same build. Other distributions need the equivalent GTK3/WebKitGTK packages; see the [Wails installation guide](https://wails.io/docs/gettingstarted/installation/).

Install Chrome/Chromium for browser extraction, and yt-dlp plus a supported JavaScript runtime such as Deno for YouTube downloads. Keep these executables in `PATH`, or pass `--browser` for Chrome/Chromium. The desktop app and visible extraction browser require a graphical session. A terminal-only build does not require GTK/WebKitGTK:

```sh
go build -trimpath -o kuraku .
./kuraku --help
```

## Desktop usage

1. Paste a YouTube, HLS, or direct-file URL. For a webpage, choose **Extract from website**.
2. Choose where to save the file with **Browse…**.
3. Click **Start download**. For extraction, play the video in the browser, then select the captured media in the app.

HLS quality is selected automatically by default; uncheck the option to choose manually. Use **More settings** for additional options or **Cancel download** to stop. The app runs one download at a time.

Prefill the desktop form from the terminal:

```sh
./kuraku gui -u 'http://domain.zxc' -o movie.mp4 --auto
./kuraku gui -u 'https://example.com/video.mp4' -o movie.mp4 --startdownload
./kuraku gui -u 'https://example.com/watch' --mode extract --headless --capture-duration 20s
```

GUI parameters fill the form. Add `--startdownload` to submit the form once when it is ready, using the same validation and action as **Start download**; this flag requires `-u`/`--url`. Without it, start manually. Omitted options retain desktop defaults, including the Downloads output path and HLS cache folder. Use `--mode auto|hls|file|youtube|extract` to choose a mode; extraction options select `extract` when `--mode` is omitted. Use `--auto=false` for manual HLS quality. GUI timeout/capture durations must be whole seconds (for example `30s` or `1m`).

## Terminal usage

| Command | Purpose |
| --- | --- |
| `./kuraku gui` | Open the desktop app; build with `./build.sh` first |
| `./kuraku <URL>` | Automatically detect YouTube, HLS, or a direct file |
| `./kuraku youtube <URL>` | Download one YouTube video as MP4 |
| `./kuraku hls <URL>` | Download a finished HLS stream as MP4 |
| `./kuraku file <URL>` | Save a direct file unchanged |
| `./kuraku extract <URL>` | Open a webpage in Chrome and choose captured media |
| `./kuraku --help` | Show available commands and options |

Replace the example URLs with your own links.

```sh
# Open the desktop app
./kuraku gui

# Automatically detect the download type
./kuraku 'https://youtu.be/jNQXAC9IVRw' -o youtube.mp4

# Explicit YouTube download
./kuraku youtube 'https://youtu.be/jNQXAC9IVRw' -o youtube.mp4

# HLS stream: automatically select the highest-bandwidth quality
./kuraku hls 'https://example.com/master.m3u8' -o movie.mp4 --auto

# Direct file
./kuraku file 'https://example.com/video.mp4' -o video.mp4

# Webpage: play the video in Chrome, then enter a media number in the terminal
./kuraku extract 'https://example.com/watch' -o movie.mp4 --auto
```

Passing a URL without a command automatically detects YouTube, HLS, or a direct file. For webpages, use `extract`; automatic mode may otherwise save the page's HTML.

| Option | Purpose |
| --- | --- |
| `-o <path>` | Output file; defaults to `output.mp4` |
| `--auto` | Automatically select the highest-bandwidth HLS quality |
| `-c <number>` | Parallel HLS downloads; defaults to `8` |
| `-r <number>` | Retries after failure; defaults to `3` |
| `--timeout <duration>` | Request timeout, for example `60s` |
| `--temp <folder>` | HLS resume folder; defaults to `m3u8_tmp` in the CLI |
| `--user-agent <value>` | Custom browser identification for requests |
| `--referer <URL>` | Source page address required by some websites |
| `--browser <path>` | Chrome/Chromium executable for extraction; auto-detected by default |
| `--allow-popups` | Allow popups and new tabs needed for login or external players; extraction closes all extra tabs by default |
| `--headless` | Extract without a browser window; media selection still happens in the terminal |
| `--capture-duration <duration>` | Background capture time; defaults to `5s` |
| `--play-selector <selector>` | Website play button to click during background capture |

Options apply to the relevant download mode. `gui` also accepts these options to prefill its form, plus `-u`/`--url` and `--mode`. Put command options after the command name. `--auto` selects HLS quality; extracted media must still be selected manually. Omit `--auto` to choose HLS quality yourself.

Add `--help` to any command, for example `./kuraku youtube --help` or `./kuraku extract --help`, for its available options. Press **Ctrl+C** to cancel.

## Things to know

- **Tools:** HLS requires FFmpeg. YouTube requires FFmpeg, yt-dlp, and a supported JavaScript runtime such as Deno. Extraction requires Chrome or Chromium. Direct files need none of these tools.
- **Output:** Choose an unused filename in an existing folder. HLS and YouTube output use the `.mp4` extension.
- **Resume:** Retry HLS with the same URL, settings, and resume folder. Changed playlists or expiring links may require a fresh download. Direct files and YouTube downloads restart from the beginning.
- **Supported sources:** HLS streams must be finished. Live HLS, DASH, DRM, and HLS with separate audio tracks are unsupported. YouTube downloads cover one video at a time, with H.264 video and AAC audio available; playlists and channels are unsupported.
- **Website access:** Login or verification may need to be completed in the extraction browser. Saved sessions are separate from your regular Chrome profile. Some sites still block downloads; extraction does not bypass access restrictions or supply login cookies to YouTube downloads.
- **Platforms:** CI checks macOS and Ubuntu 22.04/24.04, including desktop builds and HLS media integration tests. Windows execution is not verified.

## Development checks

```sh
go test -race ./...
go vet ./...
```

Install FFmpeg and FFprobe to run the media integration checks; those checks are skipped if the tools are missing.
