# JavIt Downloader

A CLI downloader for YouTube videos, finished HLS playlists and single HTTP(S) files, with interactive browser extraction to discover media URLs. HLS downloads run in parallel, retain completed resources for resume, and produce MP4 through FFmpeg stream copy. The CLI uses [urfave/cli v3](https://cli.urfave.org/v3/); browser extraction uses [chromedp](https://github.com/chromedp/chromedp).

## Requirements

- **Go 1.27.1+** to build, as declared in `go.mod`.
- **FFmpeg** in `PATH` for HLS and YouTube. The local setup uses FFmpeg 9.0.2; older versions are not verified. Direct file downloads do not need FFmpeg.
- **[yt-dlp](https://github.com/yt-dlp/yt-dlp)** in `PATH` for YouTube. Install a supported JavaScript runtime such as **Deno**; see [yt-dlp EJS setup](https://github.com/yt-dlp/yt-dlp/wiki/EJS) for runtime and challenge-solver requirements.
- **Google Chrome or Chromium** installed for `extract`. The browser is auto-detected, or selected with `--browser <executable-path>`. The default mode opens a visible window and needs a desktop session; `--headless` runs the browser without a window.
- The output directory must already exist and support hard links. Final publication uses a hard link to prevent overwriting an existing file, including concurrent writes.

On macOS:

```sh
brew install go ffmpeg yt-dlp deno
# For extract, install Chrome if Chrome/Chromium is not already installed:
brew install --cask google-chrome
go build -trimpath -o javit_dl .
```

After changing source, rebuild before using `./javit_dl`. `go run .` always uses the current source.

## Desktop GUI

The optional `gui` command opens a Wails v2 desktop window with a React interface. All existing CLI commands remain available in the same binary. The GUI supports automatic, HLS, direct-file, YouTube and browser-extraction modes, a native save dialog, HLS quality/media selection, bounded activity logs and cancellation. Closing the window cancels the active job. It runs one download at a time; HLS resume data uses the user-cache directory by default.

Build on macOS with Node.js/npm and Xcode Command Line Tools installed:

```sh
./build.sh
./javit_dl gui
# CLI commands work with the same binary:
./javit_dl --help
```

`build.sh` installs locked frontend dependencies, builds React, and compiles Go with the Wails `production` build tag. Re-run it after changing frontend or Go source. A plain `go build .` produces a CLI build; the GUI requires the production tag. For source execution after building the frontend, use `go run -tags production . gui`. Node.js is only needed when building, not when running the compiled application. FFmpeg, yt-dlp and Chrome requirements still apply to their respective modes; extraction opens a separate Chrome/Chromium browser.

For frontend development, `npm --prefix frontend run dev` provides Vite; Go bindings and downloads require the desktop window. React uses the Wails Go bridge directly, without a UI component library or state-management dependency. The interface polls state every 250 ms during a job and stops polling while idle. Runtime performance has to be measured on the target machine; using a system WebView does not guarantee a fixed memory footprint.

Windows/Linux require the platform dependencies documented in [Wails installation](https://wails.io/docs/gettingstarted/installation/); native execution is verified on macOS only.

## Usage

| Command | Input | Behavior |
| --- | --- | --- |
| `javit_dl gui` | No arguments | Open the desktop interface (build with `./build.sh`) |
| `javit_dl <URL>` | YouTube video, HLS or direct file URL | Route YouTube to yt-dlp; otherwise inspect the response for HLS and save other responses unchanged |
| `javit_dl hls <URL>` | Finished HLS playlist URL | Download resources and remux to MP4 |
| `javit_dl file <URL>` | Direct HTTP(S) file URL | Save bytes unchanged, including playlist text if supplied |
| `javit_dl youtube <URL>` | One YouTube video URL | Download H.264 video/AAC audio through yt-dlp and produce MP4 |
| `javit_dl extract <URL>` | Website URL | Open a browser, continuously capture media requests, then download the URL you select |

Pass a URL directly to route YouTube video links to yt-dlp, detect HLS from the response body and save other responses unchanged. Detection does not depend on the URL extension or Content-Type. Use `youtube <URL>`, `hls <URL>` or `file <URL>` when you want to force a type. Use `extract <website-URL>` to discover media interactively. Passing a normal website URL directly to automatic mode can save HTML; it does not extract video links from the page.

```sh
# YouTube watch links, Shorts, live video links and youtu.be links → MP4
./javit_dl 'https://www.youtube.com/watch?v=jNQXAC9IVRw' -o youtube.mp4

# Explicit YouTube command and custom path
./javit_dl youtube 'https://youtu.be/jNQXAC9IVRw' -o '/home/xxx/youtube.mp4'

# Highest-bandwidth HLS variant → movie.mp4
./javit_dl 'https://example.com/master.m3u8' -o movie.mp4 --auto

# Choose a master-playlist variant interactively
./javit_dl 'https://example.com/master.m3u8' -o movie.mp4

# Download a single file unchanged
./javit_dl 'https://example.com/video.mp4' -o video.mp4

# Save to a custom path; the folder must already exist
./javit_dl 'https://example.com/archive.zip' -o '/home/xxx/archive.zip'

# Discover media while navigating or playing video in a browser
./javit_dl extract 'https://example.com/watch' -o movie.mp4 --auto

# Resume the same failed HLS job with the same options
./javit_dl 'https://example.com/master.m3u8' -o movie.mp4 --auto -c 8 -r 3 --temp m3u8_tmp

# For origins that require these headers
./javit_dl 'https://example.com/video.m3u8' -o movie.mp4 --auto \
  --user-agent 'Mozilla/5.0' --referer 'https://example.com/'

./javit_dl --help
./javit_dl hls --help
./javit_dl file --help
./javit_dl youtube --help
./javit_dl extract --help

# Run from source using the same commands
go run . 'https://example.com/master.m3u8' -o movie.mp4 --auto
```

| Option | Commands | Default | Meaning |
| --- | --- | --- | --- |
| `-o`, `--out` | all | `output.mp4` | HLS and YouTube output is normalized to `.mp4`; file downloads use the exact path |
| `-r`, `--retries` | all | `3` | Extra attempts after failure, from 0 to 10 |
| `--timeout` | all | `30s` | Must be positive. Whole request timeout for HLS resources; socket timeout for YouTube; header/connect timeout for files; initial navigation and new-target attachment timeout in `extract` |
| `--user-agent` | all | HTTP: `JavIt-DL/2.0`; YouTube: yt-dlp default; `extract`: browser default | Custom User-Agent; `extract` applies it to the browser and subsequent HTTP downloads |
| `--referer` | all | Empty; captured request Referer in `extract` | Override the downloader Referer; this does not change browser capture requests |
| `-c`, `--concurrency` | automatic, `hls`, `extract` | `8` | Parallel HLS resource downloads, from 1 to 64 |
| `--tf`, `--temp` | automatic, `hls`, `extract` | `m3u8_tmp` | Parent directory for per-job HLS resume data; `-tf` is also accepted |
| `--auto` | automatic, `hls`, `extract` | false | Choose the highest declared HLS `BANDWIDTH` variant; does not select an extracted media URL |
| `--browser` | `extract` | Auto-detected | Chrome/Chromium executable path |
| `--headless` | `extract` | false | Capture in the background, choose a URL in the terminal; failures retry once in visible mode |
| `--capture-duration` | `extract` | `5s` | Headless capture time after initial navigation; must be positive in headless mode |
| `--play-selector` | `extract` | Empty | CSS selector to click once during headless capture; requires `--headless` |
| `-h`, `--help` | all | — | Show application or command help |

Automatic mode and each command accept exactly one absolute HTTP(S) URL without embedded credentials. Options may appear before or after the URL. For an explicit command, put its options after the command name, for example `javit_dl extract --auto <URL>`. The old `download`, `-u`/`--url` and `--rawurl` syntax has been replaced by a positional URL, with optional `hls`, `file`, `youtube` and `extract` commands. Web mode has been removed.

Automatic file transfers reuse the inspected response, so detection does not issue another file request. For HLS, detection reads a bounded prefix and then fetches the full playlist using the HLS request timeout. Use `hls <URL>` to avoid this initial detection request when a source only allows a single request. Extraction also makes a fresh download request after the browser request; single-use or expired signed URLs can fail.

## Browser extraction

```sh
./javit_dl extract 'https://example.com/watch' -o movie.mp4 --auto
# Chrome/Chromium is auto-detected; override its path if needed:
./javit_dl extract 'https://example.com/watch' --browser '/path/to/chrome' -o movie.mp4
```

1. Run `extract` with a website URL and the desired output path.
2. Navigate, log in if required, and play the video in the separate Chrome/Chromium window. A dedicated per-site profile saves browser sessions automatically and does not reuse your regular browser profile.
3. Watch the terminal for unique media URLs. Numbers are stable and start at **1**, in discovery order; `[HLS]` identifies playlist candidates. Signed query strings remain intact.
4. Enter a media number to close the browser and start downloading using the copied session. `--auto` selects the highest-bandwidth HLS variant only; you always choose the captured URL yourself. Without `--auto`, master playlists prompt for a **0-based** quality index.

At the media-selection prompt, `list` or an empty line shows all results; `q`, `quit` or `exit` closes the session. End-of-input also closes the session. During a download, use Ctrl+C to cancel.

### Headless extraction

```sh
# No browser window; collect URLs for 15 seconds, then choose in the terminal
./javit_dl extract 'https://example.com/watch' --headless -o movie.mp4 --auto

# Give a slower player more time and click its play button
./javit_dl extract 'https://example.com/watch' --headless \
  --capture-duration 30s --play-selector '.play-button' -o movie.mp4 --auto
```

Headless capture starts after initial navigation and runs for `--capture-duration` (default `5s`). Without `--play-selector`, the app attempts muted playback of native video/audio elements immediately and every 500 ms, including accessible iframe documents and watched tabs. With a selector, it waits for that element in the initial page and attempts one click within the capture period; it does not also attempt native autoplay. A missing or invalid selector does not extend the capture deadline.

At the end of the capture period, the result list is frozen and shown in the terminal. You still select a media number; `--auto` only selects HLS quality. Piped input also waits for capture to finish. After you select a valid downloadable media number, the app copies cookies and headers, closes Chrome, and starts the download. Use Ctrl+C to stop during capture or download.

If Cloudflare verification prevents headless capture, the app automatically reopens the website in a visible browser using the same saved profile. Complete verification manually, play the video, and choose a URL in the terminal. The visible browser closes after you select a valid downloadable media number or quit; the app does not solve CAPTCHA or verification automatically. Future runs reuse the saved profile, but the site may require verification again. Headless mode still requires Chrome/Chromium and uses its resources. If headless capture or a selected download fails, the app closes the hidden browser and retries once in visible mode using the same saved profile. Play the video and select a fresh URL in the terminal. Quitting, end-of-input and cancellation do not trigger this retry. A failed visible-mode download ends the command; rerun extraction to select a fresh URL.

### Saved browser sessions

No profile flag is needed. Each starting hostname gets a separate profile under the system user-config directory: on macOS, `~/Library/Application Support/JavIt-DL/browser/<site-hash>`; on Linux, `$XDG_CONFIG_HOME/JavIt-DL/browser/<site-hash>` (default `~/.config`); on Windows, `%AppData%\JavIt-DL\browser\<site-hash>`.

Persistent cookies and site storage are reused by visible/headless runs for that hostname. Session cookies follow Chrome's normal lifetime rules, and verification may expire or be rejected after switching browser modes. Close an extraction before starting another for the same site; Chrome locks its profile. To clear saved sessions, close extraction browsers and remove the `JavIt-DL/browser` folder. No regular Chrome profile is modified.

### Capture behavior and limits

Capture classifies successful HTTP(S) responses (2xx) by URL extension and response MIME type. HLS is recognized through `.m3u8` or supported playlist MIME types; DASH through `.mpd` or its MIME type. Direct-file extensions include `.mp4`, `.webm`, `.mov`, `.m4a`, `.mp3`, `.ogg` and `.wav`; extensionless audio/video responses must be browser media requests. Extraction does not inspect response bodies, so an HLS URL with neither a recognized extension nor a playlist MIME type will not be listed.

The downloader copies the browser User-Agent, request Referer (unless `--referer` overrides it), and cookies scoped by domain/path/HTTPS before closing Chrome. Selecting an invalid number or unsupported DASH result keeps the browser open; selecting HLS or direct media closes it before download begins, including before HLS quality selection. Downloads use the copied session without a running browser. Headless failures retry once in visible mode as described above; failures after a visible selection end the command. Ctrl+C cancels extraction/download; the saved browser profile is retained. One run downloads one selected URL to `-o`: HLS normalizes the extension to `.mp4`, while direct media uses the exact path without transcoding. The output directory must exist and the requested path must be unused before the browser opens.

The initial tab is watched before navigation. New tabs and separate iframe targets are discovered every 500 ms; reload a new tab or replay the embedded video if its first media request was missed. Extraction observes network requests rather than crawling every HTML link. DASH, DRM, live HLS, separate HLS audio renditions, browser-only blobs, partitioned cookies and custom Authorization headers are not supported. Fragmented MP4 requests may represent individual segments rather than a whole video; prefer the HLS master playlist when available. Browser login/cookies do not guarantee that a site will allow a separate HTTP downloader.

YouTube watch URLs use the `youtube` command or automatic mode. `extract` downloads captured HLS/direct media itself; it does not route the page through yt-dlp or merge separate YouTube video/audio requests.

## YouTube

YouTube extraction and audio/video merging are delegated to yt-dlp and FFmpeg. The backend selects the best available H.264 (AVC) video and AAC audio, including a combined H.264/AAC stream as fallback, and remuxes to MP4 without re-encoding. If those codecs are unavailable, the download fails rather than silently selecting VP9/AV1/Opus. Available resolution may be lower than other codec variants. Keep yt-dlp updated as YouTube changes. The app invokes the installed executable directly; no additional Go wrapper library is needed.

One output path represents one video. Watch links with a playlist parameter still download only that video; pure playlist/channel URLs are rejected. Live links are passed to yt-dlp and may continue until the stream ends or you cancel. Private, age-restricted or bot-protected videos may require authentication and can fail; cookie/login options are not exposed by the YouTube backend. Logging in through `extract` does not supply cookies to yt-dlp.

Custom output paths follow the same folder-exists and no-overwrite rules as other downloads. yt-dlp writes to a private staging directory beside the destination; only a successful nonempty MP4 is atomically published. Failed/cancelled YouTube jobs remove their staging directory and do not resume across runs. App options control retries, socket timeout and custom HTTP headers. The CLI default User-Agent value (`JavIt-DL/2.0`) is not forwarded to yt-dlp; other configured User-Agent values are forwarded. Local yt-dlp configuration and plugins are disabled so they cannot change the app's output behavior.

## Safety and resume

- A failed or truncated resource prevents conversion; failed segments are never skipped.
- The HTTP downloader retries HTTP 408, 429, 5xx and transport/read failures with bounded delays. Other unexpected HTTP statuses fail immediately. Each raw retry restarts the file from zero.
- Downloads are streamed to private staging files. Only complete files are published. Existing outputs are never overwritten; choose a new output path if one exists.
- HLS cache identity includes the final playlist URL, exact playlist content, User-Agent and Referer. Resume requires these to remain the same. In `extract`, the browser User-Agent and captured Referer affect this identity; cookies are not included in the cache identity. A changed playlist or signed URL starts another cache directory.
- Only the successful job's own cache directory is deleted. Download, cancellation or conversion failures retain completed resources. Keys and cached media are private files.
- Ctrl+C or SIGTERM cancels HTTP requests and FFmpeg. Raw download staging files are removed; raw downloads do not resume across runs.
- Each HLS cache has a `.lock` directory to prevent simultaneous writers. After a process crash, remove **only that job's `.lock` directory**, and only after verifying that no process is using it.
- Raw file transfers have no whole-file deadline, so large files can take longer than 30 seconds. Interrupt a stalled transfer with Ctrl+C. HLS requests can use a larger `--timeout` for large segments.

## Supported media

Finished HLS (`EXT-X-ENDLIST`) with muxed audio/video or audio only, master variants, MPEG-TS, fMP4/CMAF initialization segments, byte ranges, AES-128 key rotation, explicit IVs and media-sequence-derived IVs. FFmpeg handles local decryption and remuxing, with its network protocols disabled. Relative references use the final redirected playlist URL and standard URL resolution; parent query strings are not automatically copied to child URLs.

Live/LL-HLS, separate external audio renditions, variable substitution, gap segments, I-frame-only playlists and DRM/SAMPLE-AES are explicitly rejected. MP4 conversion uses stream copy; incompatible codecs fail instead of silently transcoding or claiming success.

## Validation

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -o javit_dl .
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...

# Optional: browser regressions with Chrome/Chromium and a desktop session
JAVIT_DL_BROWSER_TEST=1 go test -race ./internal/downloader -run 'TestExtract(Browser|Headless|SavedSession)' -v
```

Tests generate short media locally with FFmpeg and serve it through a local HTTP server. Integration tests verify TS/fMP4/byte-range/AES-128 downloads, inspect the resulting MP4 with FFprobe and fully decode it with FFmpeg. Install FFmpeg and FFprobe to run those checks; they are skipped when the executables are absent. Automated tests contact no external media sites. YouTube backend tests use local fake executables to verify arguments, routing, publication, cancellation and failures. Extraction classification/deduplication checks run in the normal test suite. The optional browser test verifies JavaScript-triggered capture, duplicate suppression, signed query preservation, invalid selection, browser closure before download begins, and a direct-file download using copied browser cookies/User-Agent/Referer. Headless checks cover native autoplay, a delayed CSS play button, the capture deadline before selection, empty results and a missing selector. The headless fallback check verifies immediate handoff after HTTP 403 or empty capture, one shared terminal reader, a successful visible download, and no retry after quitting. The saved-session check verifies a local challenge handoff to a visible browser and persistent cookie reuse on a subsequent headless run; it briefly opens a browser window and does not solve a real Cloudflare challenge. Downloaded payloads are synthetic; it does not validate real-site extraction or media playback.

CI runs formatting, race/coverage tests, vet, build and vulnerability checks on macOS with Homebrew FFmpeg. The real Chrome extraction test is opt-in and is not enabled by the current CI workflow.

## Source layout

```text
main.go                         # Entry point and signal cancellation
assets.go                       # Embedded frontend build
build.sh                        # React + desktop Go build
frontend/                       # React components and Vite build
internal/
  cli/
    cli.go                      # urfave commands, flags and CLI defaults
    cli_test.go                 # CLI regression and end-to-end media tests
  gui/                          # Wails window, job state and GUI bindings
  downloader/
    download.go                 # Options, validation and download orchestration
    http.go                     # HTTP client, retries, raw transfers and progress
    hls.go                      # HLS selection, workers, resume and FFmpeg
    playlist.go                 # Playlist parsing and local manifest rewriting
    file.go                     # Atomic publication without overwriting
    extract.go                  # Interactive browser capture and session cookies
    extract_test.go             # Capture checks and opt-in local Chrome regression
    youtube.go                  # YouTube URL routing and yt-dlp integration
    youtube_test.go             # Local backend regression checks
    downloader_test.go          # Downloader, parser and filesystem regression tests
```

Dependencies flow from `main` into `internal/cli` and the optional `internal/gui` launcher; both reuse `internal/downloader`. The downloader does not depend on the CLI library. New commands belong in `internal/cli`; download behavior belongs in `internal/downloader`. Tests stay beside the package they exercise. The root entry point keeps `go run .` and `go build .` working.

## References

Implementation references:

- [Go releases](https://go.dev/dl/) and [toolchain selection](https://go.dev/doc/toolchain).
- [Go HTTP](https://pkg.go.dev/net/http), [signal cancellation](https://pkg.go.dev/os/signal#NotifyContext), [subprocess cancellation](https://pkg.go.dev/os/exec#CommandContext) and [file publication](https://pkg.go.dev/os#Link).
- [FFmpeg releases](https://ffmpeg.org/download.html) and [FFmpeg command-line documentation](https://ffmpeg.org/ffmpeg.html).
- [RFC 8216: HTTP Live Streaming](https://www.rfc-editor.org/rfc/rfc8216), particularly byte ranges, encryption scope, media sequence numbers and IVs.
- [Go vulnerability management](https://go.dev/doc/security/vuln/).
- [chromedp browser control](https://github.com/chromedp/chromedp) and [Chrome DevTools Network protocol](https://chromedevtools.github.io/devtools-protocol/tot/Network/).
- [yt-dlp](https://github.com/yt-dlp/yt-dlp) and [JavaScript runtime setup](https://github.com/yt-dlp/yt-dlp/wiki/EJS).
