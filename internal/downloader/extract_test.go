package downloader

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
	c.add(mediaURL{URL: "https://cdn.test/master.m3u8?a=1", Kind: "HLS", Referer: "old"})
	c.add(mediaURL{URL: "https://cdn.test/master.m3u8?a=1", Kind: "HLS", Referer: "new"})
	c.add(mediaURL{URL: "https://cdn.test/master.m3u8?a=2", Kind: "HLS", Referer: "other"})
	if got := c.snapshot(); len(got) != 2 || got[0].Referer != "new" {
		t.Fatalf("dedupe changed signed URLs or lost headers: %+v", got)
	}
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.add(mediaURL{URL: "https://cdn.test/late.mp4", Kind: "FILE", Referer: "late"})
	if len(c.snapshot()) != 2 {
		t.Fatal("capture changed after deadline")
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
				select {
				case <-chromedp.FromContext(browser).Browser.LostConnection:
				default:
					t.Error("browser still open when download started")
				}
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
		go func() { fmt.Fprintln(writer, "invalid"); fmt.Fprintln(writer, "2") }()
	}}
	var candidates []Media
	err := extractSession(ctx, browser, Options{URL: server.URL + "/", Out: out, Temp: t.TempDir(), Concurrency: 2, Timeout: 10 * time.Second}, ExtractOptions{OnMedia: func(media []Media) { candidates = media }}, input, observed, &stderr)
	if err != nil {
		t.Fatalf("extract: %v; %s", err, &stderr)
	}
	if len(candidates) != 2 || candidates[0].Index != 1 || candidates[0].Kind != "HLS" || candidates[1].Kind != "FILE" || !strings.Contains(candidates[1].URL, "token=a%2Fb") {
		t.Fatalf("GUI candidates: %+v", candidates)
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
	match       string
	once        bool
}

func (w *captureOutput) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	match := w.match
	if match == "" {
		match = "[2] [FILE]"
	}
	if !w.once && bytes.Contains(p, []byte(match)) {
		w.once = true
		w.selectMedia()
	}
	return n, err
}

func TestExtractHeadless(t *testing.T) {
	if os.Getenv("JAVIT_DL_BROWSER_TEST") == "" {
		t.Skip("set JAVIT_DL_BROWSER_TEST=1 to run Chrome extraction")
	}
	browserPath := isolateExtractProfile(t)
	for _, tc := range []struct {
		name, html, selector string
		wantMedia            bool
	}{
		{"native-play", `<video></video><script>document.querySelector('video').play=()=>{fetch('/movie.mp4?token=a%2Fb');return Promise.resolve()}</script>`, "", true},
		{"delayed-button", `<script>setTimeout(()=>{const b=document.createElement('button');b.id='play';b.onclick=()=>fetch('/movie.mp4?token=a%2Fb');document.body.append(b)},200)</script>`, "#play", true},
		{"empty-page", `<html><body>No video</body></html>`, "", false},
		{"missing-button", `<html><body>No button</body></html>`, "#missing", false},
		{"challenge-page", `<title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/test.js"></script>Performing security verification`, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var loaded atomic.Int64
			var requests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/" {
					loaded.Store(time.Now().UnixNano())
					http.SetCookie(w, &http.Cookie{Name: "session", Value: "test", Path: "/", HttpOnly: true})
					w.Header().Set("Content-Type", "text/html")
					if tc.name == "challenge-page" {
						w.WriteHeader(http.StatusForbidden)
					}
					io.WriteString(w, "<!doctype html><html><body>"+tc.html+"</body></html>")
					return
				}
				if r.URL.Path != "/movie.mp4" {
					http.NotFound(w, r)
					return
				}
				requests.Add(1)
				cookie, err := r.Cookie("session")
				if err != nil || cookie.Value != "test" || r.Referer() != server.URL+"/" || !strings.Contains(r.UserAgent(), "Chrome/") || r.URL.RawQuery != "token=a%2Fb" {
					t.Errorf("headless session not preserved: cookie=%v err=%v headers=%v query=%s", cookie, err, r.Header, r.URL.RawQuery)
				}
				w.Header().Set("Content-Type", "video/mp4")
				io.WriteString(w, "headless media")
			}))
			defer server.Close()
			out := filepath.Join(t.TempDir(), "movie.mp4")
			var output, stderr bytes.Buffer
			o := Options{URL: server.URL + "/", Out: out, Temp: t.TempDir(), Concurrency: 2, Timeout: 15 * time.Second}
			x := ExtractOptions{Browser: browserPath, Headless: true, CaptureDuration: time.Second, PlaySelector: tc.selector}
			var err error
			if !tc.wantMedia {
				alloc, stop := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
				defer stop()
				browser, closeBrowser := chromedp.NewContext(alloc)
				defer closeBrowser()
				err = extractSession(ctx, browser, o, x, strings.NewReader("1\n"), &output, &stderr)
			} else {
				err = Extract(ctx, o, x, strings.NewReader("1\n"), &output, &stderr)
			}
			if time.Since(time.Unix(0, loaded.Load())) < time.Second {
				t.Fatal("selection/download occurred before capture completed")
			}
			if !tc.wantMedia {
				want := "no downloadable media captured"
				if tc.name == "challenge-page" {
					want = errExtractionVerification.Error()
				}
				if err == nil || !strings.Contains(err.Error(), want) || (tc.name != "challenge-page" && !strings.Contains(err.Error(), "--headless")) {
					t.Fatalf("missing fallback guidance: %v", err)
				}
				if tc.name == "challenge-page" && !errors.Is(err, errExtractionVerification) {
					t.Fatal("verification error cannot trigger fallback")
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("empty capture published output")
				}
				return
			}
			if err != nil {
				t.Fatalf("headless: %v\n%s\n%s", err, &output, &stderr)
			}
			body, err := os.ReadFile(out)
			if err != nil || string(body) != "headless media" || requests.Load() < 2 {
				t.Fatalf("headless publication: %q, %v, requests=%d", body, err, requests.Load())
			}
			if !strings.Contains(output.String(), "Capture complete") || !strings.Contains(output.String(), "[1] [FILE]") || strings.Contains(output.String(), "[2]") {
				t.Fatalf("bad capture results: %s", &output)
			}
		})
	}
}

// Local verification handoff only: this does not solve a real Cloudflare challenge.
func TestExtractSavedSession(t *testing.T) {
	if os.Getenv("JAVIT_DL_BROWSER_TEST") == "" {
		t.Skip("set JAVIT_DL_BROWSER_TEST=1 to run Chrome extraction")
	}
	browserPath := isolateExtractProfile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var visibleLoads, savedLoads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/movie.mp4" {
			cookie, err := r.Cookie("verified")
			if err != nil || cookie.Value != "yes" {
				t.Error("download lost verified session cookie")
			}
			w.Header().Set("Content-Type", "video/mp4")
			io.WriteString(w, "saved session media")
			return
		}
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if cookie, err := r.Cookie("verified"); err == nil && cookie.Value == "yes" {
			savedLoads.Add(1)
			io.WriteString(w, `<video></video><script>document.querySelector('video').play=()=>{fetch('/movie.mp4');return Promise.resolve()}</script>`)
			return
		}
		if strings.Contains(r.UserAgent(), "HeadlessChrome") {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `<title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/test.js"></script>`)
			return
		}
		visibleLoads.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "verified", Value: "yes", Path: "/", MaxAge: 3600, HttpOnly: true})
		io.WriteString(w, `<script>setTimeout(()=>fetch('/movie.mp4'),200)</script>`)
	}))
	defer server.Close()
	for run := 0; run < 2; run++ {
		out := filepath.Join(t.TempDir(), "movie.mp4")
		input, writer := io.Pipe()
		output := &captureOutput{match: "[1] [FILE]", selectMedia: func() { go func() { fmt.Fprintln(writer, "1") }() }}
		var stderr bytes.Buffer
		err := Extract(ctx, Options{URL: server.URL + "/", Out: out, Temp: t.TempDir(), Concurrency: 2, Timeout: 15 * time.Second}, ExtractOptions{Browser: browserPath, Headless: true, CaptureDuration: time.Second}, input, output, &stderr)
		input.Close()
		writer.Close()
		if err != nil {
			t.Fatalf("run %d: %v\n%s\n%s", run, err, output.String(), &stderr)
		}
		body, err := os.ReadFile(out)
		if err != nil || string(body) != "saved session media" {
			t.Fatalf("run %d output: %q, %v", run, body, err)
		}
		fallback := strings.Contains(output.String(), "Verification required. Opening the browser")
		if fallback != (run == 0) {
			t.Fatalf("run %d unexpected fallback: %s", run, output.String())
		}
	}
	if visibleLoads.Load() != 1 || savedLoads.Load() != 1 {
		t.Fatalf("profile was not reused: visible=%d saved=%d", visibleLoads.Load(), savedLoads.Load())
	}
}

func TestExtractHeadlessVisibleRetry(t *testing.T) {
	if os.Getenv("JAVIT_DL_BROWSER_TEST") == "" {
		t.Skip("set JAVIT_DL_BROWSER_TEST=1 to run Chrome extraction")
	}
	browserPath := isolateExtractProfile(t)
	for _, mode := range []string{"download-403", "empty-capture", "quit"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			var visibleLoads, rejected atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				headless := strings.Contains(r.UserAgent(), "HeadlessChrome")
				switch r.URL.Path {
				case "/":
					w.Header().Set("Content-Type", "text/html")
					if !headless {
						visibleLoads.Add(1)
					}
					if headless && mode == "empty-capture" {
						io.WriteString(w, "<html>No player</html>")
						return
					}
					io.WriteString(w, `<video></video><script>const play=()=>{fetch('/movie.mp4');return Promise.resolve()};document.querySelector('video').play=play;if(!navigator.userAgent.includes('HeadlessChrome'))setTimeout(play,200)</script>`)
				case "/movie.mp4":
					if headless && r.Header.Get("Sec-Fetch-Mode") == "" {
						rejected.Add(1)
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "video/mp4")
					io.WriteString(w, "visible retry media")
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			input, writer := io.Pipe()
			defer input.Close()
			defer writer.Close()
			output := &captureOutput{match: "[1] [FILE]"}
			output.selectMedia = func() {
				output.once = false // Select freshly captured media in both browser runs.
				go func() {
					if mode == "quit" {
						fmt.Fprintln(writer, "q")
						return
					}
					fmt.Fprint(writer, "1\nlist\n")
				}()
			}
			out := filepath.Join(t.TempDir(), "movie.mp4")
			var stderr bytes.Buffer
			err := Extract(ctx, Options{URL: server.URL + "/", Out: out, Temp: t.TempDir(), Concurrency: 2, Timeout: 10 * time.Second}, ExtractOptions{Browser: browserPath, Headless: true, CaptureDuration: time.Second}, input, output, &stderr)
			if err != nil {
				t.Fatalf("%v\n%s\n%s", err, output.String(), &stderr)
			}
			if mode == "quit" {
				if visibleLoads.Load() != 0 || strings.Contains(output.String(), "Opening the browser") {
					t.Fatal("quit reopened browser")
				}
				if _, err := os.Stat(out); !os.IsNotExist(err) {
					t.Fatal("quit published output")
				}
				return
			}
			body, err := os.ReadFile(out)
			if err != nil || string(body) != "visible retry media" || visibleLoads.Load() != 1 || strings.Count(output.String(), "Opening the browser") != 1 {
				t.Fatalf("handoff: %q %v loads=%d\n%s", body, err, visibleLoads.Load(), output.String())
			}
			if mode == "download-403" && rejected.Load() != 1 {
				t.Fatalf("headless did not hand off immediately: %d requests rejected", rejected.Load())
			}
		})
	}
}

// Isolate app data while preserving Chrome's real macOS home/keychain environment.
func isolateExtractProfile(t *testing.T) string {
	t.Helper()
	chromeHome := os.Getenv("HOME")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	if runtime.GOOS != "darwin" {
		return ""
	}
	chrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if _, err := os.Stat(chrome); err != nil {
		t.Skip("Google Chrome unavailable")
	}
	wrapper := filepath.Join(t.TempDir(), "chrome")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	script := "#!/bin/sh\nexport HOME=" + quote(chromeHome) + "\nexec " + quote(chrome) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return wrapper
}
