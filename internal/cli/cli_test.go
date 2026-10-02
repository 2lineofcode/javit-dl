package cli

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{"hls", "file:///etc/passwd"}, {"hls", "https://example.com", "-c", "0"},
		{"file", "https://example.com", "-r", "-1"}, {"hls", "https://example.com", "--timeout", "0s"},
		{"file", "https://example.com", "-o", ""}, {"hls", "https://user:pass@example.com"},
		{"file", "https://example.com", "--referer", "bad\r\nheader"}, {"serve"},
		{"--unknown"}, {"download"}, {"-u", "https://example.com"},
		{"hls"}, {"file"}, {"hls", "https://example.com", "extra"}, {"file", "https://example.com", "extra"},
		{"file", "https://example.com", "--rawurl"}, {"file", "https://example.com", "--auto"},
		{"file", "https://example.com", "--out"},
		{"https://example.com", "extra"}, {"https://example.com", "--timeout", "0s"},
		{"https://example.com", "--referer", "bad\r\nheader"}, {"https://user:pass@example.com"}, {"extract", "https://example.com", "--headless", "--capture-duration", "0s"}, {"extract", "https://example.com", "--headless", "--capture-duration", "-1s"}, {"extract", "https://example.com", "--play-selector", ".play"}, {"file", "https://example.com", "--headless"}, {"extract"}, {"extract", "file:///tmp/video"}, {"extract", "https://example.com", "--timeout", "0s"}, {"extract", "https://example.com", "extra"}, {"youtube"}, {"youtube", "https://example.com/video"}, {"youtube", "https://youtu.be/BaW_jenozKc", "extra"},
	} {
		if err := Run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Errorf("expected invalid arguments: %q", args)
		}
	}
	if err := Run(context.Background(), []string{"-h"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestCLIHelp(t *testing.T) {
	for _, args := range [][]string{nil, {"-h"}, {"--help"}, {"hls", "--help"}, {"help", "hls"}, {"file", "--help"}, {"help", "file"}, {"youtube", "--help"}, {"help", "youtube"}, {"extract", "--help"}, {"help", "extract"}} {
		var output, stderr bytes.Buffer
		if err := Run(context.Background(), args, nil, &output, &stderr); err != nil {
			t.Fatalf("help %q: %v", args, err)
		}
		for _, want := range []string{"javit_dl", "Examples:", "https://", "-o"} {
			if !strings.Contains(output.String(), want) {
				t.Errorf("help %q missing %s: %s", args, want, output.String())
			}
		}
		if strings.Contains(output.String(), "--rawurl") || strings.Contains(output.String(), "--url") {
			t.Errorf("help %q contains removed flags: %s", args, output.String())
		}
		if (len(args) > 0 && args[0] == "file") || (len(args) > 1 && args[1] == "file") {
			if strings.Contains(output.String(), "--auto") || strings.Contains(output.String(), "--temp") {
				t.Errorf("file help contains HLS options: %s", output.String())
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("help %q wrote errors: %s", args, stderr.String())
		}
	}
}

func TestFileDownload(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // Direct files do not require FFmpeg.
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != "test" || r.Header.Get("Referer") != "https://example.com/" {
			t.Error("missing configured headers")
		}
		if attempts.Add(1) == 1 {
			w.Header().Set("Content-Length", "20")
			io.WriteString(w, "short")
			return
		}
		io.WriteString(w, "complete")
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "file.mp4")
	err := Run(context.Background(), []string{server.URL, "--out", out, "--retries", "1", "--user-agent", "test", "--referer", "https://example.com/"}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "complete" || attempts.Load() != 2 {
		t.Fatalf("bad retry output: %q, attempts %d", data, attempts.Load())
	}
	if err := Run(context.Background(), []string{"file", "-o", out, server.URL}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("existing output accepted")
	}
	if attempts.Load() != 2 {
		t.Fatal("existing output should fail before HTTP request")
	}
}

func TestAutomaticFileDownload(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, tc := range []struct {
		name, body string
		forceFile  bool
	}{
		{"short", "abc", false},
		{"binary", strings.Repeat("\x00\xffabc", 2000), false},
		{"not-a-playlist", "#EXTM3U-not-a-playlist", false},
		{"force-file", "#EXTM3U\n#EXT-X-ENDLIST\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/misleading.m3u8", http.StatusFound)
					return
				}
				requests.Add(1)
				if r.Header.Get("User-Agent") != "test" || r.Header.Get("Referer") != "https://example.com/" {
					t.Error("missing detection/download headers")
				}
				w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "custom file.bin")
			args := []string{server.URL + "/redirect", "-o", out, "--user-agent", "test", "--referer", "https://example.com/"}
			if tc.forceFile {
				args = append([]string{"file"}, args...)
			}
			if err := Run(context.Background(), args, nil, io.Discard, io.Discard); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(out)
			if err != nil || string(body) != tc.body || requests.Load() != 1 {
				t.Fatalf("bytes lost or file refetched: err=%v, requests=%d", err, requests.Load())
			}
			if err := Run(context.Background(), args, nil, io.Discard, io.Discard); err == nil || requests.Load() != 1 {
				t.Fatal("existing file should fail before a request")
			}
		})
	}
}

func TestAutomaticHLSFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	for _, header := range []string{"#EXTM3U\n", "\ufeff#EXTM3U\r\n", "#EXTM3U \n"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, header+"#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
		}))
		out := filepath.Join(t.TempDir(), "movie.bin")
		err := Run(context.Background(), []string{server.URL, "-o", out, "-r", "0"}, nil, io.Discard, io.Discard)
		server.Close()
		if err == nil || !strings.Contains(err.Error(), "ffmpeg is required") {
			t.Fatalf("playlist was not detected: %v", err)
		}
		entries, err := os.ReadDir(filepath.Dir(out))
		if err != nil || len(entries) != 0 {
			t.Fatalf("failed detection/download left output or staging files: %v", entries)
		}
	}
}

func TestAutomaticOutputCollision(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nsegment.ts\n#EXT-X-ENDLIST\n")
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "movie.mp4")
	if err := os.WriteFile(out, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), []string{server.URL, "-o", strings.TrimSuffix(out, ".mp4") + ".bin"}, nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "output already exists") || requests.Load() != 1 {
		t.Fatalf("normalized collision not rejected after detection: %v", err)
	}
	body, err := os.ReadFile(out)
	entries, dirErr := os.ReadDir(filepath.Dir(out))
	if err != nil || string(body) != "keep" || dirErr != nil || len(entries) != 1 {
		t.Fatal("existing output changed or staging file leaked")
	}
}

func TestHLSIntegration(t *testing.T) {
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	for _, mode := range []string{"ts", "fmp4", "fmp4-range", "aes"} {
		t.Run(mode, func(t *testing.T) {
			media := t.TempDir()
			args := []string{"-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100", "-t", "2.4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-g", "10", "-c:a", "aac", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0"}
			if strings.HasPrefix(mode, "fmp4") {
				args = append(args, "-hls_segment_type", "fmp4")
			}
			args = append(args, filepath.Join(media, "list.m3u8"))
			if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				t.Fatalf("generate media: %v\n%s", err, output)
			}
			body, err := os.ReadFile(filepath.Join(media, "list.m3u8"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "fmp4-range" {
				initData, err := os.ReadFile(filepath.Join(media, "init.mp4"))
				if err != nil {
					t.Fatal(err)
				}
				bundle := append([]byte(nil), initData...)
				var ranged strings.Builder
				first := true
				for _, line := range strings.Split(string(body), "\n") {
					if strings.HasPrefix(line, "#EXT-X-MAP:") {
						line = fmt.Sprintf(`#EXT-X-MAP:URI="bundle.mp4",BYTERANGE="%d@0"`, len(initData))
					}
					if line != "" && !strings.HasPrefix(line, "#") {
						data, err := os.ReadFile(filepath.Join(media, line))
						if err != nil {
							t.Fatal(err)
						}
						fmt.Fprintf(&ranged, "#EXT-X-BYTERANGE:%d", len(data))
						if first {
							fmt.Fprintf(&ranged, "@%d", len(bundle))
							first = false
						}
						fmt.Fprintln(&ranged)
						bundle = append(bundle, data...)
						line = "bundle.mp4"
					}
					fmt.Fprintln(&ranged, line)
				}
				body = []byte(ranged.String())
				if err := os.WriteFile(filepath.Join(media, "bundle.mp4"), bundle, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(media, "list.m3u8"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "aes" {
				var encrypted strings.Builder
				seq := uint64(42)
				for _, line := range strings.Split(string(body), "\n") {
					if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
						line = "#EXT-X-MEDIA-SEQUENCE:42"
					}
					if line != "" && !strings.HasPrefix(line, "#") {
						if seq == 44 {
							fmt.Fprintln(&encrypted, "#EXT-X-KEY:METHOD=NONE")
							fmt.Fprintln(&encrypted, line)
							seq++
							continue
						}
						key := bytes.Repeat([]byte{byte(seq)}, 16)
						keyName := fmt.Sprintf("key%d.bin", seq)
						os.WriteFile(filepath.Join(media, keyName), key, 0600)
						fmt.Fprintf(&encrypted, "#EXT-X-KEY:METHOD=AES-128,URI=\"%s\"", keyName)
						if seq == 43 {
							fmt.Fprintf(&encrypted, ",IV=0x%x", seq)
						}
						fmt.Fprintln(&encrypted)
						data, err := os.ReadFile(filepath.Join(media, line))
						if err != nil {
							t.Fatal(err)
						}
						n := aes.BlockSize - len(data)%aes.BlockSize
						data = append(data, bytes.Repeat([]byte{byte(n)}, n)...)
						iv := make([]byte, 16)
						binary.BigEndian.PutUint64(iv[8:], seq)
						block, _ := aes.NewCipher(key)
						cipher.NewCBCEncrypter(block, iv).CryptBlocks(data, data)
						os.WriteFile(filepath.Join(media, line), data, 0600)
						seq++
					}
					fmt.Fprintln(&encrypted, line)
				}
				body = []byte(encrypted.String())
				os.WriteFile(filepath.Join(media, "list.m3u8"), body, 0600)
			}
			var counts sync.Map
			files := http.FileServer(http.Dir(media))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirect" {
					http.Redirect(w, r, "/nested/master.m3u8", http.StatusFound)
					return
				}
				if r.URL.Path == "/nested/master.m3u8" {
					master := "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\n../missing.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200\n../list.m3u8\n"
					if mode == "aes" {
						master = "\ufeff" + master
					}
					io.WriteString(w, master)
					return
				}
				counter, _ := counts.LoadOrStore(r.URL.Path, new(atomic.Int32))
				counter.(*atomic.Int32).Add(1)
				files.ServeHTTP(w, r)
			}))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "movie.mp4")
			temp := t.TempDir()
			var stderr bytes.Buffer
			cliArgs := []string{"hls", server.URL + "/redirect", "-o", out, "-tf", temp}
			if mode != "ts" {
				cliArgs = append(cliArgs[1:], "--auto")
				cliArgs[2] = strings.TrimSuffix(out, ".mp4") + ".ts"
			}
			err = Run(context.Background(), cliArgs, strings.NewReader("1\n"), io.Discard, &stderr)
			if err != nil {
				t.Fatalf("download: %v\n%s", err, stderr.String())
			}
			output, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type", "-show_entries", "format=duration", "-of", "json", out).CombinedOutput()
			if err != nil || !bytes.Contains(output, []byte(`"video"`)) || !bytes.Contains(output, []byte(`"audio"`)) {
				t.Fatalf("invalid MP4: %v\n%s", err, output)
			}
			var metadata struct{ Format struct{ Duration string } }
			if err := json.Unmarshal(output, &metadata); err != nil {
				t.Fatal(err)
			}
			duration, err := strconv.ParseFloat(metadata.Format.Duration, 64)
			if err != nil || duration < 2.3 || duration > 2.6 {
				t.Fatalf("missing media: duration %f, %v", duration, err)
			}
			if decoded, err := exec.Command("ffmpeg", "-v", "error", "-xerror", "-i", out, "-f", "null", "-").CombinedOutput(); err != nil {
				t.Fatalf("media is not decodable: %v\n%s", err, decoded)
			}
			remaining, _ := os.ReadDir(temp)
			if len(remaining) != 0 {
				t.Fatalf("cache not cleaned: %v", remaining)
			}
			counts.Range(func(k, v any) bool {
				want := int32(1)
				if k == "/bundle.mp4" {
					want = 4
				}
				if v.(*atomic.Int32).Load() != want {
					t.Errorf("resource %s fetched repeatedly", k)
				}
				return true
			})
		})
	}
}

func TestGUILauncher(t *testing.T) {
	called := false
	launch := func(context.Context, map[string]any) error { called = true; return nil }
	if err := RunWithGUI(context.Background(), []string{"gui"}, strings.NewReader(""), io.Discard, io.Discard, launch); err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
	called = false
	if err := RunWithGUI(context.Background(), []string{"gui", "unexpected"}, strings.NewReader(""), io.Discard, io.Discard, launch); err == nil || called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

func TestGUIParameters(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want map[string]any
	}{
		{[]string{"gui"}, map[string]any{}},
		{[]string{"gui", "-u", "http://example.com/video", "--startdownload"}, map[string]any{"url": "http://example.com/video", "startDownload": true}},
		{[]string{"gui", "--startdownload=false"}, map[string]any{"startDownload": false}},
		{[]string{"gui", "-u", "https://example.com/watch?token=a%2Fb", "-o", "movie.mp4", "-c", "4", "-r", "0", "--timeout", "45s", "--auto=false", "--headless", "--capture-duration", "20s", "--play-selector", ".play", "--browser", "/path/to/chrome", "--temp", "cache", "--user-agent", "Custom UA", "--referer", "https://example.com/"}, map[string]any{"url": "https://example.com/watch?token=a%2Fb", "out": "movie.mp4", "concurrency": 4, "retries": 0, "timeout": 45, "auto": false, "headless": true, "captureDuration": 20, "playSelector": ".play", "browser": "/path/to/chrome", "temp": "cache", "userAgent": "Custom UA", "referer": "https://example.com/", "mode": "extract"}},
		{[]string{"gui", "--url", "http://example.com/video", "--mode", "file"}, map[string]any{"url": "http://example.com/video", "mode": "file"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var got map[string]any
			err := RunWithGUI(context.Background(), tc.args, strings.NewReader(""), io.Discard, io.Discard, func(_ context.Context, initial map[string]any) error { got = initial; return nil })
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%#v want=%#v err=%v", got, tc.want, err)
			}
		})
	}
	for _, args := range [][]string{{"gui", "--startdownload"}, {"gui", "-u", " ", "--startdownload"}, {"gui", "-u"}, {"gui", "-x"}, {"gui", "--mode", "invalid"}, {"gui", "--timeout", "500ms"}, {"gui", "--timeout", "1500ms"}, {"gui", "--capture-duration", "2h"}, {"gui", "--mode", "file", "--headless"}, {"gui", "--play-selector", ".play"}} {
		called := false
		err := RunWithGUI(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard, func(context.Context, map[string]any) error { called = true; return nil })
		if err == nil || called {
			t.Fatalf("accepted invalid parameters %v: called=%v err=%v", args, called, err)
		}
	}
}

func TestCLIOverwrite(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "new file") }))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "existing.zip")
	if err := os.WriteFile(out, []byte("old file"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Run(context.Background(), []string{server.URL, "-o", out}, strings.NewReader("yes\n"), &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	body, _ := os.ReadFile(out)
	if string(body) != "new file" || !strings.Contains(output.String(), "Overwrite? [y/N]") {
		t.Fatalf("overwrite: %q %s", body, &output)
	}
}
