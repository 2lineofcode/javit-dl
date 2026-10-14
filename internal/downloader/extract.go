package downloader

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"kuraku-downloader/internal/appdata"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// ExtractOptions controls browser capture independently of download options.
type ExtractOptions struct {
	Browser, PlaySelector string
	Headless              bool
	AutoFilename          bool
	AllowPopups           bool
	CaptureDuration       time.Duration
	debugAddress          string
	OnMedia               func([]Media)
	OnOutput              func(string)
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
	ctx, cancelSession := context.WithCancel(ctx)
	input = contextualFile(ctx, input)
	defer cancelInput(ctx, input)()
	defer cancelSession()
	if input != nil {
		input = bufio.NewReader(input)
	}
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
	if x.AutoFilename {
		info, err := os.Stat(filepath.Dir(o.Out))
		if err != nil {
			return fmt.Errorf("output directory: %w", err)
		}
		if !info.IsDir() {
			return errors.New("output parent must be a directory")
		}
	} else if err := o.prepareOutput(ctx, input, output); err != nil {
		return err
	}
	if input == nil {
		return errors.New("extract requires interactive input")
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return fmt.Errorf("browser session directory: %w", err)
	}
	configDir, err = appdata.Dir(configDir)
	if err != nil {
		return fmt.Errorf("browser session directory: %w", err)
	}
	site := sha256.Sum256([]byte(strings.ToLower(u.Hostname())))
	profile := filepath.Join(configDir, "browser", fmt.Sprintf("%x", site))
	if err := os.MkdirAll(profile, 0700); err != nil {
		return fmt.Errorf("create browser session: %w", err)
	}
	input = &extractionInput{Reader: input, lines: make(chan string), errors: make(chan error, 1)}
	run := func(headless bool) error {
		// Chrome marks even a visible browser as automated when the debugging port is 0.
		// Reserve an available loopback port instead; Chrome must bind it after we close it.
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return fmt.Errorf("allocate extraction browser port: %w", err)
		}
		port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
		if err := listener.Close(); err != nil {
			return fmt.Errorf("release extraction browser port: %w", err)
		}
		opts := []chromedp.ExecAllocatorOption{chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck, chromedp.UserDataDir(profile), chromedp.Flag("remote-debugging-address", "127.0.0.1"), chromedp.Flag("remote-debugging-port", port), chromedp.Flag("no-sandbox", false)}
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
		session.debugAddress = "http://127.0.0.1:" + port
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
	errors  chan error
	ready   map[target.ID]bool
}

func newMediaCapture() *mediaCapture {
	return &mediaCapture{ready: make(map[target.ID]bool), seen: make(map[string]int), changed: make(chan struct{}, 1), errors: make(chan error, 1)}
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

func extractSession(ctx, browserCtx context.Context, o Options, x ExtractOptions, input io.Reader, output, stderr io.Writer) error {
	if x.OnMedia != nil {
		x.OnMedia(nil)
	}
	capture := newMediaCapture()
	if err := chromedp.Run(browserCtx, network.Enable(), chromedp.Evaluate(`navigator.userAgent`, &o.UserAgent), target.SetAutoAttach(false, false).WithFlatten(true)); err != nil {
		return fmt.Errorf("start extraction browser (install Chrome/Chromium or use --browser): %w", err)
	}
	if x.Headless && x.PlaySelector != "" {
		selector, _ := json.Marshal(x.PlaySelector)
		if err := chromedp.Run(browserCtx, chromedp.Evaluate("document.querySelector("+string(selector)+")", nil)); err != nil {
			return fmt.Errorf("invalid play-selector: %w", err)
		}
	}
	stopPages, err := capture.watchPages(browserCtx, x)
	if err != nil {
		return fmt.Errorf("watch browser pages: %w", err)
	}
	defer stopPages()
	popups := make(chan *target.Info, 32)
	mainTarget := chromedp.FromContext(browserCtx).Target.TargetID
	if !x.AllowPopups {
		chromedp.ListenBrowser(browserCtx, func(event any) {
			if created, ok := event.(*target.EventTargetCreated); ok && created.TargetInfo.Type == "page" && created.TargetInfo.TargetID != mainTarget {
				select {
				case popups <- created.TargetInfo:
				default: // The existing tab poll also catches bursts of popups.
				}
			}
		})
	}
	closePopup := func(info *target.Info) bool {
		if x.AllowPopups || !isExtractionPopup(info, mainTarget) {
			return false
		}
		// Close extra tabs without changing the player page's browser APIs.
		closing, cancel := context.WithTimeout(browserCtx, 2*time.Second)
		defer cancel()
		executor := cdp.WithExecutor(closing, chromedp.FromContext(browserCtx).Browser)
		if err := target.CloseTarget(info.TargetID).Do(executor); err == nil {
			_ = target.ActivateTarget(chromedp.FromContext(browserCtx).Target.TargetID).Do(executor)
		}
		return true
	}
	startup, cancel := context.WithTimeout(browserCtx, o.Timeout)
	err = chromedp.Run(startup, chromedp.Navigate(o.URL))
	cancel()
	if err != nil {
		return fmt.Errorf("open extraction browser (install Chrome/Chromium or use --browser): %w", err)
	}
	if x.Headless {
		fmt.Fprintf(output, "Capturing media in the background for %s...\n", x.CaptureDuration)
	} else {
		fmt.Fprintln(output, "Complete any login or human verification in the extraction browser, then play the video. Your browser session is saved automatically. HLS playlists appear as [HLS].\nEnter a media number to download, list to show results, or q to quit.")
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
	// Auto-attach captures startup requests; polling maintains playback/title contexts.
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
	selectionLines := (<-chan string)(lines)
	if x.Headless {
		timer := time.NewTimer(x.CaptureDuration)
		defer timer.Stop()
		captureDone = timer.C
		selectionLines = nil // Piped selections wait for capture to finish too.
		playbackCtx, stopPlayback = context.WithTimeout(browserCtx, x.CaptureDuration)
		defer stopPlayback()

	}
	for {
		select {
		case err := <-capture.errors:
			fmt.Fprintf(stderr, "Cannot watch browser media: %v\n", err)
		case popup := <-popups:
			closePopup(popup)
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
				if closePopup(info) {
					continue
				}
				if info.Type != "page" || watched[string(info.TargetID)] != nil {
					continue
				}
				capture.mu.Lock()
				ready := capture.ready[info.TargetID]
				capture.mu.Unlock()
				if !ready {
					continue
				}
				tab, closeTab := chromedp.NewContext(browserCtx, chromedp.WithTargetID(info.TargetID))
				defer closeTab()
				watched[string(info.TargetID)] = tab
				attachCtx, cancel := context.WithTimeout(tab, o.Timeout)
				var stop func() bool
				if x.Headless {
					stop = context.AfterFunc(playbackCtx, cancel)
				}
				err := chromedp.Run(attachCtx, target.SetAutoAttach(false, false).WithFlatten(true))
				if stop != nil {
					stop()
				}
				cancel()
				if err != nil {
					fmt.Fprintf(stderr, "Cannot watch tab: %v\n", err)
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
			if x.AutoFilename {
				page := browserCtx
				if targets, err := chromedp.Targets(browserCtx); err == nil {
					for _, target := range targets {
						if target.Type == "page" && target.URL == item.Referer && watched[string(target.TargetID)] != nil {
							page = watched[string(target.TargetID)]
							break
						}
					}
				}
				inspect, cancel := context.WithTimeout(page, o.Timeout)
				var title string
				if err := chromedp.Run(inspect, chromedp.Title(&title)); err != nil {
					fmt.Fprintf(stderr, "Cannot read page title; using a fallback filename: %v\n", err)
				}
				cancel()
				selected.Out, err = extractedOutput(o.Out, title, item)
				if err != nil {
					fmt.Fprintf(stderr, "Choose another output path: %v\n", err)
					continue
				}
				fmt.Fprintf(output, "Save to: %s\n", selected.Out)
				if x.OnOutput != nil {
					x.OnOutput(selected.Out)
				}
			}
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
			err = Download(ctx, selected, &LineReader{Context: ctx, Lines: lines}, output, stderr)
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

func extractedOutput(out, title string, item mediaURL) (string, error) {
	name := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			return '_'
		}
		return r
	}, title)
	// Leave room for the extension and collision suffix within filesystem limits.
	for index, r := range name {
		if index+utf8.RuneLen(r) > 200 {
			name = name[:index]
			break
		}
	}
	name = strings.Trim(name, " .")
	if name == "" || name == "Just a moment" {
		name = "video"
	}
	ext := ".mp4"
	if item.Kind == "FILE" {
		if u, err := url.Parse(item.URL); err == nil {
			switch candidate := strings.ToLower(filepath.Ext(u.Path)); candidate {
			case ".mp4", ".webm", ".mov", ".m4a", ".mp3", ".ogg", ".wav":
				ext = candidate
			}
		}
	}
	for index := 1; ; index++ {
		base := name
		if index > 1 {
			base = fmt.Sprintf("%s (%d)", name, index)
		}
		path := filepath.Join(filepath.Dir(out), base+ext)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", err
		}
	}
}

// Installed before navigation in every frame, including paused cross-origin frames.
func extractionScript(x ExtractOptions) string {
	selector, _ := json.Marshal(x.PlaySelector)
	return fmt.Sprintf(`(() => {
  if (globalThis.__kurakuCaptureInstalled) return;
  globalThis.__kurakuCaptureInstalled = true;
  if (!%t) return;
  const selector = %s;
  let clicked = false;
  const play = () => {
   if (selector) {
    const button = document.querySelector(selector);
    if (button && !clicked) { clicked = true; button.click(); }
   } else {
    for (const media of document.querySelectorAll('video,audio')) {
     media.muted = true;
     media.play().catch(() => {});
    }
   }
  };
  play();
  const timer = setInterval(play, 250);
  const finish = () => setTimeout(() => clearInterval(timer), %d);
  if (document.readyState === 'complete') finish();
  else window.addEventListener('load', finish, {once: true});
 })()`, x.Headless, selector, x.CaptureDuration.Milliseconds())
}

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

// LineReader adapts terminal or GUI selection lines to a cancellable reader.
type LineReader struct {
	Context context.Context
	Lines   <-chan string
	pending string
}

func (r *LineReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.pending == "" {
		select {
		case <-r.Context.Done():
			return 0, r.Context.Err()
		case line, ok := <-r.Lines:
			if !ok {
				return 0, io.EOF
			}
			r.pending = strings.TrimSuffix(line, "\n") + "\n"
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
