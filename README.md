# JavIt Downloader

Download YouTube videos, HLS (`.m3u8`) streams, and direct files through a desktop app or the terminal. For videos embedded in websites, use browser extraction to find and select the media to download.

## Features

- Desktop interface with download progress, quality selection, and cancellation.
- YouTube and HLS downloads saved as MP4; direct files saved unchanged.
- Browser extraction with saved login sessions per website.
- Resume interrupted HLS downloads when the source and settings remain unchanged.
- Existing files are never overwritten.

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
./javit_dl gui
```

Re-run `./build.sh` after changing the source. Node.js is only needed to build the app.

For a terminal-only build, use `go build -trimpath -o javit_dl .` instead.

## Desktop usage

1. Paste a YouTube, HLS, or direct-file URL. For a webpage, choose **Extract from website**.
2. Choose where to save the file with **Browse…**.
3. Click **Start download**. For extraction, play the video in the browser, then select the captured media in the app.

HLS quality is selected automatically by default; uncheck the option to choose manually. Use **More settings** for additional options or **Cancel download** to stop. The app runs one download at a time.

## Terminal usage

| Command | Purpose |
| --- | --- |
| `./javit_dl gui` | Open the desktop app; build with `./build.sh` first |
| `./javit_dl <URL>` | Automatically detect YouTube, HLS, or a direct file |
| `./javit_dl youtube <URL>` | Download one YouTube video as MP4 |
| `./javit_dl hls <URL>` | Download a finished HLS stream as MP4 |
| `./javit_dl file <URL>` | Save a direct file unchanged |
| `./javit_dl extract <URL>` | Open a webpage in Chrome and choose captured media |
| `./javit_dl --help` | Show available commands and options |

Replace the example URLs with your own links.

```sh
# Open the desktop app
./javit_dl gui

# Automatically detect the download type
./javit_dl 'https://youtu.be/jNQXAC9IVRw' -o youtube.mp4

# Explicit YouTube download
./javit_dl youtube 'https://youtu.be/jNQXAC9IVRw' -o youtube.mp4

# HLS stream: automatically select the highest-bandwidth quality
./javit_dl hls 'https://example.com/master.m3u8' -o movie.mp4 --auto

# Direct file
./javit_dl file 'https://example.com/video.mp4' -o video.mp4

# Webpage: play the video in Chrome, then enter a media number in the terminal
./javit_dl extract 'https://example.com/watch' -o movie.mp4 --auto
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
| `--headless` | Extract without a browser window; media selection still happens in the terminal |
| `--capture-duration <duration>` | Background capture time; defaults to `5s` |
| `--play-selector <selector>` | Website play button to click during background capture |

Options apply to the relevant download mode; `gui` takes no options. Put command options after the command name. `--auto` selects HLS quality; extracted media must still be selected manually. Omit `--auto` to choose HLS quality yourself.

Add `--help` to any command, for example `./javit_dl youtube --help` or `./javit_dl extract --help`, for its available options. Press **Ctrl+C** to cancel.

## Things to know

- **Tools:** HLS requires FFmpeg. YouTube requires FFmpeg, yt-dlp, and a supported JavaScript runtime such as Deno. Extraction requires Chrome or Chromium. Direct files need none of these tools.
- **Output:** Choose an unused filename in an existing folder. HLS and YouTube output use the `.mp4` extension.
- **Resume:** Retry HLS with the same URL, settings, and resume folder. Changed playlists or expiring links may require a fresh download. Direct files and YouTube downloads restart from the beginning.
- **Supported sources:** HLS streams must be finished. Live HLS, DASH, DRM, and HLS with separate audio tracks are unsupported. YouTube downloads cover one video at a time, with H.264 video and AAC audio available; playlists and channels are unsupported.
- **Website access:** Login or verification may need to be completed in the extraction browser. Saved sessions are separate from your regular Chrome profile. Some sites still block downloads; extraction does not bypass access restrictions or supply login cookies to YouTube downloads.
- **Platforms:** Native execution is verified on macOS only. Windows and Linux builds require the dependencies described in the [Wails installation guide](https://wails.io/docs/gettingstarted/installation/).

## Development checks

```sh
go test -race ./...
go vet ./...
```

Install FFmpeg and FFprobe to run the media integration checks; those checks are skipped if the tools are missing.
