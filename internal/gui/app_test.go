package gui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kuraku-downloader/internal/cli"
	"kuraku-downloader/internal/downloader"
)

func waitJob(t *testing.T, a *App) State {
	t.Helper()
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.Cancel()
		t.Fatal("job did not stop")
	}
	return a.GetState()
}

func TestJobLifecycle(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "download payload") }))
	defer server.Close()
	a := newApp(context.Background())
	r, err := a.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	r.URL = server.URL
	r.Out = filepath.Join(t.TempDir(), "download.bin")
	r.Mode = "file"
	if err := a.Start(r); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, a); s.Status != "Completed" || s.Error != "" {
		t.Fatalf("state: %+v", s)
	}
	payload, err := os.ReadFile(r.Out)
	if err != nil || string(payload) != "download payload" {
		t.Fatalf("payload=%q err=%v", payload, err)
	}
	if err := a.Start(r); err != nil {
		t.Fatal(err)
	}
	if s := waitJob(t, a); s.Status != "Failed" || !strings.Contains(s.Error, "already exists") {
		t.Fatalf("state: %+v", s)
	}
	payload, _ = os.ReadFile(r.Out)
	if string(payload) != "download payload" {
		t.Fatal("existing output changed")
	}
	r.Mode = "invalid"
	if a.Start(r) == nil {
		t.Fatal("accepted invalid mode")
	}
}

func TestCancellationAndSingleJob(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	a := newApp(context.Background())
	r, _ := a.Defaults()
	r.URL = server.URL
	r.Out = filepath.Join(t.TempDir(), "cancel.bin")
	r.Mode = "file"
	if err := a.Start(r); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request not started")
	}
	if a.Start(r) == nil {
		t.Fatal("accepted concurrent job")
	}
	a.Cancel()
	if s := waitJob(t, a); s.Status != "Cancelled" {
		t.Fatalf("state: %+v", s)
	}
	if _, err := os.Stat(r.Out); !os.IsNotExist(err) {
		t.Fatalf("partial output published: %v", err)
	}
}

func TestSelectionAndBoundedState(t *testing.T) {
	a := newApp(context.Background())
	a.state.Active = true
	a.variant = make(chan int, 1)
	a.input = make(chan string, 1)
	a.setChoices("quality", []Choice{{Index: 0, Label: "Quality 1"}})
	s := a.GetState()
	s.Choices[0].Label = "mutated"
	if a.GetState().Choices[0].Label == "mutated" {
		t.Fatal("state aliases internal choices")
	}
	if a.Select(s.ChoiceID+1, 0) == nil || a.Select(s.ChoiceID, 99) == nil {
		t.Fatal("accepted stale/invalid choice")
	}
	if err := a.Select(s.ChoiceID, 0); err != nil {
		t.Fatal(err)
	}
	if index := <-a.variant; index != 0 {
		t.Fatal(index)
	}
	if a.Select(s.ChoiceID, 0) == nil {
		t.Fatal("accepted duplicate selection")
	}
	a.setChoices("media", []Choice{{Index: 2, Label: "FILE"}, {Index: 3, Disabled: true}})
	s = a.GetState()
	if a.Select(s.ChoiceID, 3) == nil {
		t.Fatal("accepted unsupported media")
	}
	if err := a.Select(s.ChoiceID, 2); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reader := &downloader.LineReader{Context: ctx, Lines: a.input}
	p := make([]byte, 10)
	n, err := reader.Read(p)
	if err != nil || string(p[:n]) != "2\n" {
		t.Fatalf("input=%q err=%v", p[:n], err)
	}
	cancel()
	if _, err := reader.Read(p); err != context.Canceled {
		t.Fatalf("cancel read: %v", err)
	}
	for i := 0; i < 1000; i++ {
		fmt.Fprintln(a, "activity")
	}
	fmt.Fprint(a, "\rDownloading: 50% (5/10) · 5.0 MiB · 1.2 MiB/s\n")
	s = a.GetState()
	if len(s.Logs) != 120 || s.Progress != "Downloading: 50% (5/10) · 5.0 MiB · 1.2 MiB/s" {
		t.Fatalf("unbounded state: logs=%d progress=%q", len(s.Logs), s.Progress)
	}
	fmt.Fprint(a, "Preparing download\n\r[download] 25.0% · 2.0 MiB/s · ETA 00:03\n")
	s = a.GetState()
	if s.Progress != "[download] 25.0% · 2.0 MiB/s · ETA 00:03" || s.Logs[len(s.Logs)-1] != "Preparing download" {
		t.Fatalf("coalesced YouTube output lost progress: %+v", s)
	}
}

func TestHLSQualityCancellation(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg required")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1500000\nquality.m3u8\n")
	}))
	defer server.Close()
	a := newApp(context.Background())
	r, _ := a.Defaults()
	r.URL = server.URL
	r.Out = filepath.Join(t.TempDir(), "video.mp4")
	r.Mode = "hls"
	r.Auto = false
	if err := a.Start(r); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for a.GetState().ChoiceKind != "quality" {
		select {
		case <-deadline:
			a.Cancel()
			t.Fatal("quality callback not reached")
		case <-time.After(10 * time.Millisecond):
		}
	}
	s := a.GetState()
	if len(s.Choices) != 1 || !strings.Contains(s.Choices[0].Label, "1.50 Mbps") {
		t.Fatalf("choices=%+v", s.Choices)
	}
	a.shutdown()
	if s := a.GetState(); s.Status != "Cancelled" {
		t.Fatalf("state=%+v", s)
	}
}

func TestCLIFormDefaults(t *testing.T) {
	a := newApp(context.Background())
	defaults, err := a.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	err = cli.RunWithGUI(context.Background(), []string{"gui", "-u", "http://example.com/watch", "--startdownload", "--headless", "--allow-popups", "-r", "0", "--auto=false", "--timeout", "1m"}, strings.NewReader(""), io.Discard, io.Discard, func(_ context.Context, initial map[string]any) error { a.initial = initial; return nil })
	if err != nil {
		t.Fatal(err)
	}
	r, err := a.Defaults()
	if err != nil {
		t.Fatal(err)
	}
	if !r.StartDownload || r.URL != "http://example.com/watch" || r.Mode != "extract" || !r.Headless || !r.AllowPopups || r.Retries != 0 || r.Auto || r.Timeout != 60 || r.Out != defaults.Out || r.Temp != defaults.Temp || r.CaptureDuration != defaults.CaptureDuration || r.UserAgent != "" {
		t.Fatalf("defaults=%+v", r)
	}
	if a.GetState().Active {
		t.Fatal("prefill started a download")
	}
}

func TestExtractFilenameDefaults(t *testing.T) {
	a := newApp(context.Background())
	r, err := a.Defaults()
	if err != nil || !r.AutoFilename {
		t.Fatalf("automatic filename should be enabled: %+v %v", r, err)
	}
	a.initial = map[string]any{"out": "custom.mp4", "mode": "extract"}
	r, err = a.Defaults()
	if err != nil || r.AutoFilename || r.Out != "custom.mp4" {
		t.Fatalf("explicit output must be preserved: %+v %v", r, err)
	}
}

func TestMediaChoicesKeepAllCandidates(t *testing.T) {
	a := newApp(context.Background())
	a.state.Active = true
	a.input = make(chan string, 1)
	media := make([]downloader.Media, 401)
	for i := range media {
		media[i] = downloader.Media{Index: i + 1, URL: fmt.Sprintf("https://example.com/%d.mp4", i), Kind: "FILE"}
	}
	a.setMedia(media)
	state := a.GetState()
	if len(state.Choices) != 401 || state.Choices[0].Index != 1 || state.Choices[400].Index != 401 {
		t.Fatalf("candidates dropped: %+v", state.Choices)
	}
	if err := a.Select(state.ChoiceID, 1); err != nil {
		t.Fatal(err)
	}
	if got := <-a.input; got != "1\n" {
		t.Fatal(got)
	}
	a.setMedia(media)
	if len(a.GetState().Choices) != 0 {
		t.Fatal("selected media reappeared")
	}
}
