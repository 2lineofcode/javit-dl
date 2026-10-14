package downloader

import (
	"bytes"
	"context"
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

	"github.com/chromedp/chromedp"
)

func TestExtractWorkerPlayback(t *testing.T) {
	if os.Getenv("KURAKU_BROWSER_TEST") == "" {
		t.Skip("set KURAKU_BROWSER_TEST=1 to run Chrome playback")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	file := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-f", "lavfi", "-i", "color=c=blue:s=160x90:r=24", "-t", "3", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-movflags", "frag_keyframe+empty_moov+default_base_moof", file)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate clip: %v %s", err, output)
	}
	var playing atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/clip.mp4":
			http.ServeFile(w, r, file)
		case "/playing":
			playing.Store(true)
		case "/worker.js":
			w.Header().Set("Content-Type", "application/javascript")
			io.WriteString(w, `fetch('/clip.mp4').then(r=>r.arrayBuffer()).then(data=>postMessage(data,[data]))`)
		default:
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, `<video muted></video><script>
const video=document.querySelector('video'), source=new MediaSource();video.src=URL.createObjectURL(source);
source.addEventListener('sourceopen',()=>{const buffer=source.addSourceBuffer('video/mp4; codecs="avc1.64000a"');const worker=new Worker('/worker.js');worker.onmessage=e=>buffer.appendBuffer(e.data);buffer.addEventListener('updateend',()=>{source.endOfStream();video.play().catch(e=>console.error(e));});});
setInterval(()=>{if(video.currentTime>0.5)fetch('/playing')},100);
</script>`)
		}
	}))
	defer server.Close()
	alloc, stop := chromedp.NewExecAllocator(ctx, chromedp.DefaultExecAllocatorOptions[:]...)
	defer stop()
	browser, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	var output, stderr bytes.Buffer
	err := extractSession(ctx, browser, Options{URL: server.URL, Out: filepath.Join(t.TempDir(), "out.mp4"), Timeout: 10 * time.Second}, ExtractOptions{Headless: true, CaptureDuration: 5 * time.Second}, strings.NewReader("q\n"), &output, &stderr)
	if err != nil || !playing.Load() {
		t.Fatalf("decoded playback=%v error=%v\n%s\n%s", playing.Load(), err, &output, &stderr)
	}
}
