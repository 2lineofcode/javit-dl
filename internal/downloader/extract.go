package downloader

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/chromedp"
)

// ExtractOptions controls browser capture independently of download options.
type ExtractOptions struct {
	Browser, PlaySelector string
	Headless              bool
	CaptureDuration       time.Duration
	OnMedia               func([]Media)
}

// Media is a captured candidate; Index matches the interactive selection number.
type Media struct {
	Index int    `json:"index"`
	URL   string `json:"url"`
	Kind  string `json:"kind"`
}

var errExtractionVerification = errors.New("Cloudflare security verification requires an interactive browser")

// Extract opens a dedicated browser and watches media requests until one is selected.
func Extract(ctx context.Context, o Options, x ExtractOptions, input io.Reader, output, stderr io.Writer) error {
	u, err := parseHTTPURL(o.URL)
	if err != nil {
		return err
	}
	if x.Headless && x.CaptureDuration <= 0 {
		return errors.New("capture-duration must be positive")
	}
	if !x.Headless && x.PlaySelector != "" {
		return errors.New("play-selector requires --headless")
	}
	if err := validateOptions(o); err != nil {
		return err
	}
	if err := checkOutput(o.Out); err != nil {
		return err
	}
	if input == nil {
		return errors.New("extract requires interactive input")
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("browser session directory: %w", err)
	}
	site := sha256.Sum256([]byte(strings.ToLower(u.Hostname())))
	profile := filepath.Join(configDir, "JavIt-DL", "browser", fmt.Sprintf("%x", site))
	if err := os.MkdirAll(profile, 0700); err != nil {
		return fmt.Errorf("create browser session: %w", err)
	}
	ctx, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	input = &extractionInput{Reader: input, lines: make(chan string), errors: make(chan error, 1)}
	run := func(headless bool) error {
		opts := []chromedp.ExecAllocatorOption{chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck, chromedp.UserDataDir(profile), chromedp.Flag("remote-debugging-address", "127.0.0.1"), chromedp.Flag("no-sandbox", false)}
		if headless {
			opts = append(opts, chromedp.Headless, chromedp.Flag("autoplay-policy", "no-user-gesture-required"))
		}
		if x.Browser != "" {
			opts = append(opts, chromedp.ExecPath(x.Browser))
		}
		if o.UserAgent != "" {
			opts = append(opts, chromedp.UserAgent(o.UserAgent))
		}
		alloc, stop := chromedp.NewExecAllocator(ctx, opts...)
		defer stop()
		browserCtx, closeBrowser := chromedp.NewContext(alloc)
		defer closeBrowser()
		defer func() {
			if browserCtx.Err() == nil && chromedp.FromContext(browserCtx).Browser != nil {
				shutdown, cancel := context.WithTimeout(browserCtx, 5*time.Second)
				defer cancel()
				if err := chromedp.Cancel(shutdown); err != nil {
					fmt.Fprintf(stderr, "Browser session shutdown: %v\n", err)
				}
			}
		}()
		session := x
		session.Headless = headless
		return extractSession(ctx, browserCtx, o, session, input, output, stderr)
	}
	err = run(x.Headless)
	if x.Headless && err != nil && ctx.Err() == nil && !errors.Is(err, context.Canceled) {
		if errors.Is(err, errExtractionVerification) {
			fmt.Fprintln(output, "Verification required. Opening the browser; complete verification and play the video, then choose a media number here. Your browser session is saved automatically.")
		} else {
			fmt.Fprintf(output, "Headless extraction failed: %v\nOpening the browser; play the video and choose a media number here.\n", err)
		}
		return run(false)
	}
	return err
}

type mediaURL struct{ URL, Kind, Referer string }

type mediaCapture struct {
	mu      sync.Mutex
	items   []mediaURL
	seen    map[string]int
	changed chan struct{}
	stopped bool
}

func newMediaCapture() *mediaCapture {
	return &mediaCapture{seen: make(map[string]int), changed: make(chan struct{}, 1)}
}

func (c *mediaCapture) add(item mediaURL) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	if index, exists := c.seen[item.URL]; exists {
		c.items[index] = item // Refresh headers after navigation without changing the displayed index.
	} else {
		c.seen[item.URL] = len(c.items)
		c.items = append(c.items, item)
	}
	c.mu.Unlock()
	select {
	case c.changed <- struct{}{}:
	default:
	}
}

func (c *mediaCapture) snapshot() []mediaURL {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]mediaURL(nil), c.items...)
}

func mediaKind(raw, contentType string, resource network.ResourceType) string {
	u, err := parseHTTPURL(raw)
	if err != nil {
		return ""
	}
	typ, _, _ := mime.ParseMediaType(contentType)
	switch strings.ToLower(typ) {
	case "application/vnd.apple.mpegurl", "application/x-mpegurl", "audio/mpegurl", "audio/x-mpegurl":
		return "HLS"
	case "application/dash+xml":
		return "DASH (unsupported)"
	}
	switch strings.ToLower(filepath.Ext(u.Path)) {
	case ".m3u8":
		return "HLS"
	case ".mpd":
		return "DASH (unsupported)"
	case ".mp4", ".webm", ".mov", ".m4a", ".mp3", ".ogg", ".wav":
		return "FILE"
	}
	if resource == network.ResourceTypeMedia && (strings.HasPrefix(typ, "video/") || strings.HasPrefix(typ, "audio/")) && typ != "video/mp2t" {
		return "FILE"
	}
	return ""
}

func (c *mediaCapture) watch(ctx context.Context) {
	// Request metadata lives only until its response/failure; segment traffic does not accumulate.
	requests := make(map[network.RequestID]string)
	chromedp.ListenTarget(ctx, func(event any) {
		switch e := event.(type) {
		case *network.EventRequestWillBeSent:
			referer := ""
			for key, value := range e.Request.Headers {
				if strings.EqualFold(key, "Referer") {
					referer, _ = value.(string)
				}
			}
			requests[e.RequestID] = referer
		case *network.EventResponseReceived:
			referer := requests[e.RequestID]
			delete(requests, e.RequestID)
			if e.Response.Status < 200 || e.Response.Status >= 300 {
				return
			}
			if kind := mediaKind(e.Response.URL, e.Response.MimeType, e.Type); kind != "" {
				c.add(mediaURL{URL: e.Response.URL, Kind: kind, Referer: referer})
			}
		case *network.EventLoadingFailed:
			delete(requests, e.RequestID)
		}
	})
}

func extractSession(ctx, browserCtx context.Context, o Options, x ExtractOptions, input io.Reader, output, stderr io.Writer) error {
	if x.OnMedia != nil {
		x.OnMedia(nil)
	}
	capture := newMediaCapture()
	capture.watch(browserCtx)
	if err := chromedp.Run(browserCtx, network.Enable(), chromedp.Evaluate(`navigator.userAgent`, &o.UserAgent)); err != nil {
		return fmt.Errorf("start extraction browser (install Chrome/Chromium or use --browser): %w", err)
	}
	startup, cancel := context.WithTimeout(browserCtx, o.Timeout)
	err := chromedp.Run(startup, chromedp.Navigate(o.URL))
	cancel()
	if err != nil {
		return fmt.Errorf("open extraction browser (install Chrome/Chromium or use --browser): %w", err)
	}
	if x.Headless {
		fmt.Fprintf(output, "Capturing media in the background for %s...\n", x.CaptureDuration)
	} else {
		fmt.Fprintln(output, "Play a video or navigate in the extraction browser. HLS playlists appear as [HLS].\nEnter a media number to download, list to show results, or q to quit.")
	}
	stream, ok := input.(*extractionInput)
	if !ok {
		stream = &extractionInput{Reader: input, lines: make(chan string), errors: make(chan error, 1)}
	}
	lines, readErrors := stream.lines, stream.errors
	startInput := func() { stream.start(ctx) }
	if !x.Headless {
		startInput()
	}
	// ponytail: polling may miss a new tab's first requests; reload it if needed, use auto-attach if that matters.
	watched := map[string]context.Context{string(chromedp.FromContext(browserCtx).Target.TargetID): browserCtx}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	displayed := 0
	printItems := func(all bool) {
		items := capture.snapshot()
		if x.OnMedia != nil {
			media := make([]Media, len(items))
			for i, item := range items {
				media[i] = Media{Index: i + 1, URL: item.URL, Kind: item.Kind}
			}
			x.OnMedia(media)
		}
		if all && len(items) == 0 {
			fmt.Fprintln(output, "No media captured yet; navigate or play a video in the browser.")
		}
		start := displayed
		if all {
			start = 0
		}
		for i := start; i < len(items); i++ {
			fmt.Fprintf(output, "[%d] [%s] %s\n", i+1, items[i].Kind, items[i].URL)
		}
		displayed = len(items)
	}
	var captureDone <-chan time.Time
	var playbackCtx context.Context
	var stopPlayback context.CancelFunc
	playbackErrors := make(chan error, 1)
	selectionLines := (<-chan string)(lines)
	if x.Headless {
		timer := time.NewTimer(x.CaptureDuration)
		defer timer.Stop()
		captureDone = timer.C
		selectionLines = nil // Piped selections wait for capture to finish too.
		playbackCtx, stopPlayback = context.WithTimeout(browserCtx, x.CaptureDuration)
		defer stopPlayback()
		if x.PlaySelector != "" {
			go func() { playbackErrors <- chromedp.Run(playbackCtx, chromedp.Click(x.PlaySelector, chromedp.ByQuery)) }()
		} else if err := chromedp.Run(playbackCtx, chromedp.Evaluate(autoPlayMedia, nil)); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
	}
	for {
		select {
		case <-captureDone:
			stopPlayback()
			capture.mu.Lock()
			capture.stopped = true
			capture.mu.Unlock()
			captureDone = nil
			ticker.Stop()
			items := capture.snapshot()
			supported := false
			for _, item := range items {
				if item.Kind == "HLS" || item.Kind == "FILE" {
					supported = true
					break
				}
			}
			if !supported {
				inspectCtx, cancel := context.WithTimeout(browserCtx, o.Timeout)
				var challenge bool
				err := chromedp.Run(inspectCtx, chromedp.Evaluate(`document.title === 'Just a moment...' && (!!document.querySelector('script[src*="/cdn-cgi/challenge-platform/"], iframe[src*="challenges.cloudflare.com"]') || /Cloudflare/.test(document.body.innerText))`, &challenge))
				cancel()
				if err != nil {
					return fmt.Errorf("inspect extraction page: %w", err)
				}
				if challenge {
					return errExtractionVerification
				}
				return errors.New("no downloadable media captured; increase --capture-duration, specify --play-selector, or retry without --headless for login/manual playback")
			}
			printItems(true)
			fmt.Fprintln(output, "Capture complete. Enter a media number to download, list, or q to quit.")
			startInput()
			selectionLines = lines
		case err := <-playbackErrors:
			if err != nil && captureDone != nil {
				fmt.Fprintln(stderr, "Could not click play selector; check --play-selector or use interactive extraction.")
			}
		case <-ctx.Done():
			return ctx.Err()
		case <-browserCtx.Done():
			return errors.New("extraction browser closed")
		case <-ticker.C:
			targets, err := chromedp.Targets(browserCtx)
			if err != nil {
				return fmt.Errorf("watch browser tabs: %w", err)
			}
			for _, info := range targets {
				if (info.Type != "page" && info.Type != "iframe") || watched[string(info.TargetID)] != nil {
					continue
				}
				tab, closeTab := chromedp.NewContext(browserCtx, chromedp.WithTargetID(info.TargetID))
				defer closeTab()
				watched[string(info.TargetID)] = tab
				capture.watch(tab)
				attachCtx, cancel := context.WithTimeout(tab, o.Timeout)
				var stop func() bool
				if x.Headless {
					stop = context.AfterFunc(playbackCtx, cancel)
				}
				err := chromedp.Run(attachCtx, network.Enable())
				if stop != nil {
					stop()
				}
				cancel()
				if err != nil {
					fmt.Fprintf(stderr, "Cannot watch tab: %v\n", err)
				}
			}
			if x.Headless && x.PlaySelector == "" && captureDone != nil {
				for _, tab := range watched {
					attempt, cancel := context.WithTimeout(tab, time.Second)
					stop := context.AfterFunc(playbackCtx, cancel)
					err := chromedp.Run(attempt, chromedp.Evaluate(autoPlayMedia, nil))
					stop()
					cancel()
					if err != nil && ctx.Err() != nil {
						return ctx.Err()
					}
				}
			}
		case <-capture.changed:
			if !x.Headless {
				printItems(false)
			}
		case line, ok := <-selectionLines:
			if !ok {
				err := <-readErrors
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			switch line {
			case "q", "quit", "exit":
				return nil
			case "list", "":
				printItems(true)
				continue
			}
			index, err := strconv.Atoi(line)
			items := capture.snapshot()
			if err != nil || index < 1 || index > len(items) {
				fmt.Fprintln(stderr, "Choose a listed media number, list, or q.")
				continue
			}
			item := items[index-1]
			if strings.HasPrefix(item.Kind, "DASH") {
				fmt.Fprintln(stderr, "DASH is detected but unsupported by this downloader; choose HLS or a direct media file.")
				continue
			}
			selected := o
			selected.URL = item.URL
			selected.Detect, selected.Raw, selected.YouTube = false, item.Kind == "FILE", false
			if selected.Referer == "" {
				selected.Referer = item.Referer
			}
			selected.CookieJar, err = browserCookies(browserCtx)
			if err != nil {
				return fmt.Errorf("read extraction session cookies: %w", err)
			}
			shutdown, cancel := context.WithTimeout(browserCtx, 5*time.Second)
			err = chromedp.Cancel(shutdown)
			cancel()
			if err != nil {
				return fmt.Errorf("close extraction browser: %w", err)
			}
			// Input has one reader; HLS variant prompts consume subsequent terminal lines.
			err = Download(ctx, selected, &extractInput{ctx: ctx, lines: lines}, output, stderr)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if x.Headless {
				return fmt.Errorf("headless download failed: %w", err)
			}
			return err
		}
	}
}

// ponytail: native media in accessible frames only; custom players use --play-selector or interactive mode.
const autoPlayMedia = `(() => {
 const play = doc => {
  for (const media of doc.querySelectorAll('video,audio')) {
   media.muted = true;
   media.play().catch(() => {});
  }
  for (const frame of doc.querySelectorAll('iframe')) {
   try { if (frame.contentDocument) play(frame.contentDocument); } catch (_) {}
  }
 };
 play(document);
})()`

// Both browser runs share one stdin reader, so the old run cannot steal input
// or discard buffered selections during the handoff to a visible browser.
type extractionInput struct {
	io.Reader
	once   sync.Once
	lines  chan string
	errors chan error
}

func (r *extractionInput) start(ctx context.Context) {
	r.once.Do(func() {
		go func() {
			reader := bufio.NewReader(r.Reader)
			for {
				line, err := reader.ReadString('\n')
				if line != "" {
					select {
					case r.lines <- strings.TrimSpace(line):
					case <-ctx.Done():
						return
					}
				}
				if err != nil {
					r.errors <- err
					close(r.lines)
					return
				}
			}
		}()
	})
}

type extractInput struct {
	ctx     context.Context
	lines   <-chan string
	pending string
}

func (r *extractInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pending == "" {
		select {
		case <-r.ctx.Done():
			return 0, r.ctx.Err()
		case line, ok := <-r.lines:
			if !ok {
				return 0, io.EOF
			}
			r.pending = line + "\n"
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func browserCookies(ctx context.Context) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	var cookies []*network.Cookie
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(ctx context.Context) error {
		var err error
		cookies, err = storage.GetCookies().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		return err
	}))
	if err != nil {
		return nil, err
	}
	for _, c := range cookies {
		// Partitioned cookies cannot be represented safely by a standard HTTP cookie jar.
		if c.PartitionKey != nil || c.PartitionKeyOpaque {
			continue
		}
		scheme := "http"
		if c.Secure {
			scheme = "https"
		}
		host := strings.TrimPrefix(c.Domain, ".")
		u := &url.URL{Scheme: scheme, Host: host, Path: c.Path}
		domain := ""
		if strings.HasPrefix(c.Domain, ".") {
			domain = c.Domain
		}
		cookie := &http.Cookie{Name: c.Name, Value: c.Value, Domain: domain, Path: c.Path, Secure: c.Secure, HttpOnly: c.HTTPOnly}
		if !c.Session {
			cookie.Expires = time.Unix(int64(c.Expires), 0)
		}
		jar.SetCookies(u, []*http.Cookie{cookie})
	}
	return jar, nil
}
