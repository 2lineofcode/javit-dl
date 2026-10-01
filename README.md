# JavIt Downloader

A CLI downloader for YouTube videos, finished HLS playlists and single HTTP(S) files. HLS downloads run in parallel, retain completed resources for resume, and produce MP4 through FFmpeg stream copy. The CLI uses [urfave/cli v3](https://cli.urfave.org/v3/).

## Requirements

- **Go 1.27.1+** to build (latest stable verified on 2026-10-01).
- **FFmpeg 9.0.2+** in `PATH` for HLS and YouTube. Direct file downloads do not need FFmpeg.
- **[yt-dlp](https://github.com/yt-dlp/yt-dlp)** in `PATH` for YouTube. Install a supported JavaScript runtime such as **Deno**; see [yt-dlp EJS setup](https://github.com/yt-dlp/yt-dlp/wiki/EJS) for runtime and challenge-solver requirements.
- The output directory must already exist and support hard links. Final publication uses a hard link to prevent overwriting an existing file, including concurrent writes.

On macOS:

```sh
brew install go ffmpeg yt-dlp deno
go build -trimpath -o javit_dl .
```

After changing source, rebuild before using `./javit_dl`. `go run .` always uses the current source.

## Usage

Pass a URL directly to route YouTube video links to yt-dlp, detect HLS from the response body and save other responses unchanged. Detection does not depend on the URL extension or Content-Type. Use `youtube <URL>`, `hls <URL>` or `file <URL>` when you want to force a type.

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

# Resume the same failed HLS job with the same options
./javit_dl 'https://example.com/master.m3u8' -o movie.mp4 --auto -c 8 -r 3 --temp m3u8_tmp

# For origins that require these headers
./javit_dl 'https://example.com/video.m3u8' -o movie.mp4 --auto \
  --user-agent 'Mozilla/5.0' --referer 'https://example.com/'

./javit_dl --help
./javit_dl hls --help
./javit_dl file --help
./javit_dl youtube --help

# Run from source using the same commands
go run . 'https://example.com/master.m3u8' -o movie.mp4 --auto
```

| Option | Commands | Default | Meaning |
| --- | --- | --- | --- |
| `-o`, `--out` | all | `output.mp4` | HLS and YouTube output is normalized to `.mp4`; file downloads use the exact path |
| `-r`, `--retries` | all | `3` | Extra attempts after failure, from 0 to 10 |
| `--timeout` | all | `30s` | Whole request timeout for HLS resources; socket timeout for YouTube; header/connect timeout for file downloads |
| `--user-agent` | all | `JavIt-DL/2.0` | User-Agent sent for all requests |
| `--referer` | all | empty | Optional Referer sent for all requests |
| `-c`, `--concurrency` | automatic, `hls` | `8` | Parallel HLS resource downloads, from 1 to 64 |
| `--tf`, `--temp` | automatic, `hls` | `m3u8_tmp` | Parent directory for per-job HLS resume data; `-tf` is also accepted |
| `--auto` | automatic, `hls` | false | Choose the highest declared `BANDWIDTH` variant |

Automatic mode and each command accept exactly one absolute HTTP(S) URL without embedded credentials. Options may appear before or after the URL. The old `download`, `-u`/`--url` and `--rawurl` syntax has been replaced by a positional URL, with optional `hls`, `file` and `youtube` commands. Web mode has been removed.

Automatic file transfers reuse the inspected response, so detection does not issue another file request. For HLS, detection reads a bounded prefix and then fetches the full playlist using the HLS request timeout. Use `hls <URL>` to avoid this initial detection request when a source only allows a single request.

## YouTube

YouTube extraction and audio/video merging are delegated to yt-dlp and FFmpeg. The backend selects the best available H.264 (AVC) video and AAC audio, including a combined H.264/AAC stream as fallback, and remuxes to MP4 without re-encoding. If those codecs are unavailable, the download fails rather than silently selecting VP9/AV1/Opus. Available resolution may be lower than other codec variants. Keep yt-dlp updated as YouTube changes. The app invokes the installed executable directly; no additional Go wrapper library is needed.

One output path represents one video. Watch links with a playlist parameter still download only that video; pure playlist/channel URLs are rejected. Live links are passed to yt-dlp and may continue until the stream ends or you cancel. Private, age-restricted or bot-protected videos may require authentication and can fail; cookie/login options are not exposed yet.

Custom output paths follow the same folder-exists and no-overwrite rules as other downloads. yt-dlp writes to a private staging directory beside the destination; only a successful nonempty MP4 is atomically published. Failed/cancelled YouTube jobs remove their staging directory and do not resume across runs. App options control retries, socket timeout and custom HTTP headers; local yt-dlp configuration and plugins are disabled so they cannot change the app's output behavior.

## Safety and resume

- A failed or truncated resource prevents conversion; failed segments are never skipped.
- HTTP 408, 429, 5xx and transport/read failures are retried with bounded delays. Other HTTP statuses fail immediately. Each raw retry restarts the file from zero.
- Downloads are streamed to private staging files. Only complete files are published. Existing outputs are never overwritten; choose a new output path if one exists.
- HLS cache identity includes the final playlist URL, exact playlist content, User-Agent and Referer. Resume requires these to remain the same. A changed playlist or signed URL starts another cache directory.
- Only the successful job's own cache directory is deleted. Download, cancellation or conversion failures retain completed resources. Keys and cached media are private files.
- Ctrl+C or SIGTERM cancels HTTP requests and FFmpeg. Raw download staging files are removed; raw downloads do not resume across runs.
- Each HLS cache has a `.lock` directory to prevent simultaneous writers. After a process crash, remove **only that job's `.lock` directory**, and only after verifying that no process is using it.
- Raw file transfers have no whole-file deadline, so large files can take longer than 30 seconds. Interrupt a stalled transfer with Ctrl+C. HLS requests can use a larger `--timeout` for large segments.

## Supported media

Finished HLS (`EXT-X-ENDLIST`) with muxed audio/video or audio only, master variants, MPEG-TS, fMP4/CMAF initialization segments, byte ranges, AES-128 key rotation, explicit IVs and media-sequence-derived IVs. FFmpeg handles local decryption and remuxing, with its network protocols disabled. Relative references use the final redirected playlist URL and standard URL resolution; parent query strings are not automatically copied to child URLs.

Live/LL-HLS, separate external audio renditions, variable substitution, gap segments, I-frame-only playlists and DRM/SAMPLE-AES are explicitly rejected. The old implementation did not reliably handle these cases. MP4 conversion uses stream copy; incompatible codecs fail instead of silently transcoding or claiming success.

## Validation

```sh
go test -race -cover ./...
go vet ./...
go build -trimpath -o javit_dl .
go run golang.org/x/vuln/cmd/govulncheck@v1.8.0 ./...
```

Tests generate short media locally with FFmpeg and serve it through a local HTTP server. Integration tests verify TS/fMP4/byte-range/AES-128 downloads, inspect the resulting MP4 with FFprobe and fully decode it with FFmpeg. Install FFmpeg and FFprobe to run those checks; they are skipped when the executables are absent. Automated tests contact no external media sites. YouTube backend tests use local fake executables to verify arguments, routing, publication, cancellation and failures. CI runs the same checks on macOS with Homebrew FFmpeg.

## Source layout

```text
main.go                         # Entry point and signal cancellation
internal/
  cli/
    cli.go                      # urfave commands, flags and CLI defaults
    cli_test.go                 # CLI regression and end-to-end media tests
  downloader/
    download.go                 # Options, validation and download orchestration
    http.go                     # HTTP client, retries, raw transfers and progress
    hls.go                      # HLS selection, workers, resume and FFmpeg
    playlist.go                 # Playlist parsing and local manifest rewriting
    file.go                     # Atomic publication without overwriting
    youtube.go                  # YouTube URL routing and yt-dlp integration
    youtube_test.go             # Local backend regression checks
    downloader_test.go          # Downloader, parser and filesystem regression tests
```

Dependencies flow from `main` → `internal/cli` → `internal/downloader`. The downloader does not depend on the CLI library. New commands belong in `internal/cli`; download behavior belongs in `internal/downloader`. Tests stay beside the package they exercise. The root entry point keeps `go run .` and `go build .` working.

See [REVIEW.md](REVIEW.md) for the maintenance review and validation boundaries.

## References

Version and implementation references were checked through Context7 (`/golang/go/go1.27.1`, `/websites/ffmpeg_documentation`) and official documentation on 2026-10-01:

- [Go releases](https://go.dev/dl/) and [toolchain selection](https://go.dev/doc/toolchain).
- [Go HTTP](https://pkg.go.dev/net/http), [signal cancellation](https://pkg.go.dev/os/signal#NotifyContext), [subprocess cancellation](https://pkg.go.dev/os/exec#CommandContext) and [file publication](https://pkg.go.dev/os#Link).
- [FFmpeg releases](https://ffmpeg.org/download.html) and [FFmpeg command-line documentation](https://ffmpeg.org/ffmpeg.html).
- [RFC 8216: HTTP Live Streaming](https://www.rfc-editor.org/rfc/rfc8216), particularly byte ranges, encryption scope, media sequence numbers and IVs.
- [Go vulnerability management](https://go.dev/doc/security/vuln/).
