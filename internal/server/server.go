package server

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"javit_dl/internal/downloader"
	"javit_dl/internal/jobs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Deps struct {
	HTTPClient *http.Client
	UserAgent  string
}

func Start(port string, deps Deps) error {
	// Prepare template once
	tpl := template.Must(template.ParseFiles("web/templates/root.tmpl"))

	mux := http.NewServeMux()

	fmt.Println("Server ready on: http://localhost" + port)

	// GET /
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tpl.Execute(w, nil); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	})

	// POST /download
	mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		u := strings.TrimSpace(r.PostFormValue("url"))
		if u == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Missing url"})
			return
		}
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			name = "output"
		}
		cc, _ := strconv.Atoi(r.PostFormValue("c"))
		if cc <= 0 {
			cc = 8
		}
		rr, _ := strconv.Atoi(r.PostFormValue("r"))
		if rr < 0 {
			rr = 3
		}
		autoSel := r.PostFormValue("auto") == "on"
		raw := r.PostFormValue("raw") == "on"
		folder := strings.TrimSpace(r.PostFormValue("folder"))
		if folder == "" {
			folder = "."
		}
		absFolder := downloader.ExpandUserPath(folder)
		if !filepath.IsAbs(absFolder) {
			absFolder = filepath.Join(".", absFolder)
		}
		absFolder, _ = filepath.Abs(absFolder)
		if err := os.MkdirAll(absFolder, 0755); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "Cannot create folder"})
			return
		}
		cwd, _ := os.Getwd()
		jobID := fmt.Sprintf("job_%d", time.Now().UnixNano())
		progress := &jobs.JobProgress{Phase: "queued", Started: time.Now()}
		jobs.SetJob(jobID, progress)
		go func() {
			jobs.UpdateJobPhase(jobID, "starting")
			finalOut := filepath.Join(absFolder, name+".mp4")
			err := downloader.RunDownloadWithForWeb(deps.HTTPClient, deps.UserAgent, u, finalOut, cc, rr, autoSel, raw, jobID, progressCallbacks{})
			if err != nil {
				jobs.UpdateJobError(jobID, err.Error())
				return
			}
			rel, _ := filepath.Rel(cwd, finalOut)
			if strings.HasPrefix(finalOut, cwd) {
				jobs.UpdateJobDone(jobID, rel)
			} else {
				jobs.UpdateJobDone(jobID, finalOut)
			}
		}()
		writeJSON(w, http.StatusAccepted, map[string]any{"id": jobID})
	})

	// GET /status?id=
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		p := jobs.GetJob(id)
		if p == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		writeJSON(w, http.StatusOK, p)
	})

	// GET /api/dirs?p=
	mux.HandleFunc("/api/dirs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		p := r.URL.Query().Get("p")
		if p == "" {
			p = "/"
		}
		p = filepath.Clean(p)
		abs := p
		if !filepath.IsAbs(abs) {
			abs, _ = filepath.Abs(abs)
		}
		entries, _ := os.ReadDir(abs)
		var onlyDirs []string
		for _, e := range entries {
			if e.IsDir() {
				name := e.Name()
				if strings.HasPrefix(name, ".") {
					continue
				}
				onlyDirs = append(onlyDirs, name)
			}
		}
		up := filepath.Dir(abs)
		if up == abs {
			up = ""
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": abs, "up": up, "entries": onlyDirs})
	})

	// GET /dl?id=
	mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing id", http.StatusBadRequest)
			return
		}
		p := jobs.GetJob(id)
		if p == nil || !p.Done || p.File == "" {
			http.Error(w, "Not ready", http.StatusBadRequest)
			return
		}
		filePath := p.File
		if !filepath.IsAbs(filePath) {
			cwd, _ := os.Getwd()
			filePath = filepath.Join(cwd, filePath)
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Disposition", "attachment; filename="+path.Base(filePath))
		f, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "Open error", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		io.Copy(w, f)
	})

	// GET /view?id=
	mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "Missing id", http.StatusBadRequest)
			return
		}
		p := jobs.GetJob(id)
		if p == nil || !p.Done || p.File == "" {
			http.Error(w, "Not ready", http.StatusBadRequest)
			return
		}
		filePath := p.File
		if !filepath.IsAbs(filePath) {
			cwd, _ := os.Getwd()
			filePath = filepath.Join(cwd, filePath)
		}
		w.Header().Set("Content-Type", "video/mp4")
		f, err := os.Open(filePath)
		if err != nil {
			http.Error(w, "Open error", http.StatusInternalServerError)
			return
		}
		defer f.Close()
		io.Copy(w, f)
	})

	// Wrap with middleware
	handler := recoverMiddleware(loggingMiddleware(mux))

	portPrefix := ""
	if !strings.Contains(port, ":") {
		portPrefix = ":" + port
	} else {
		portPrefix = port
	}
	return http.ListenAndServe(portPrefix, handler)
}

type progressCallbacks struct{}

func (progressCallbacks) UpdatePhase(id, phase string)        { jobs.UpdateJobPhase(id, phase) }
func (progressCallbacks) UpdateSegments(id string, total int) { jobs.UpdateJobSegmentsTotal(id, total) }
func (progressCallbacks) Inc(id string)                       { jobs.IncrementJobSegment(id) }

// writeJSON writes the given value as JSON with the provided status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// loggingMiddleware logs basic request info.
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// recoverMiddleware prevents panics from crashing the server and returns 500.
func recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic: %v", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
