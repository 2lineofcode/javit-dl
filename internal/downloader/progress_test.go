package downloader

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestByteProgress(t *testing.T) {
	var output bytes.Buffer
	start := time.Now()
	p := &byteProgress{out: &output, total: 4, last: start}
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() { p.Write(make([]byte, 1024)); p.done.Add(1) })
	}
	workers.Wait()
	p.print(start.Add(time.Second))
	if !strings.Contains(output.String(), "Downloading: 100% (4/4) · 4.0 KiB · 4.0 KiB/s") {
		t.Fatalf("parallel throughput: %q", output.String())
	}
	output.Reset()
	p.print(start.Add(2 * time.Second))
	if !strings.Contains(output.String(), "0.0 B/s") {
		t.Fatalf("idle transfer still shows speed: %q", output.String())
	}
}

func TestDownloadSpeedAndCachedAssets(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		io.WriteString(w, strings.Repeat("x", 1024))
	}))
	defer server.Close()
	var output bytes.Buffer
	o := testOptions()
	o.URL, o.Out, o.Raw = server.URL, filepath.Join(t.TempDir(), "file"), true
	if err := Download(context.Background(), o, nil, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Downloading: 1.0 KiB · ") || !strings.Contains(output.String(), "/s") {
		t.Fatalf("raw file has no speed: %q", output.String())
	}
	d := newDownloader(o, &output)
	defer d.client.CloseIdleConnections()
	assets := []asset{{uri: server.URL + "/a", name: "a"}, {uri: server.URL + "/b", name: "b"}}
	dir := t.TempDir()
	for _, want := range []string{"2.0 KiB", "0.0 B"} {
		output.Reset()
		if err := d.downloadAssets(context.Background(), assets, dir); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "Downloading: 100% (2/2) · "+want+" · ") || !strings.Contains(output.String(), "/s") {
			t.Fatalf("HLS/cache throughput: %q", output.String())
		}
	}
	if requests.Load() != 3 {
		t.Fatalf("cached resources were downloaded again: %d requests", requests.Load())
	}
}
