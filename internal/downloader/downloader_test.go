package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testOptions() Options {
	return Options{Concurrency: 3, Timeout: 5 * time.Second, UserAgent: "test", Auto: true}
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
	d.URL = server.URL
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
	d.URL = server.URL + "/missing"
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
		t.Fatalf("bad Downloading: %+v", p)
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
	o.Concurrency = 1
	o.URL, o.Out, o.Temp = server.URL+"/list.m3u8", filepath.Join(t.TempDir(), "output.mp4"), t.TempDir()
	sentinel := filepath.Join(o.Temp, "unrelated.txt")
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
	if _, err := os.Stat(o.Out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("incomplete MP4 published")
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatal("unrelated temp data deleted")
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
	o.Concurrency, o.Retries = 2, 1
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
		t.Fatalf("wrong Concurrency: %d", maximum.Load())
	}
	if requests.Load() != 2 {
		t.Fatalf("503 retry count: %d", requests.Load())
	}
}
