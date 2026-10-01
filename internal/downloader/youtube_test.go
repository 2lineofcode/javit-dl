package downloader

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestYouTubeRouting(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{"https://www.youtube.com/watch?v=BaW_jenozKc", true},
		{"https://youtu.be/BaW_jenozKc", true},
		{"https://music.youtube.com/watch?v=BaW_jenozKc", true},
		{"https://m.youtube.com/shorts/BaW_jenozKc", true},
		{"https://www.youtube-nocookie.com/embed/BaW_jenozKc", true},
		{"https://YOUTUBE.COM/live/BaW_jenozKc", true},
		{"https://youtube.com.example.com/watch?v=BaW_jenozKc", false},
		{"https://notyoutube.com/watch?v=BaW_jenozKc", false},
		{"https://example.com/video.mp4", false},
	} {
		u, err := parseHTTPURL(tc.url)
		if err != nil || isYouTubeURL(u) != tc.want {
			t.Errorf("routing %s: err=%v", tc.url, err)
		}
	}
}

func TestYouTubeDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executables use a POSIX shell")
	}
	bin := t.TempDir()
	stub := `#!/bin/sh
printf '%s\n' "$@" > "$TEST_YOUTUBE_ARGS"
case "$TEST_YOUTUBE_MODE" in
  failure) printf 'partial' > video.mp4; exit 1 ;;
  empty) : > video.mp4 ;;
  missing) exit 0 ;;
  cancel) exec /bin/sleep 30 ;;
  collision) printf 'keep' > "$TEST_YOUTUBE_OUT"; printf 'video' > video.mp4 ;;
  *) printf 'video' > video.mp4 ;;
esac
`
	for name, body := range map[string]string{"yt-dlp": stub, "ffmpeg": "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	for _, mode := range []string{"success", "force-success", "failure", "empty", "missing", "collision", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			out := filepath.Join(dir, "custom % file.mp4")
			argsFile := filepath.Join(t.TempDir(), "args")
			t.Setenv("TEST_YOUTUBE_MODE", mode)
			t.Setenv("TEST_YOUTUBE_ARGS", argsFile)
			t.Setenv("TEST_YOUTUBE_OUT", out)
			o := testOptions()
			o.URL, o.Out, o.Detect = "https://youtu.be/BaW_jenozKc", strings.TrimSuffix(out, ".mp4")+".mkv", true
			o.Detect, o.YouTube = mode != "force-success", mode == "force-success"
			o.UserAgent, o.Referer = "custom agent", "https://example.com/"
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 200*time.Millisecond)
				defer cancel()
			}
			err := Download(ctx, o, nil, io.Discard, io.Discard)
			if strings.Contains(mode, "success") {
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(out)
				if err != nil || string(body) != "video" {
					t.Fatalf("wrong published file: %v", err)
				}
				before, err := os.ReadFile(argsFile)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range []string{"--ignore-config\n", "--no-plugin-dirs\n", "--no-playlist\n", "--abort-on-unavailable-fragments\n", "--extractor-retries\n0\n", "--format\nbv[vcodec^=avc1]+ba[acodec^=mp4a]/b[vcodec^=avc1][acodec^=mp4a]\n", "--remux-video\nmp4\n", "video.%(ext)s\n", "--retries\n0\n", "--socket-timeout\n5\n", "User-Agent:custom agent\n", "Referer:https://example.com/\n", "--\nhttps://youtu.be/BaW_jenozKc\n"} {
					if !strings.Contains(string(before), want) {
						t.Errorf("missing yt-dlp args %q: %s", want, before)
					}
				}
				if err := Download(ctx, o, nil, io.Discard, io.Discard); err == nil {
					t.Fatal("existing output accepted")
				}
			} else if err == nil {
				t.Fatal("failed/incomplete download accepted")
			}
			if mode == "cancel" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation lost: %v", err)
			}
			if mode == "collision" {
				body, _ := os.ReadFile(out)
				if !errors.Is(err, os.ErrExist) || string(body) != "keep" {
					t.Fatalf("concurrent output overwritten: %v", err)
				}
			}
			entries, err := os.ReadDir(dir)
			want := 0
			if strings.Contains(mode, "success") || mode == "collision" {
				want = 1
			}
			if err != nil || len(entries) != want {
				t.Fatalf("temporary files leaked: %v", entries)
			}
		})
	}
}

func TestYouTubeValidation(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, raw := range []string{"https://example.com/video", "https://youtube.com/playlist?list=abc", "https://youtube.com/@channel"} {
		o := testOptions()
		o.URL, o.Out, o.YouTube = raw, filepath.Join(t.TempDir(), "video.mp4"), true
		if err := Download(context.Background(), o, nil, io.Discard, io.Discard); err == nil || strings.Contains(err.Error(), "require yt-dlp") {
			t.Fatalf("invalid YouTube URL reached backend: %s: %v", raw, err)
		}
	}
	o := testOptions()
	o.URL, o.Out, o.Detect = "https://youtu.be/BaW_jenozKc", filepath.Join(t.TempDir(), "video.mp4"), true
	if err := Download(context.Background(), o, nil, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "require yt-dlp") {
		t.Fatalf("missing dependency error: %v", err)
	}
}
