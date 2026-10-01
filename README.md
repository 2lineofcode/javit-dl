# JavIt Downloader

A CLI downloader for finished HLS playlists and single HTTP(S) files. HLS downloads run in parallel, retain completed resources for resume, and produce MP4 through FFmpeg stream copy. No third-party Go dependencies.

## Requirements

- **Go 1.27.1+** to build (latest stable verified on 2026-10-01).
- **FFmpeg 9.0.2+** in `PATH` for HLS. Raw downloads do not need FFmpeg.
- The output directory must already exist and support hard links. Final publication uses a hard link to prevent overwriting an existing file, including concurrent writes.

On macOS:

```sh
brew install go ffmpeg
go build -trimpath -o javit_dl .
```

After changing source, rebuild before using `./javit_dl`. `go run .` always uses the current source.

## Usage

```sh
# Highest-bandwidth HLS variant → movie.mp4
go run . -u 'https://example.com/master.m3u8' -o movie.mp4 --auto

# Choose a master-playlist variant interactively
go run . -u 'https://example.com/master.m3u8' -o movie.mp4

# Download a single file unchanged; .mp4 is allowed
go run . -u 'https://example.com/video.mp4' -o video.mp4 --rawurl

# Resume the same failed HLS job with the same options
go run . download -u 'https://example.com/master.m3u8' -o movie.mp4 --auto -c 8 -r 3 -tf m3u8_tmp

# For origins that require these headers
go run . -u 'https://example.com/video.m3u8' -o movie.mp4 --auto \
  --user-agent 'Mozilla/5.0' --referer 'https://example.com/'

./javit_dl -h
```

| Option | Default | Meaning |
| --- | --- | --- |
| `-u`, `--url` | required | HTTP(S) playlist or file URL; embedded credentials are rejected |
| `-o`, `--out` | `output.mp4` | HLS output is normalized to `.mp4`; raw output uses the exact path |
| `-c`, `--concurrency` | `8` | Parallel HLS resource downloads, from 1 to 64 |
| `-r`, `--retries` | `3` | Extra attempts after failure, from 0 to 10 |
| `-tf`, `--temp` | `m3u8_tmp` | Parent directory for per-job HLS resume data |
| `--auto` | false | Choose the highest declared `BANDWIDTH` variant |
| `--rawurl` | false | Download one file unchanged, without HLS parsing or conversion |
| `--timeout` | `30s` | Whole request timeout for HLS resources; header/connect timeout for raw downloads |
| `--user-agent` | `JavIt-DL/2.0` | User-Agent sent for all requests |
| `--referer` | empty | Optional Referer sent for all requests |

Both `javit_dl -u ...` and `javit_dl download -u ...` are supported. Options must precede positional arguments. Web mode has been removed.

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

Tests generate short media locally with FFmpeg and serve it through a local HTTP server. Integration tests verify TS/fMP4/byte-range/AES-128 downloads, inspect the resulting MP4 with FFprobe and fully decode it with FFmpeg. Install FFmpeg and FFprobe to run those checks; they are skipped when the executables are absent. No external media sites are contacted. CI runs the same checks on macOS with Homebrew FFmpeg.

## Source layout

- `main.go`: flags, validation and cancellation.
- `playlist.go`: master selection metadata and rewriting remote resource references into a local playlist.
- `downloader.go`: HTTP streaming, bounded workers, retries, resume, atomic publication and FFmpeg.
- `downloader_test.go`: regression and integration checks.
- `_stable/`: historical snapshots, intentionally preserved; not included by `go build .` or `go test ./...` and not maintained. Use the root CLI.

See [REVIEW.md](REVIEW.md) for the maintenance review and validation boundaries.

## References

Version and implementation references were checked through Context7 (`/golang/go/go1.27.1`, `/websites/ffmpeg_documentation`) and official documentation on 2026-10-01:

- [Go releases](https://go.dev/dl/) and [toolchain selection](https://go.dev/doc/toolchain).
- [Go HTTP](https://pkg.go.dev/net/http), [signal cancellation](https://pkg.go.dev/os/signal#NotifyContext), [subprocess cancellation](https://pkg.go.dev/os/exec#CommandContext) and [file publication](https://pkg.go.dev/os#Link).
- [FFmpeg releases](https://ffmpeg.org/download.html) and [FFmpeg command-line documentation](https://ffmpeg.org/ffmpeg.html).
- [RFC 8216: HTTP Live Streaming](https://www.rfc-editor.org/rfc/rfc8216), particularly byte ranges, encryption scope, media sequence numbers and IVs.
- [Go vulnerability management](https://go.dev/doc/security/vuln/).
