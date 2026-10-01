# Maintenance review — 2026-10-01

Scope: the current working-tree CLI. The existing deletion of web modules and deployment configuration was preserved, as were historical `_stable/` snapshots. The user confirmed CLI-only maintenance.

## Findings and repairs

| Priority | Finding in the previous working-tree implementation | Repair |
| --- | --- | --- |
| Critical | TS source was deleted even when FFmpeg was unavailable or conversion failed. | Require FFmpeg before HLS downloading; publish MP4 only after successful conversion; retain resources on failure. |
| High | Failed TS/CMAF segments were skipped and jobs reported success with missing media. | Every resource must be downloaded successfully before conversion. |
| High | A single final encryption key was applied to all segments; the default IV was all zeros and media sequence was ignored. | Preserve per-segment key transitions and media sequence in a local manifest; delegate AES decryption to FFmpeg. |
| High | Ciphertext was zero-padded before decryption and PKCS7 padding was removed without validation. | Remove custom crypto; validate key length and ciphertext block alignment before native demuxing. |
| High | Fixed shared cache paths reused files across unrelated playlists and cleanup could delete a caller's entire temp directory. | Hash-scoped private caches, a per-job lock, atomic cache writes and cleanup restricted to the job directory. |
| High | `os.Create` immediately truncated outputs; read/copy/create failures were often ignored. | Stage beside the destination, check reads/writes/sync/close, and publish without overwrite through `os.Link`. |
| Medium | Nonpositive concurrency could hang or panic; negative retries could silently report success. | Validate bounded concurrency/retries, paths, headers and HTTP(S) input URLs before downloading. |
| Medium | One goroutine per segment caused unbounded goroutine growth. | Fixed worker pool with context cancellation on the first failure. |
| Medium | Requests and FFmpeg did not respond coherently to cancellation; raw downloads had a 30-second whole-file timeout. | Signal context cancels all work; raw transfers use connect/header timeouts without a fixed whole-file deadline. |
| Medium | Raw `.mp4` output was rejected, and command/help examples contradicted accepted flags. | Accept raw filenames unchanged; support current short flags, documented long aliases and the optional `download` subcommand. |
| Medium | Relative resources used the original URL rather than the final redirect destination. | Resolve against the final response URL; preserve each resource's own query parameters. |
| Medium | The FFmpeg H.264 filter was applied in the wrong remuxing direction and diagnostics were hidden. | Native HLS input, stream copy and MP4 faststart; visible diagnostics and a checked exit status. |
| Medium | No runnable regression coverage; README described deleted web code. | Local HTTP/media tests, current CLI documentation and CI checks. |

## Implementation and versions

The download flow was rewritten in three Go source files. Existing capabilities retained include bounded parallel downloads, retries, master-variant selection, raw downloads, HLS resume, AES-128 and CMAF/fMP4. HTTP and filesystem work use the standard library. FFmpeg receives a local manifest only after all resources are present; allowed input protocols are restricted to `file,crypto`.

Go was raised from 1.25.1 to the verified latest stable 1.27.1. The installed FFmpeg/FFprobe are already 9.0.2, matching the verified latest stable release. The working tree already removed Cobra and its transitive dependencies; that dependency-free direction was retained. CI uses verified current checkout v7.0.1 and setup-go v7.0.0, pinned to full commit hashes.

## Validation boundary

Validation commands and the final results are recorded below after execution. Real-media checks use locally generated audio/video, a local HTTP server and actual FFmpeg/FFprobe. They do not exercise private provider authentication, production CDN behavior, long videos, every codec or native Linux/Windows execution. Cross-compilation verifies compilation only.

The existing `_stable/` snapshots remain historical and contain old issues; they are outside the runnable root package. The removed web/deployment code has not been restored or validated. No staging, commit, push or deployment is performed by this maintenance work.

Unsupported playlists fail explicitly: live/LL-HLS, separate external audio renditions, variables, gaps, I-frame-only playlists and DRM/SAMPLE-AES. Atomic final publication requires a filesystem supporting hard links. A crashed job may leave a stale cache lock that requires manual removal after checking no job is running.

## Sources

Context7: `/golang/go/go1.27.1` and `/websites/ffmpeg_documentation`.

- [Latest stable Go release](https://go.dev/dl/) and [toolchain rules](https://go.dev/doc/toolchain).
- [Go HTTP transport](https://pkg.go.dev/net/http#Transport), [signal context](https://pkg.go.dev/os/signal#NotifyContext), [command context](https://pkg.go.dev/os/exec#CommandContext) and [hard-link publication](https://pkg.go.dev/os#Link).
- [FFmpeg latest stable release](https://ffmpeg.org/download.html) and [stream copy / mapping / fatal-error options](https://ffmpeg.org/ffmpeg.html).
- [HLS specification](https://www.rfc-editor.org/rfc/rfc8216), especially sections 4.3.2 and 5.2.
- [GitHub checkout release](https://github.com/actions/checkout/releases/tag/v7.0.1) and [setup-go release](https://github.com/actions/setup-go/releases/tag/v7.0.0).
- [Official Go vulnerability checker](https://go.dev/doc/security/vuln/).

## Final verification results

| Check | Result |
| --- | --- |
| `go test -race -cover ./...` | Passed; 84.1% statement coverage, no race reports. |
| Actual media integration | Passed for TS, fMP4, fMP4 byte ranges and AES-128; manual/automatic variant selection and redirected bases exercised. |
| Encryption integration | Nonzero media sequence, key rotation, short explicit IV normalization and `METHOD=NONE` transition passed. |
| Resulting media | FFprobe confirmed audio/video and expected duration; FFmpeg fully decoded every generated MP4 without errors. |
| Failure/cancellation/resume | Truncated raw responses, missing resources, invalid keys/ranges/ciphertext, conversion failure, collision protection, cancellation and reuse of completed resources passed. |
| Concurrency/retry | Bounded workers and transient 503 retries passed. |
| `go vet ./...` | Passed. |
| `go build -trimpath -o javit_dl .` | Passed; the local macOS executable was rebuilt and CLI help/input rejection checked. |
| Linux and Windows amd64 builds | Passed with `CGO_ENABLED=0`; no execution on those OSes was performed. The initial cross-build inherited enabled CGO and failed against the macOS C compiler, so pure-Go cross-builds were used. |
| `govulncheck@v1.8.0 ./...` | No vulnerabilities found. |
| Formatting and `git diff --check` | Passed. |
| GitHub CI | Workflow added and reviewed locally; no remote CI run was triggered. |
