package downloader

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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

// Extract opens a disposable browser and watches media requests until one is selected.
func Extract(ctx context.Context, o Options, browser string, input io.Reader, output, stderr io.Writer) error {
	if _, err := parseHTTPURL(o.URL); err != nil {
		return err
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
	opts := []chromedp.ExecAllocatorOption{chromedp.NoFirstRun, chromedp.NoDefaultBrowserCheck, chromedp.Flag("remote-debugging-address", "127.0.0.1"), chromedp.Flag("no-sandbox", false)}
	if browser != "" {
		opts = append(opts, chromedp.ExecPath(browser))
	}
	if o.UserAgent != "" {
		opts = append(opts, chromedp.UserAgent(o.UserAgent))
	}
	alloc, stop := chromedp.NewExecAllocator(ctx, opts...)
	defer stop()
	browserCtx, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	return extractSession(ctx, browserCtx, o, input, output, stderr)
}

type mediaURL struct{ URL, Kind, Referer string }

type mediaCapture struct {
	mu      sync.Mutex
	items   []mediaURL
	seen    map[string]int
	changed chan struct{}
}

func newMediaCapture() *mediaCapture {
	return &mediaCapture{seen: make(map[string]int), changed: make(chan struct{}, 1)}
}

func (c *mediaCapture) add(item mediaURL) {
	c.mu.Lock()
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
				c.add(mediaURL{e.Response.URL, kind, referer})
			}
		case *network.EventLoadingFailed:
			delete(requests, e.RequestID)
		}
	})
}

func extractSession(ctx, browserCtx context.Context, o Options, input io.Reader, output, stderr io.Writer) error {
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
	fmt.Fprintln(output, "Play a video or navigate in the extraction browser. HLS playlists appear as [HLS].\nEnter a media number to download, list to show results, or q to quit.")
	lines := make(chan string)
	readErrors := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(input)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				select {
				case lines <- strings.TrimSpace(line):
				case <-ctx.Done():
					return
				case <-browserCtx.Done():
					return
				}
			}
			if err != nil {
				readErrors <- err
				close(lines)
				return
			}
		}
	}()
	// ponytail: polling may miss a new tab's first requests; reload it if needed, use auto-attach if that matters.
	watched := map[string]bool{string(chromedp.FromContext(browserCtx).Target.TargetID): true}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	displayed := 0
	printItems := func(all bool) {
		items := capture.snapshot()
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
	for {
		select {
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
				if (info.Type != "page" && info.Type != "iframe") || watched[string(info.TargetID)] {
					continue
				}
				watched[string(info.TargetID)] = true
				tab, closeTab := chromedp.NewContext(browserCtx, chromedp.WithTargetID(info.TargetID))
				defer closeTab()
				capture.watch(tab)
				attachCtx, cancel := context.WithTimeout(tab, o.Timeout)
				err := chromedp.Run(attachCtx, network.Enable())
				cancel()
				if err != nil {
					fmt.Fprintf(stderr, "Cannot watch tab: %v\n", err)
				}
			}
		case <-capture.changed:
			printItems(false)
		case line, ok := <-lines:
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
			// Input has one reader; HLS variant prompts consume subsequent terminal lines.
			err = Download(ctx, selected, &extractInput{ctx: ctx, lines: lines}, output, stderr)
			if err == nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			fmt.Fprintf(stderr, "Download failed: %v\nChoose another media number, or q to quit.\n", err)
		}
	}
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
