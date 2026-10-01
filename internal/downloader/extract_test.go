package downloader

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func TestMediaCapture(t *testing.T) {
	for _, tc := range []struct {
		uri, mime, want string
		resource        network.ResourceType
	}{
		{"https://cdn.test/master.M3U8?token=a%2Fb", "text/plain", "HLS", network.ResourceTypeFetch},
		{"https://cdn.test/api/play", "application/vnd.apple.mpegurl; charset=utf-8", "HLS", network.ResourceTypeFetch},
		{"https://cdn.test/play", "application/x-mpegURL", "HLS", network.ResourceTypeXHR},
		{"https://cdn.test/movie.mp4?token=x", "", "FILE", network.ResourceTypeMedia},
		{"https://cdn.test/play", "video/mp4", "FILE", network.ResourceTypeMedia},
		{"https://cdn.test/master.mpd", "", "DASH (unsupported)", network.ResourceTypeFetch},
		{"https://cdn.test/seg.ts", "video/mp2t", "", network.ResourceTypeMedia},
		{"https://cdn.test/index.html", "text/html", "", network.ResourceTypeDocument},
		{"blob:https://cdn.test/id", "video/mp4", "", network.ResourceTypeMedia},
		{"https://user:pass@cdn.test/master.m3u8", "", "", network.ResourceTypeFetch},
	} {
		if got := mediaKind(tc.uri, tc.mime, tc.resource); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.uri, got, tc.want)
		}
	}
	c := newMediaCapture()
	c.add(mediaURL{"https://cdn.test/master.m3u8?a=1", "HLS", "old"})
	c.add(mediaURL{"https://cdn.test/master.m3u8?a=1", "HLS", "new"})
	c.add(mediaURL{"https://cdn.test/master.m3u8?a=2", "HLS", "other"})
	if got := c.snapshot(); len(got) != 2 || got[0].Referer != "new" {
		t.Fatalf("dedupe changed signed URLs or lost headers: %+v", got)
	}
	r := &extractInput{ctx: context.Background(), lines: func() <-chan string { ch := make(chan string, 2); ch <- "0"; ch <- "1"; close(ch); return ch }()}
	got, err := io.ReadAll(r)
	if err != nil || string(got) != "0\n1\n" {
		t.Fatalf("variant input: %q, %v", got, err)
	}
}

// Opt in to the real-browser regression; it uses only a local server and headless Chrome.
func TestExtractBrowser(t *testing.T) {
	if os.Getenv("JAVIT_DL_BROWSER_TEST") == "" {
		t.Skip("set JAVIT_DL_BROWSER_TEST=1 to run Chrome extraction")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	alloc, stop := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stop()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	var selected atomic.Bool
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			http.SetCookie(w, &http.Cookie{Name: "session", Value: "test", Path: "/", HttpOnly: true})
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<script>setTimeout(()=>fetch('/api/playlist?token=a%2Fb'),100);setTimeout(()=>fetch('/api/playlist?token=a%2Fb'),300);setTimeout(()=>fetch('/movie.mp4?token=a%2Fb'),600)</script>`)
		case "/api/playlist":
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			io.WriteString(w, "#EXTM3U\n#EXT-X-ENDLIST\n")
		case "/movie.mp4":
			if selected.Load() {
				cookie, err := r.Cookie("session")
				if err != nil || cookie.Value != "test" || r.Referer() != server.URL+"/" || !strings.Contains(r.UserAgent(), "Chrome/") || r.URL.RawQuery != "token=a%2Fb" {
					t.Errorf("session headers/query lost: cookie=%v err=%v referer=%s UA=%s query=%s", cookie, err, r.Referer(), r.UserAgent(), r.URL.RawQuery)
				}
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "complete media")
		}
	}))
	defer server.Close()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	var stderr bytes.Buffer
	out := filepath.Join(t.TempDir(), "movie.mp4")
	observed := &captureOutput{selectMedia: func() {
		selected.Store(true)
		go func() { fmt.Fprintln(writer, "invalid"); fmt.Fprintln(writer, "1"); fmt.Fprintln(writer, "2") }()
	}}
	err := extractSession(ctx, browser, Options{URL: server.URL + "/", Out: out, Temp: t.TempDir(), Concurrency: 2, Timeout: 10 * time.Second}, input, observed, &stderr)
	if err != nil {
		t.Fatalf("extract: %v; %s", err, &stderr)
	}
	body, err := os.ReadFile(out)
	if err != nil || string(body) != "complete media" {
		t.Fatalf("publication: %q, %v", body, err)
	}
	if !strings.Contains(stderr.String(), "DASH") && !strings.Contains(stderr.String(), "Choose a listed") {
		t.Fatalf("invalid selection not handled: %s", &stderr)
	}
	if !strings.Contains(observed.String(), "[1] [HLS]") || !strings.Contains(observed.String(), "[2] [FILE]") || strings.Contains(observed.String(), "[3]") {
		t.Fatalf("missing or duplicate candidates: %s", observed.String())
	}
}

type captureOutput struct {
	bytes.Buffer
	selectMedia func()
	once        bool
}

func (w *captureOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if !w.once && bytes.Contains(p, []byte("[2] [FILE]")) {
		w.once = true
		w.selectMedia()
	}
	return n, err
}
