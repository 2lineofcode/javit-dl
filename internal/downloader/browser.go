package downloader

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto"
	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// Keep one flat browser session: attaching another session can release a paused
// page before Network.enable, losing one-shot startup requests.
func (c *mediaCapture) watchPages(ctx context.Context, x ExtractOptions) (func(), error) {
	address := x.debugAddress
	if address == "" {
		args, err := browser.GetBrowserCommandLine().Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
		if err != nil {
			return nil, err
		}
		for _, arg := range args {
			if dir, ok := strings.CutPrefix(arg, "--user-data-dir="); ok {
				port, err := os.ReadFile(filepath.Join(dir, "DevToolsActivePort"))
				if err != nil {
					return nil, err
				}
				address = "http://127.0.0.1:" + strings.SplitN(string(port), "\n", 2)[0]
			}
		}
	}
	if address == "" {
		return nil, fmt.Errorf("browser debugging address unavailable")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address+"/json/version", nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var version struct {
		URL string `json:"webSocketDebuggerUrl"`
	}
	err = json.NewDecoder(resp.Body).Decode(&version)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	conn, err := chromedp.DialContext(ctx, version.URL)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	closeOnCancel := context.AfterFunc(ctx, func() { _ = conn.Close() })
	var writeMu, commandsMu sync.Mutex
	var id atomic.Int64
	commands := make(map[int64]chan *cdproto.Message)
	send := func(session target.SessionID, method string, params any) error {
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		number := id.Add(1)
		result := make(chan *cdproto.Message, 1)
		commandsMu.Lock()
		commands[number] = result
		commandsMu.Unlock()
		defer func() { commandsMu.Lock(); delete(commands, number); commandsMu.Unlock() }()
		writeMu.Lock()
		err = conn.Write(ctx, &cdproto.Message{ID: number, SessionID: session, Method: cdproto.MethodType(method), Params: raw})
		writeMu.Unlock()
		if err != nil {
			return err
		}
		timer := time.NewTimer(5 * time.Second)
		defer timer.Stop()
		select {
		case response := <-result:
			if response.Error != nil {
				return response.Error
			}
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("browser command timed out: %s", method)
		}
	}
	var workers sync.WaitGroup
	mainTarget := chromedp.FromContext(ctx).Target.TargetID
	mainSetup := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		requests := make(map[string]string)
		for {
			var message cdproto.Message
			if err := conn.Read(ctx, &message); err != nil {
				if ctx.Err() == nil {
					select {
					case c.errors <- fmt.Errorf("browser capture connection: %w", err):
					default:
					}
				}
				return
			}
			if message.ID != 0 {
				commandsMu.Lock()
				result := commands[message.ID]
				commandsMu.Unlock()
				if result != nil {
					select {
					case result <- &message:
					default:
					}
				}
				continue
			}
			event, err := cdproto.UnmarshalMessage(&message)
			if err != nil {
				continue
			}
			switch e := event.(type) {
			case *target.EventAttachedToTarget:
				workers.Go(func() {
					if !x.AllowPopups && isExtractionPopup(e.TargetInfo, mainTarget) {
						// Resume first: a paused window.open can also stall its opener.
						_ = send(e.SessionID, "Runtime.runIfWaitingForDebugger", nil)
						_ = send("", "Target.closeTarget", map[string]any{"targetId": e.TargetInfo.TargetID})
						return
					}
					page := e.TargetInfo.Type == "page" || e.TargetInfo.Type == "iframe"
					err := send(e.SessionID, "Network.enable", nil)
					if err == nil && page {
						err = send(e.SessionID, "Page.enable", nil)
					}
					if err == nil && page {
						err = send(e.SessionID, "Runtime.enable", nil)
					}
					if err == nil && page {
						err = send(e.SessionID, "Page.addScriptToEvaluateOnNewDocument", map[string]any{"source": extractionScript(x), "runImmediately": true})
					}
					if err == nil && page {
						err = send(e.SessionID, "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true, "filter": []map[string]any{{"type": "page"}, {"type": "iframe"}, {"type": "worker"}, {"type": "shared_worker"}, {"type": "service_worker"}, {"exclude": true}}})
					}
					c.mu.Lock()
					c.ready[e.TargetInfo.TargetID] = err == nil
					c.mu.Unlock()
					resumeErr := send(e.SessionID, "Runtime.runIfWaitingForDebugger", nil)
					if err == nil {
						err = resumeErr
					}
					if e.TargetInfo.TargetID == mainTarget {
						select {
						case mainSetup <- err:
						default:
						}
					}
					if err != nil && ctx.Err() == nil {
						select {
						case c.errors <- err:
						default:
						}
					}
				})
			case *network.EventRequestWillBeSent:
				for key, value := range e.Request.Headers {
					if strings.EqualFold(key, "Referer") {
						requests[string(message.SessionID)+":"+string(e.RequestID)], _ = value.(string)
					}
				}
			case *network.EventResponseReceived:
				key := string(message.SessionID) + ":" + string(e.RequestID)
				referer := requests[key]
				delete(requests, key)
				if e.Response.Status >= 200 && e.Response.Status < 300 {
					if kind := mediaKind(e.Response.URL, e.Response.MimeType, e.Type); kind != "" {
						c.add(mediaURL{URL: e.Response.URL, Kind: kind, Referer: referer})
					}
				}
			case *network.EventLoadingFailed:
				delete(requests, string(message.SessionID)+":"+string(e.RequestID))
			}
		}
	}()
	stop := func() { cancel(); _ = conn.Close(); closeOnCancel(); <-done; workers.Wait() }
	if err := send("", "Target.setAutoAttach", map[string]any{"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true, "filter": []map[string]any{{"type": "page"}, {"type": "iframe"}, {"type": "worker"}, {"type": "shared_worker"}, {"type": "service_worker"}, {"exclude": true}}}); err != nil {
		stop()
		return nil, err
	}
	select {
	case err := <-mainSetup:
		if err != nil {
			stop()
			return nil, err
		}
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	case <-time.After(5 * time.Second):
		stop()
		return nil, fmt.Errorf("browser capture initialization timed out")
	}
	return stop, nil
}

func isExtractionPopup(info *target.Info, mainTarget target.ID) bool {
	return info.Type == "page" && info.TargetID != mainTarget &&
		!strings.HasPrefix(info.URL, "devtools://") && !strings.HasPrefix(info.URL, "chrome-devtools://")
}
