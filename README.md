# M3U8 / File Downloader (CLI + Web)

Fast, resumable M3U8 (HLS) and single-file downloader with optional AES-128 decryption and automatic MP4 conversion, available as both CLI and Web UI.

## Requirements
- Go 1.25+
- ffmpeg available in PATH (or at `/opt/homebrew/bin/ffmpeg` on macOS). Used to convert TS → MP4.
- github.com/spf13/cobra (automatically installed via `go mod download`)

## Build / Run

CLI mode:
```bash
# run
go run . download -u https://example.com/playlist.m3u8 -o output.ts

# build
go build -o javit_dl
./javit_dl download -u https://example.com/playlist.m3u8 -o output.ts
```

Web mode:
```bash
go run . serve -p :8080
# or after building:
./javit_dl serve -p :8080
# open http://localhost:8080
```

## Usage

### CLI Commands

**`javit_dl download`** - Download M3U8 or single file

Flags:
- `-u, --url` string: URL to m3u8 playlist or raw file (required)
- `-o, --out` string: Output file (TS; MP4 is produced automatically next to it) (default "output.ts")
- `-c, --concurrency` int: Concurrent downloads (M3U8 only) (default 8)
- `-r, --retries` int: Retries per segment (M3U8 only) (default 3)
- `-t, --temp` string: Temporary segment directory (M3U8 only) (default "m3u8_tmp")
- `--auto` bool: Auto select highest bandwidth variant for master playlists (M3U8 only)
- `--rawurl` bool: Download the URL as a single file (skip M3U8 parsing)

**`javit_dl serve`** - Start web server

Flags:
- `-p, --port` string: Server port (default ":8080")

Examples:
```bash
# Download M3U8 with auto-select highest variant
./javit_dl download -u https://example.com/master.m3u8 -o movie.ts --auto

# Download single file (non-m3u8)
./javit_dl download -u https://example.com/video.ts -o video.ts --rawurl

# Custom concurrency and retries
./javit_dl download -u https://example.com/playlist.m3u8 -o output.ts -c 16 -r 5

# Start web server on custom port
./javit_dl serve -p :3000
```

### Web UI
- Single-page app at `/` with:
  - URL field
  - Output: filename (without .mp4) + folder (with a folder picker modal)
  - Options: raw mode, auto-select variant, concurrency, retries
  - Inline progress bar, live status (`progress n/TotalSegments (percentage%)`)
  - On completion: shows `Downloaded <path>` with a link to play inline

Endpoints used by the SPA:
- `POST /download` → start job; returns `{ id }`
- `GET /status?id=...` → returns JSON progress
- `GET /view?id=...` → inline-play MP4 in browser
- `GET /api/dirs?p=/path` → list subfolders for the folder picker

Notes:
- The folder picker can navigate the entire filesystem.
- Hidden folders (starting with `.`) are not shown.
- Temp data for web jobs is written to the OS temp dir (e.g. `$TMPDIR` on macOS, `/tmp` on Linux) and cleaned up after finishing.

## Features
- Master playlist support with variant autodetection (`-auto`)
- AES-128 segment decryption when key is present
- Concurrent segment downloading with retries
- Automatic merge and faststart MP4 conversion (via `ffmpeg`)
- Web UI with progress, folder picking, inline playback

## Project Structure
```
.
├─ main.go                         # Entry point
├─ cmd/
│  ├─ root.go                      # Cobra root command & flags
│  ├─ download.go                  # Download subcommand
│  └─ serve.go                     # Serve subcommand
├─ internal/
│  ├─ globals/
│  │  └─ globals.go               # shared HTTP client & user-agent
│  ├─ downloader/
│  │  ├─ downloader.go            # pipeline: fetch, parse, decrypt, merge, convert
│  │  ├─ types.go                 # KeyInfo, Segment
│  │  └─ util.go                  # helpers (format/expand)
│  ├─ jobs/
│  │  └─ jobs.go                  # job progress store & helpers
│  └─ server/
│     └─ server.go                # HTTP handlers & Start(...)
└─ web/
   └─ rootpage.go                 # HTML/CSS/JS (inlined template)
```

## Tips
- If `ffmpeg` is missing, MP4 conversion will be skipped.
- If some segments fail, the downloader continues and merges available segments (skipped segments are logged).
- For very large playlists, consider lowering `-c` if the host rate-limits.

## License
MIT
