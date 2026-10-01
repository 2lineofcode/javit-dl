package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testOptions() options {
	return options{concurrency: 3, timeout: 5 * time.Second, userAgent: "test", auto: true}
}

func TestCLIValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-u", "file:///etc/passwd"}, {"-u", "https://example.com", "-c", "0"},
		{"-u", "https://example.com", "-r", "-1"}, {"-u", "https://example.com", "-timeout", "0s"},
		{"-u", "https://example.com", "-o", ""}, {"-u", "https://user:pass@example.com"},
		{"-u", "https://example.com", "-referer", "bad\r\nheader"}, {"serve"},
	} {
		if err := run(context.Background(), args, strings.NewReader(""), io.Discard, io.Discard); err == nil {
			t.Errorf("expected invalid arguments: %q", args)
		}
	}
	if err := run(context.Background(), []string{"-h"}, nil, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestInteractiveCancellation(t *testing.T) {
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readChoice(ctx, bufio.NewReader(input)); !errors.Is(err, context.Canceled) {
		t.Fatalf("prompt did not cancel: %v", err)
	}
}

func TestPublishDoesNotOverwrite(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output")
	err := publish(out, func(f *os.File) error {
		if _, err := f.WriteString("new download"); err != nil {
			return err
		}
		return os.WriteFile(out, []byte("another process"), 0600)
	})
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected collision: %v", err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "another process" {
		t.Fatalf("clobbered destination: %q", data)
	}
	files, _ := os.ReadDir(filepath.Dir(out))
	if len(files) != 1 {
		t.Fatalf("partial files were not cleaned: %v", files)
	}
}

func TestRawDownload(t *testing.T) {
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
	err := run(context.Background(), []string{"download", "--url", server.URL, "--out", out, "--rawurl", "--retries", "1", "--user-agent", "test", "--referer", "https://example.com/"}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "complete" || attempts.Load() != 2 {
		t.Fatalf("bad retry output: %q, attempts %d", data, attempts.Load())
	}
	if err := run(context.Background(), []string{"-u", server.URL, "-o", out, "-rawurl"}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("existing output accepted")
	}
	if attempts.Load() != 2 {
		t.Fatal("existing output should fail before HTTP request")
	}
}

func TestRawCancellationAndFailure(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Length", "100")
		io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	o := testOptions()
	d := newDownloader(o, io.Discard)
	defer d.client.CloseIdleConnections()
	out := filepath.Join(t.TempDir(), "output")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.url = server.URL
	result := make(chan error, 1)
	go func() { result <- publish(out, func(f *os.File) error { return d.fetchRaw(ctx, f) }) }()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation: %v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("partial output published")
	}
	d.url = server.URL + "/missing"
	if err := publish(out, func(f *os.File) error { return d.fetchRaw(context.Background(), f) }); err == nil {
		t.Fatal("404 accepted")
	}
	files, _ := os.ReadDir(filepath.Dir(out))
	if len(files) != 0 {
		t.Fatal("failed raw download left staging files")
	}
}

func TestPlaylistParsing(t *testing.T) {
	base, _ := parseHTTPURL("https://example.com/path/list.m3u8?token=parent")
	body := "#EXTM3U\n#EXT-X-MEDIA-SEQUENCE:42\n#EXT-X-KEY:METHOD=AES-128,URI=\"key?k=a,b\"\n#EXTINF:1,\na.ts?token=segment\n#EXT-X-KEY:METHOD=NONE\n#EXTINF:1,\nb.ts\n#EXT-X-ENDLIST\n"
	lines, err := playlistLines(body)
	if err != nil {
		t.Fatal(err)
	}
	p, err := localPlaylist(lines, base)
	if err != nil {
		t.Fatal(err)
	}
	if p.segments != 2 || len(p.assets) != 3 {
		t.Fatalf("bad resources: %+v", p)
	}
	if p.assets[0].uri != "https://example.com/path/key?k=a,b" || p.assets[1].uri != "https://example.com/path/a.ts?token=segment" || p.assets[2].uri != "https://example.com/path/b.ts" {
		t.Fatalf("bad URL resolution: %+v", p.assets)
	}
	if !strings.Contains(p.text, "#EXT-X-MEDIA-SEQUENCE:42") || !strings.Contains(p.text, "METHOD=NONE") {
		t.Fatal("encryption state lost")
	}
	for _, body := range []string{
		"<html>error</html>", "#EXTM3U\n#EXTINF:1,\na.ts\n", // incomplete/live
		"#EXTM3U\n#EXT-X-KEY:METHOD=SAMPLE-AES,URI=\"key\"\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST",
		"#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key\",IV=0xgarbage\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST",
		"#EXTM3U\n#EXT-X-MAP:URI=\"file:///private/file\"\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST",
		"#EXTM3U\n#EXT-X-GAP\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST",
		"#EXTM3U\n#EXT-X-BYTERANGE:4\n#EXTINF:1,\na.ts\n#EXT-X-ENDLIST",
	} {
		lines, err := playlistLines(body)
		if err == nil {
			_, err = localPlaylist(lines, base)
		}
		if err == nil {
			t.Errorf("invalid playlist accepted: %q", body)
		}
	}
	attrs, err := parseAttributes(`BANDWIDTH=1200,CODECS="avc1.4d401f,mp4a.40.2"`)
	if err != nil || attrs["CODECS"] != "avc1.4d401f,mp4a.40.2" {
		t.Fatal(attrs, err)
	}
	if _, err := parseAttributes(`URI="unterminated`); err == nil {
		t.Fatal("unterminated quote accepted")
	}
}

func TestByteRangesAndResume(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Range", strings.Replace(r.Header.Get("Range"), "bytes=", "bytes ", 1)+"/8")
		w.WriteHeader(http.StatusPartialContent)
		if r.Header.Get("Range") == "bytes=0-3" {
			io.WriteString(w, "init")
		} else {
			io.WriteString(w, "data")
		}
	}))
	defer server.Close()
	base, _ := parseHTTPURL(server.URL + "/list.m3u8")
	lines, _ := playlistLines("#EXTM3U\n#EXT-X-MAP:URI=\"file.mp4\",BYTERANGE=\"4@0\"\n#EXT-X-BYTERANGE:4@0\n#EXTINF:1,\nfile.mp4\n#EXT-X-BYTERANGE:4\n#EXTINF:1,\nfile.mp4\n#EXT-X-ENDLIST")
	p, err := localPlaylist(lines, base)
	if err != nil {
		t.Fatal(err)
	}
	if p.assets[2].offset != 4 || strings.Contains(p.text, "BYTERANGE") {
		t.Fatalf("bad byte ranges: %+v", p)
	}
	d := newDownloader(testOptions(), io.Discard)
	defer d.client.CloseIdleConnections()
	dir := t.TempDir()
	if err := d.downloadAssets(context.Background(), p.assets, dir); err != nil {
		t.Fatal(err)
	}
	if err := d.downloadAssets(context.Background(), p.assets, dir); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("cached resources redownloaded: %d", attempts.Load())
	}
	data, _ := os.ReadFile(filepath.Join(dir, p.assets[2].name))
	if string(data) != "data" {
		t.Fatalf("wrong range data: %q", data)
	}
}

func TestHLSFailureKeepsResumeData(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	var goodRequests atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/list.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ngood.ts\n#EXTINF:1,\nbad.ts\n#EXT-X-ENDLIST")
		case "/good.ts":
			goodRequests.Add(1)
			io.WriteString(w, "complete segment")
		case "/bad.ts":
			if fail.Load() {
				http.Error(w, "missing", 404)
			} else {
				io.WriteString(w, "not actual media")
			}
		}
	}))
	defer server.Close()
	o := testOptions()
	o.concurrency = 1
	o.url, o.out, o.temp = server.URL+"/list.m3u8", filepath.Join(t.TempDir(), "output.mp4"), t.TempDir()
	sentinel := filepath.Join(o.temp, "unrelated.txt")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	d := newDownloader(o, io.Discard)
	defer d.client.CloseIdleConnections()
	if err := d.downloadHLS(context.Background(), nil, io.Discard); err == nil {
		t.Fatal("missing segment accepted")
	}
	fail.Store(false)
	if err := d.downloadHLS(context.Background(), nil, io.Discard); err == nil {
		t.Fatal("invalid media accepted")
	}
	if goodRequests.Load() != 1 {
		t.Fatal("successful segment was not resumed")
	}
	if _, err := os.Stat(o.out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete MP4 published")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("unrelated temp data deleted")
	}
}

// Real local HTTP + FFmpeg checks; no external media services are contacted.
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
					io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=100\n../missing.m3u8\n#EXT-X-STREAM-INF:BANDWIDTH=200\n../list.m3u8\n")
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
			cliArgs := []string{"-u", server.URL + "/redirect", "-o", out, "-tf", temp}
			if mode != "ts" {
				cliArgs = append(cliArgs, "-auto")
			}
			err = run(context.Background(), cliArgs, strings.NewReader("1\n"), io.Discard, &stderr)
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

func TestInvalidResourcesAreNotCached(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/key":
			io.WriteString(w, "bad key")
		case "/range":
			io.WriteString(w, "server ignores ranges")
		case "/cipher":
			io.WriteString(w, "truncated CBC")
		case "/empty":
		}
	}))
	defer server.Close()
	d := newDownloader(testOptions(), io.Discard)
	defer d.client.CloseIdleConnections()
	dir := t.TempDir()
	for _, a := range []asset{
		{uri: server.URL + "/key", name: "key", key: true},
		{uri: server.URL + "/range", name: "range", length: 4},
		{uri: server.URL + "/cipher", name: "cipher", encrypted: true},
		{uri: server.URL + "/empty", name: "empty"},
	} {
		if err := d.downloadAsset(context.Background(), a, dir); err == nil {
			t.Errorf("invalid resource accepted: %s", a.name)
		}
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 0 {
		t.Fatalf("invalid resources cached: %v", files)
	}
}

func TestWorkerConcurrencyAndTransientRetry(t *testing.T) {
	var active, maximum, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/retry" && requests.Add(1) == 1 {
			http.Error(w, "retry", 503)
			return
		}
		current := active.Add(1)
		for old := maximum.Load(); current > old && !maximum.CompareAndSwap(old, current); old = maximum.Load() {
		}
		defer active.Add(-1)
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, "segment")
	}))
	defer server.Close()
	o := testOptions()
	o.concurrency, o.retries = 2, 1
	d := newDownloader(o, io.Discard)
	defer d.client.CloseIdleConnections()
	var assets []asset
	for i := range 10 {
		assets = append(assets, asset{uri: server.URL + "/segment", name: fmt.Sprintf("%d", i)})
	}
	assets = append(assets, asset{uri: server.URL + "/retry", name: "retry"})
	if err := d.downloadAssets(context.Background(), assets, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("wrong concurrency: %d", maximum.Load())
	}
	if requests.Load() != 2 {
		t.Fatalf("503 retry count: %d", requests.Load())
	}
}
