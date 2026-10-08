package gui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"javit_dl/internal/downloader"
)

// Request contains editable GUI settings. Downloader validation remains authoritative.
type Request struct {
	StartDownload   bool   `json:"startDownload"`
	URL             string `json:"url"`
	Out             string `json:"out"`
	Mode            string `json:"mode"`
	Temp            string `json:"temp"`
	UserAgent       string `json:"userAgent"`
	Referer         string `json:"referer"`
	Browser         string `json:"browser"`
	PlaySelector    string `json:"playSelector"`
	Concurrency     int    `json:"concurrency"`
	Retries         int    `json:"retries"`
	Timeout         int    `json:"timeout"`
	CaptureDuration int    `json:"captureDuration"`
	Auto            bool   `json:"auto"`
	Headless        bool   `json:"headless"`
	AutoFilename    bool   `json:"autoFilename"`
	AllowPopups     bool   `json:"allowPopups"`
}

type Choice struct {
	Index    int    `json:"index"`
	Label    string `json:"label"`
	URL      string `json:"url"`
	Disabled bool   `json:"disabled"`
}

type State struct {
	Active     bool     `json:"active"`
	Status     string   `json:"status"`
	Error      string   `json:"error"`
	Progress   string   `json:"progress"`
	Out        string   `json:"out"`
	Logs       []string `json:"logs"`
	Choices    []Choice `json:"choices"`
	ChoiceKind string   `json:"choiceKind"`
	ChoiceID   int      `json:"choiceID"`
}

// App owns one job. All state exchanged with React is copied under the mutex.
type App struct {
	initial       map[string]any
	ctx           context.Context
	windowCtx     context.Context
	mu            sync.Mutex
	state         State
	cancel        context.CancelFunc
	done          chan struct{}
	input         chan string
	variant       chan int
	mediaSelected bool
}

func newApp(ctx context.Context) *App {
	return &App{ctx: ctx, state: State{Status: "Ready"}}
}

func (a *App) Defaults() (Request, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Request{}, err
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return Request{}, err
	}
	dir := filepath.Join(home, "Downloads")
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		dir = home
	}
	r := Request{Out: filepath.Join(dir, "output.mp4"), Mode: "auto", Temp: filepath.Join(cache, "JavIt-DL", "downloads"), Concurrency: 8, Retries: 3, Timeout: 30, CaptureDuration: 5, Auto: true, AutoFilename: true}
	patch, err := json.Marshal(a.initial)
	if err != nil {
		return Request{}, err
	}
	if err := json.Unmarshal(patch, &r); err != nil {
		return Request{}, err
	}
	if _, explicit := a.initial["out"]; explicit {
		r.AutoFilename = false
	}
	return r, nil
}

func (a *App) GetState() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	s.Logs = append([]string{}, s.Logs...)
	s.Choices = append([]Choice{}, s.Choices...)
	return s
}

func (a *App) Start(r Request) error {
	switch r.Mode {
	case "auto", "hls", "file", "youtube", "extract":
	default:
		return errors.New("invalid download mode")
	}
	if r.Timeout < 1 || r.Timeout > 86400 {
		return errors.New("timeout must be between 1 and 86400 seconds")
	}
	if r.Headless && (r.CaptureDuration < 1 || r.CaptureDuration > 3600) {
		return errors.New("capture duration must be between 1 and 3600 seconds")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Active {
		return errors.New("a download is already running")
	}
	if err := a.ctx.Err(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(a.ctx)
	a.cancel = cancel
	a.done = make(chan struct{})
	a.mediaSelected = false
	a.input = make(chan string, 1)
	a.variant = make(chan int, 1)
	a.state = State{Active: true, Status: "Running", ChoiceID: a.state.ChoiceID + 1}
	input, variant, done := a.input, a.variant, a.done
	go a.run(ctx, cancel, r, input, variant, done)
	return nil
}

func (a *App) run(ctx context.Context, cancel context.CancelFunc, r Request, input chan string, variant chan int, done chan struct{}) {
	defer close(done)
	defer cancel()
	o := downloader.Options{URL: strings.TrimSpace(r.URL), Out: r.Out, Temp: r.Temp, UserAgent: r.UserAgent, Referer: r.Referer, Concurrency: r.Concurrency, Retries: r.Retries, Timeout: time.Duration(r.Timeout) * time.Second, Auto: r.Auto, Raw: r.Mode == "file", Detect: r.Mode == "auto", YouTube: r.Mode == "youtube"}
	if o.UserAgent == "" && r.Mode != "extract" {
		o.UserAgent = "JavIt-DL/2.0"
	}
	o.ChooseVariant = func(ctx context.Context, bandwidths []int64) (int, error) {
		choices := make([]Choice, len(bandwidths))
		for i, b := range bandwidths {
			choices[i] = Choice{Index: i, Label: fmt.Sprintf("Quality %d · %.2f Mbps", i+1, float64(b)/1e6)}
		}
		a.setChoices("quality", choices)
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case index := <-variant:
			return index, nil
		}
	}
	reader := &downloader.LineReader{Context: ctx, Lines: input}
	var err error
	if r.Mode == "extract" {
		x := downloader.ExtractOptions{Browser: r.Browser, Headless: r.Headless, AllowPopups: r.AllowPopups, AutoFilename: r.AutoFilename, OnOutput: func(out string) {
			a.mu.Lock()
			a.state.Out = out
			a.mu.Unlock()
		}, CaptureDuration: time.Duration(r.CaptureDuration) * time.Second, PlaySelector: r.PlaySelector, OnMedia: a.setMedia}
		err = downloader.Extract(ctx, o, x, reader, a, a)
	} else {
		err = downloader.Download(ctx, o, reader, a, a)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.state.Active = false
	a.state.Choices = nil
	a.state.ChoiceKind = ""
	a.state.ChoiceID++
	a.state.Status = "Completed"
	if errors.Is(err, context.Canceled) {
		a.state.Status = "Cancelled"
	} else if err != nil {
		a.state.Status = "Failed"
		a.state.Error = err.Error()
	}
}

func (a *App) setMedia(media []downloader.Media) {
	choices := make([]Choice, 0, len(media))
	for _, m := range media {
		choices = append(choices, Choice{Index: m.Index, Label: m.Kind, URL: m.URL, Disabled: strings.HasPrefix(m.Kind, "DASH")})
	}
	a.mu.Lock()
	if media == nil {
		a.mediaSelected = false
	}
	if !a.mediaSelected && a.state.Status != "Cancelling…" {
		a.state.Choices = choices
		a.state.ChoiceKind = "media"
		a.state.ChoiceID++
	}
	a.mu.Unlock()
}

func (a *App) setChoices(kind string, choices []Choice) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state.Status == "Cancelling…" {
		return
	}
	a.state.Choices = choices
	a.state.ChoiceKind = kind
	a.state.ChoiceID++
}

func (a *App) Select(choiceID, index int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.state.Active || choiceID != a.state.ChoiceID {
		return errors.New("selection expired; choose from the current list")
	}
	for _, c := range a.state.Choices {
		if c.Index != index || c.Disabled {
			continue
		}
		if a.state.ChoiceKind == "quality" {
			select {
			case a.variant <- index:
			default:
				return errors.New("selection already pending")
			}
		} else {
			select {
			case a.input <- strconv.Itoa(index) + "\n":
				a.mediaSelected = true
			default:
				return errors.New("selection already pending")
			}
		}
		a.state.Choices = nil
		a.state.ChoiceKind = ""
		a.state.ChoiceID++
		return nil
	}
	return errors.New("choose an available item")
}

func (a *App) Cancel() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel != nil && a.state.Active {
		a.cancel()
		a.state.Status = "Cancelling…"
		a.state.Choices = nil
		a.state.ChoiceID++
	}
}

func (a *App) shutdown() {
	a.Cancel()
	a.mu.Lock()
	done := a.done
	a.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (a *App) Write(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Progress uses carriage returns; keep only the latest value rather than a growing log.
	for _, line := range strings.Split(string(p), "\n") {
		if index := strings.LastIndexByte(line, '\r'); index >= 0 {
			a.state.Progress = strings.TrimSpace(line[index+1:])
			line = line[:index]
		}
		line = strings.TrimSpace(line)
		if line != "" {
			a.state.Logs = append(a.state.Logs, line[:min(len(line), 4096)])
		}
	}
	if len(a.state.Logs) > 120 {
		a.state.Logs = append([]string{}, a.state.Logs[len(a.state.Logs)-120:]...)
	}
	return len(p), nil
}
