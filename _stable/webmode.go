package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"myd/web"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Global HTTP Client and User-Agent
var client = &http.Client{Timeout: 30 * time.Second}
const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36" // Disguise as Chrome

type KeyInfo struct {
	Method string
	URI    string
	IV     []byte
}

type Segment struct {
	URI      string
	Seq      int64
	Filename string
}

// ==========================================
// NEW: WriteCounter for raw download progress
// ==========================================
type WriteCounter struct {
	Total int64
	Downloaded int64
	LastUpdate time.Time
	FileName string
}

// -----------------------------
// Web job progress tracking
// -----------------------------
type JobProgress struct {
    Phase string        `json:"phase"`
    Percent float64     `json:"percent"`
    Done bool           `json:"done"`
    Error string        `json:"error"`
    File string         `json:"file"`
    TotalSegments int   `json:"totalSegments"`
    CompletedSegments int32 `json:"completedSegments"`
    Started time.Time   `json:"started"`
}

var jobStore sync.Map // map[string]*JobProgress

func setJob(id string, p *JobProgress) {
    jobStore.Store(id, p)
}

func getJob(id string) *JobProgress {
    if v, ok := jobStore.Load(id); ok {
        return v.(*JobProgress)
    }
    return nil
}

func updateJobError(id, msg string) {
    if p := getJob(id); p != nil {
        p.Error = msg
        p.Phase = "error"
    }
}

func updateJobDone(id, file string) {
    if p := getJob(id); p != nil {
        p.Done = true
        p.Phase = "done"
        p.Percent = 100
        p.File = file
    }
}

func updateJobPhase(id, phase string) {
    if p := getJob(id); p != nil {
        p.Phase = phase
    }
}

func updateJobSegmentsTotal(id string, total int) {
    if p := getJob(id); p != nil {
        p.TotalSegments = total
        if total == 0 { p.Percent = 0 }
    }
}

func incrementJobSegment(id string) {
    if p := getJob(id); p != nil {
        c := atomic.AddInt32(&p.CompletedSegments, 1)
        if p.TotalSegments > 0 {
            p.Percent = float64(c) / float64(p.TotalSegments) * 100
        }
    }
}

// Write implements the io.Writer interface.
func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Downloaded += int64(n)
	
	// Update progress setiap 200ms atau jika sudah selesai
	if time.Since(wc.LastUpdate) > 200*time.Millisecond || wc.Downloaded == wc.Total {
		wc.PrintProgress()
		wc.LastUpdate = time.Now()
	}
	return n, nil
}

func (wc *WriteCounter) PrintProgress() {
	if wc.Total == 0 {
		fmt.Printf("\rDownloading %s: %s downloaded...", wc.FileName, formatBytes(wc.Downloaded))
		return
	}
	
	pct := float64(wc.Downloaded) / float64(wc.Total) * 100
	fmt.Printf("\rDownloading %s: %.2f%% (%s / %s)", wc.FileName, pct, formatBytes(wc.Downloaded), formatBytes(wc.Total))
}

func formatBytes(b int64) string {
    const unit = 1024
    if b < unit {
        return fmt.Sprintf("%d B", b)
    }
    div, exp := int64(unit), 0
    for n := b / unit; n >= unit; n /= unit {
        div *= unit
        exp++
    }
    return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// ==========================================
// END WriteCounter
// ==========================================

// expandUserPath expands a leading ~ to the user's home directory for convenience.
func expandUserPath(p string) string {
	if p == "" { return p }
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err != nil { return p }
		if p == "~" {
			return h
		}
		return filepath.Join(h, strings.TrimPrefix(p, "~/"))
	}
	return p
}

func main() {
	// Web server flags
	serve := flag.Bool("serve", false, "Run as web server")
	addr := flag.String("addr", ":8080", "Server address")

	m3u8URL := flag.String("u", "", "URL to m3u8 playlist or raw file")
	out := flag.String("o", "output.ts", "Output file")
	concurrency := flag.Int("c", 8, "Concurrent downloads (M3U8 only)")
	retries := flag.Int("r", 3, "Retries per segment (M3U8 only)")
	tmpdir := flag.String("tf", "m3u8_tmp", "Temporary segment directory (M3U8 only)")
	auto := flag.Bool("auto", false, "Auto select highest bandwidth variant (M3U8 only)")
	rawURL := flag.Bool("rawurl", false, "Just download the single file URL, skip M3U8 parsing.")
	flag.Parse()

	// Web server mode
	if *serve {
		mux := http.NewServeMux()
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, web.RootPage)
				return
			}
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		})

		mux.HandleFunc("/download", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
				return
			}
			if err := r.ParseForm(); err != nil {
				http.Error(w, "Bad Request", http.StatusBadRequest)
				return
			}
			u := strings.TrimSpace(r.FormValue("url"))
			if u == "" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "Missing url"})
				return
			}
			name := strings.TrimSpace(r.FormValue("name"))
			if name == "" { name = "output" }
			cc, _ := strconv.Atoi(r.FormValue("c"))
			if cc <= 0 { cc = 8 }
			rr, _ := strconv.Atoi(r.FormValue("r"))
			if rr < 0 { rr = 3 }
			autoSel := r.FormValue("auto") == "on"
			raw := r.FormValue("raw") == "on"
			folder := strings.TrimSpace(r.FormValue("folder"))
			if folder == "" { folder = "." }
			folder = expandUserPath(folder)
			absFolder := folder
			if !filepath.IsAbs(absFolder) { absFolder = filepath.Join(".", absFolder) }
			absFolder, _ = filepath.Abs(absFolder)
			if err := os.MkdirAll(absFolder, 0755); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "Cannot create folder"})
				return
			}
			cwd, _ := os.Getwd()

			jobID := fmt.Sprintf("job_%d", time.Now().UnixNano())
			progress := &JobProgress{Phase: "queued", Started: time.Now()}
			setJob(jobID, progress)

			go func() {
				updateJobPhase(jobID, "starting")
				finalOut := filepath.Join(absFolder, name+".mp4")
				err := RunDownloadWithProgress(u, finalOut, cc, rr, autoSel, raw, jobID)
				if err != nil {
					updateJobError(jobID, err.Error())
					return
				}
				rel, _ := filepath.Rel(cwd, finalOut)
				if strings.HasPrefix(finalOut, cwd) { updateJobDone(jobID, rel) } else { updateJobDone(jobID, finalOut) }
			}()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			json.NewEncoder(w).Encode(map[string]string{"id": jobID})
		})

		mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			if id == "" { http.Error(w, "Missing id", http.StatusBadRequest); return }
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			p := getJob(id)
			if p == nil { w.WriteHeader(404); w.Write([]byte(`{"error":"not found"}`)); return }
			json.NewEncoder(w).Encode(p)
		})

		mux.HandleFunc("/api/dirs", func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Query().Get("p")
			if p == "" { p = "/" }
			p = filepath.Clean(p)
			abs := p
			if !filepath.IsAbs(abs) { abs, _ = filepath.Abs(abs) }
			entries, _ := os.ReadDir(abs)
			var onlyDirs []string
			for _, e := range entries {
				if e.IsDir() {
					name := e.Name()
					if strings.HasPrefix(name, ".") { continue }
					onlyDirs = append(onlyDirs, name)
				}
			}
			up := filepath.Dir(abs)
			if up == abs { up = "" }
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"path": abs, "up": up, "entries": onlyDirs})
		})

		mux.HandleFunc("/dl", func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			if id == "" { http.Error(w, "Missing id", http.StatusBadRequest); return }
			p := getJob(id)
			if p == nil || !p.Done || p.File == "" { http.Error(w, "Not ready", http.StatusBadRequest); return }
			filePath := p.File
			if !filepath.IsAbs(filePath) { // might be relative to cwd
				cwd, _ := os.Getwd()
				filePath = filepath.Join(cwd, filePath)
			}
			w.Header().Set("Content-Type", "video/mp4")
			w.Header().Set("Content-Disposition", "attachment; filename=\""+path.Base(filePath)+"\"")
			f, err := os.Open(filePath)
			if err != nil { http.Error(w, "Open error", http.StatusInternalServerError); return }
			defer f.Close()
			io.Copy(w, f)
		})

		mux.HandleFunc("/view", func(w http.ResponseWriter, r *http.Request) {
			id := r.URL.Query().Get("id")
			if id == "" { http.Error(w, "Missing id", http.StatusBadRequest); return }
			p := getJob(id)
			if p == nil || !p.Done || p.File == "" { http.Error(w, "Not ready", http.StatusBadRequest); return }
			filePath := p.File
			if !filepath.IsAbs(filePath) {
				cwd, _ := os.Getwd()
				filePath = filepath.Join(cwd, filePath)
			}
			w.Header().Set("Content-Type", "video/mp4")
			f, err := os.Open(filePath)
			if err != nil { http.Error(w, "Open error", http.StatusInternalServerError); return }
			defer f.Close()
			io.Copy(w, f)
		})

		mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir("."))))

		log.Println("Starting server on", *addr)
		if err := http.ListenAndServe(*addr, mux); err != nil {
			log.Fatal(err)
		}
		return
	}

	if *m3u8URL == "" {
		fmt.Println("")
		fmt.Println("JavIt Downloader - M3U8 & File Downloader")
		fmt.Println(" -h for help")
		fmt.Println("")
		fmt.Println("Example:")
		fmt.Println(" ./myd -url https://example.com/file.m3u8 -out some_video")
		fmt.Println(" ./myd -url https://example.com/file.m3u8 -out some_video -auto")
		fmt.Println(" ./myd -url https://example.com/file.mp4 -out some_video.mp4 -rawurl")
		os.Exit(0)
	}

	if *out == "" {
		fmt.Println("Output file cannot be empty.")
		os.Exit(1)
	}

	if strings.Contains(*out, ".mp4") {
		fmt.Println("Output file should not have .mp4 extension, it will be added automatically.")
		fmt.Println("")
		fmt.Println("Example:")
		fmt.Println(" ./myd -url https://example.com/file.m3u8 -out some_video")
		fmt.Println(" ./myd -url https://example.com/file.m3u8 -out some_video -auto")
		fmt.Println(" ./myd -url https://example.com/file.mp4 -out some_video.mp4 -rawurl")
		os.Exit(1)
	}

	start := time.Now()
	
	// ======================================
	// RAW FILE DOWNLOAD MODE
	// ======================================
	if *rawURL {
		fmt.Println("Starting raw file download:", *m3u8URL)
		if err := downloadRawFile(*m3u8URL, *out); err != nil {
			panic(err)
		}
		fmt.Println("Raw download complete →", *out)
		fmt.Println("\nAll done in", time.Since(start).Round(time.Second))
		return // Exit main function after raw download
	}

	// ======================================
	// M3U8 DOWNLOAD MODE
	// ======================================
	fmt.Println("Fetching playlist:", *m3u8URL)
	body, err := getText(*m3u8URL)
	if err != nil {
		panic(err)
	}

	u, _ := url.Parse(*m3u8URL)
	base := getBaseURL(u) 

	// ================
	// MASTER PLAYLIST
	// ================
	if strings.Contains(body, "#EXT-X-STREAM-INF") {
		variants := parseMasterVariants(body, base)
		if len(variants) == 0 {
			panic("No variants found.")
		}

		
		fmt.Printf("\nDetected %d variants:\n", len(variants))
		for i, v := range variants {
			fmt.Printf("  [%d] %s (BANDWIDTH=%d)\n", i, v.uri, v.bandwidth)
		}
		var chosen string
		if *auto {
			best := variants[0]
			for _, v := range variants {
				if v.bandwidth > best.bandwidth {
					best = v
				}
			}
			chosen = best.uri
			fmt.Println("\nAuto selected highest bandwidth:", chosen)
		} else {
			fmt.Print("\nChoose variant index: ")
			var idx int
			fmt.Scanln(&idx)
			if idx < 0 || idx >= len(variants) {
				panic("Invalid index")
			}
			chosen = variants[idx].uri
			fmt.Println("Chosen variant:", chosen)
		}
		body, err = getText(chosen)
		if err != nil {
			panic(err)
		}
		vURL, _ := url.Parse(chosen)
		base = getBaseURL(vURL) 
	}

	// =================
	// MEDIA PLAYLIST
	// =================
	segs, keyInfo, err := parseMediaPlaylist(body, base)
	if err != nil {
		panic(err)
	}
	if len(segs) == 0 {
		panic("No segments found.")
	}
	fmt.Printf("Found %d segments\n", len(segs))

	td := *tmpdir
    fmt.Println(td)
	os.MkdirAll(td, 0755)

	// KEY
	var keyBytes []byte
	var keyIV []byte
	if keyInfo != nil && strings.ToUpper(keyInfo.Method) == "AES-128" {
		fmt.Println("Fetching AES-128 key...")
		keyBytes, keyIV, err = fetchKey(keyInfo, base)
		if err != nil {
			panic(err)
		}
		fmt.Println("Key loaded (16 bytes)")
	}

	// DOWNLOAD
	total := len(segs)
	progress := make(chan struct{}, total)
	var doneCount int64
	
	fmt.Println("\nDownloading segments...")
	wg := sync.WaitGroup{}
	sem := make(chan struct{}, *concurrency)

	for _, seg := range segs {
		wg.Add(1)
		seg := seg
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			err := downloadSegment(seg, keyBytes, keyIV, td, *retries)
			<-sem
			if err == nil {
				progress <- struct{}{}
			} else {
				// ignore failed segment as requested
				fmt.Println("\n⚠️  skipped segment (failed):", seg.Filename, "error:", err)
				progress <- struct{}{} // still count it so progress can finish
			}
		}()
	}

	// Progress bar
	go func() {
		for range progress {
			doneCount++
			pct := float64(doneCount) / float64(total) * 100
			// elapsed := time.Since(start).Seconds()
			// speed := float64(doneCount) / elapsed
			// fmt.Printf("\rProgress: %d/%d (%.1f%%) | %.1f seg/s", doneCount, total, pct, speed)
			fmt.Printf("\rProgress: %d/%d | (%.1f%%)", doneCount, total, pct)
		}
	}()

	wg.Wait()
	close(progress)
	fmt.Println("\ndownloads complete ✅")

	// CONCAT (merge)
	fmt.Println("Merging segments...")
	err = concatSegments(segs, td, *out)
	if err != nil {
		panic(err)
	}

	// =========================
	// Auto run ffmpeg afterwards
	// =========================
	ffmpegOutput := strings.TrimSuffix(*out, path.Ext(*out)) + ".mp4"
	fmt.Println("Converting to MP4...")

	// find ffmpeg in $PATH
	ffmpeg_path, err := exec.LookPath("ffmpeg")
    if err != nil {
        fmt.Println("⚠️ Error: ffmpeg not found in system PATH. Conversion skipped.")
        fmt.Println("   Please ensure ffmpeg is installed and accessible in the environment where this binary runs.")
        fmt.Println("   (e.g., trying common macOS path /opt/homebrew/bin/ffmpeg)")
        ffmpeg_path = "/opt/homebrew/bin/ffmpeg"
        if _, err := os.Stat(ffmpeg_path); os.IsNotExist(err) {
            fmt.Println("   Fallback path also failed. Skipping conversion.")
        }
    }

    fmt.Println("Using ffmpeg at:", ffmpeg_path) 

	cmd := exec.Command(
		ffmpeg_path, "-y", 
		"-hide_banner", "-loglevel", "quiet",
		"-f", "mpegts",
		"-i", *out, 
		"-c", "copy",
		"-bsf:v", "h264_mp4toannexb",
		"-bsf:a", "aac_adtstoasc",
		"-movflags", "+faststart",
		"-f", "mp4",
		ffmpegOutput,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println("⚠️ convert failed:", err)
	} else {
		fmt.Println("✅ convert done →", ffmpegOutput)
	}

	// ====================
	// Cleanup temp folder
	// ====================
	fmt.Println("Cleaning up temporary files...")
	if err := os.RemoveAll(td); err != nil {
		fmt.Println("⚠️ Failed to delete tmp folder:", err)
	}

	// ====================
	// Cleanup .ts file
	// ====================
	if err := os.Remove(*out); err != nil {
		fmt.Println("⚠️ Failed to delete .ts file:", err)
	}

	fmt.Println("\nAll done in", time.Since(start).Round(time.Second))
}

// ---------------------------------------------------------------------------------------------------------------------

// downloadRawFile: Downloads a single file from the given URL to the output path with a progress bar.
func downloadRawFile(fileURL, outPath string) error {
    req, err := http.NewRequest("GET", fileURL, nil)
    if err != nil {
        return err
    }
    req.Header.Set("User-Agent", userAgent)
    
    resp, err := client.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode >= 400 {
        return fmt.Errorf("HTTP %d", resp.StatusCode)
    }

	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)

    out, err := os.Create(outPath)
    if err != nil {
        return err
    }
    defer out.Close()

	// Init WriteCounter
	counter := &WriteCounter{
		Total: size,
		FileName: path.Base(outPath),
		LastUpdate: time.Now(),
	}
    _, err = io.Copy(out, io.TeeReader(resp.Body, counter))
	fmt.Println() 
    return err
}

type variant struct {
	uri       string
	bandwidth int64
}

// getBaseURL: returns a new *url.URL which represents the base directory of the given URL,
// while preserving the original query parameters.
func getBaseURL(u *url.URL) *url.URL {
	basePath := path.Dir(u.Path) + "/"

	return &url.URL{
		Scheme:   u.Scheme,
		Host:     u.Host,
		Path:     basePath,
		RawQuery: u.RawQuery,
	}
}

func parseMasterVariants(body string, base *url.URL) []variant {
	sc := bufio.NewScanner(strings.NewReader(body))
	var variants []variant
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attr := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			bw, _ := strconv.ParseInt(attr["BANDWIDTH"], 10, 64)
			if sc.Scan() {
				uri := strings.TrimSpace(sc.Text())
				full, _ := resolveURL(base, uri)
				variants = append(variants, variant{uri: full, bandwidth: bw})
			}
		}
	}
	return variants
}

// parseAttributes: split on commas but ignore commas inside quotes (e.g. CODECS="...,...")
func parseAttributes(s string) map[string]string {
	res := map[string]string{}
	parts := splitAttrCommaAware(s)
	for _, p := range parts {
		pair := strings.SplitN(p, "=", 2)
		if len(pair) == 2 {
			key := strings.TrimSpace(pair[0])
			val := strings.TrimSpace(pair[1])
			val = strings.Trim(val, `"`)
			res[key] = val
		}
	}
	return res
}

func splitAttrCommaAware(s string) []string {
	var res []string
	var cur strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if ch == '"' {
			inQuotes = !inQuotes
			cur.WriteByte(ch)
			continue
		}
		if ch == ',' && !inQuotes {
			res = append(res, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	if cur.Len() > 0 {
		res = append(res, cur.String())
	}
	return res
}

// getText: Fetches the M3U8 content using a disguised User-Agent.
func getText(u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	return string(data), err
}

// resolveURL: resolves a reference URL against the base URL.
// The base URL is assumed to already contain the necessary query parameters.
func resolveURL(base *url.URL, ref string) (string, error) {
	ref = strings.TrimSpace(ref)

	// Tries to parse the reference directly first.
	u, err := url.Parse(ref)
	if err != nil {
		// Fallback: try escaping common problematic chars if initial parse fails
		refEsc := strings.ReplaceAll(ref, " ", "%20")
		refEsc = strings.ReplaceAll(refEsc, ",", "%2C")
		u, err = url.Parse(refEsc)
		if err != nil {
			return "", err
		}
	}
	
	// If the reference is absolute, just return it.
	if u.IsAbs() {
		return u.String(), nil
	}

	// Resolve relative URL against the base.
	return base.ResolveReference(u).String(), nil
}

func parseMediaPlaylist(body string, base *url.URL) ([]Segment, *KeyInfo, error) {
	sc := bufio.NewScanner(strings.NewReader(body))
	var segs []Segment
	var key *KeyInfo
	var seq int64 = 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#EXTM3U") {
			continue
		}
		if strings.HasPrefix(line, "#EXT-X-KEY:") {
			attrs := parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			key = &KeyInfo{Method: attrs["METHOD"], URI: attrs["URI"], IV: parseIV(attrs["IV"])}
		} else if !strings.HasPrefix(line, "#") {
			full, _ := resolveURL(base, line)
			segs = append(segs, Segment{URI: full, Seq: seq, Filename: fmt.Sprintf("%06d.ts", seq)})
			seq++
		}
	}
	return segs, key, nil
}

func parseIV(s string) []byte {
	if s == "" {
		return nil
	}
	s = strings.TrimPrefix(strings.ToLower(s), "0x")
	b, _ := hex.DecodeString(s)
	if len(b) < 16 {
		pad := make([]byte, 16)
		copy(pad[16-len(b):], b)
		return pad
	}
	return b[:16]
}

// fetchKey: Fetches the AES key using a disguised User-Agent.
func fetchKey(k *KeyInfo, base *url.URL) ([]byte, []byte, error) {
	full, _ := resolveURL(base, k.URI)
	
	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	
	if resp.StatusCode >= 400 {
		return nil, nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	key, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return key, k.IV, nil
}

// downloadSegment: Downloads and (if necessary) decrypts the segment using a disguised User-Agent.
func downloadSegment(seg Segment, key []byte, iv []byte, outDir string, retries int) error {
	outPath := path.Join(outDir, seg.Filename)
	if fi, err := os.Stat(outPath); err == nil && fi.Size() > 0 {
		return nil // resume skip
	}
	var lastErr error
	for i := 0; i <= retries; i++ {
		
		req, err := http.NewRequest("GET", seg.URI, nil)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		req.Header.Set("User-Agent", userAgent)

		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			resp.Body.Close()
			time.Sleep(time.Second)
			continue
		}
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if key != nil {
			ivv := iv
			if ivv == nil {
				ivv = make([]byte, 16)
			}
			data, err = decryptAES(data, key, ivv)
			if err != nil {
				lastErr = err
				continue
			}
		}
		err = os.WriteFile(outPath, data, 0644)
		if err == nil {
			return nil
		}
		lastErr = err
	}
	return lastErr
}

func decryptAES(data, key, iv []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(iv) != 16 {
		return nil, fmt.Errorf("invalid iv len")
	}
	data = pad16(data)
	dst := make([]byte, len(data))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(dst, data)
	return pkcs7Unpad(dst), nil
}

func pad16(b []byte) []byte {
	if len(b)%16 == 0 {
		return b
	}
	n := 16 - (len(b) % 16)
	return append(b, bytes.Repeat([]byte{0}, n)...)
}

func pkcs7Unpad(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	n := int(b[len(b)-1])
	if n <= 0 || n > 16 || n > len(b) {
		return b
	}
	return b[:len(b)-n]
}

func concatSegments(segs []Segment, dir, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 32*1024)
	for _, s := range segs {
		p := path.Join(dir, s.Filename)
		// if missing segment (failed download) -> skip
		if _, err := os.Stat(path.Join(dir, s.Filename)); os.IsNotExist(err) {
			fmt.Println("⚠️  segment missing, skipping:", s.Filename)
			continue
		}
		sf, err := os.Open(p)
		if err != nil {
			return err
		}
		io.CopyBuffer(f, sf, buf)
		sf.Close()
	}
	return nil
}

// RunDownload executes the download flow and returns the final MP4 path.
func RunDownload(inputURL string, outPath string, concurrency int, retries int, tmpdir string, auto bool, raw bool) (string, error) {
	start := time.Now()

	if raw {
		if err := downloadRawFile(inputURL, outPath); err != nil {
			return "", err
		}
		ffmpegOutput := strings.TrimSuffix(outPath, path.Ext(outPath)) + ".mp4"
		ffmpeg_path, err := exec.LookPath("ffmpeg")
        if err != nil {
            ffmpeg_path = "/opt/homebrew/bin/ffmpeg"
        }
        cmd := exec.Command(
            ffmpeg_path, "-y",
            "-hide_banner", "-loglevel", "quiet",
            "-f", "mpegts",
            "-i", outPath,
            "-c", "copy",
            "-bsf:v", "h264_mp4toannexb",
            "-bsf:a", "aac_adtstoasc",
            "-movflags", "+faststart",
            "-f", "mp4",
            ffmpegOutput,
        )
        cmd.Stdout = io.Discard
        cmd.Stderr = io.Discard
        _ = cmd.Run()
        return ffmpegOutput, nil
	}

    body, err := getText(inputURL)
    if err != nil { return "", err }
    u, _ := url.Parse(inputURL)
    base := getBaseURL(u)

    if strings.Contains(body, "#EXT-X-STREAM-INF") {
        variants := parseMasterVariants(body, base)
        if len(variants) == 0 { return "", fmt.Errorf("no variants") }
        var chosen string
        if auto {
            best := variants[0]
            for _, v := range variants { if v.bandwidth > best.bandwidth { best = v } }
            chosen = best.uri
        } else {
            chosen = variants[0].uri
        }
        body, err = getText(chosen)
        if err != nil { return "", err }
        vURL, _ := url.Parse(chosen)
        base = getBaseURL(vURL)
    }

    segs, keyInfo, err := parseMediaPlaylist(body, base)
    if err != nil { return "", err }
    if len(segs) == 0 { return "", fmt.Errorf("no segments") }

    os.MkdirAll(tmpdir, 0755)

    var keyBytes []byte
    var keyIV []byte
    if keyInfo != nil && strings.ToUpper(keyInfo.Method) == "AES-128" {
        keyBytes, keyIV, err = fetchKey(keyInfo, base)
        if err != nil { return "", err }
    }

    wg := sync.WaitGroup{}
    sem := make(chan struct{}, concurrency)
    for _, seg := range segs {
        wg.Add(1)
        seg := seg
        go func() {
            defer wg.Done()
            sem <- struct{}{}
            _ = downloadSegment(seg, keyBytes, keyIV, tmpdir, retries)
            <-sem
        }()
    }
    wg.Wait()

    if err := concatSegments(segs, tmpdir, outPath); err != nil { return "", err }

    ffmpegOutput := strings.TrimSuffix(outPath, path.Ext(outPath)) + ".mp4"
    ffmpeg_path, err := exec.LookPath("ffmpeg")
    if err != nil { ffmpeg_path = "/opt/homebrew/bin/ffmpeg" }
    cmd := exec.Command(
        ffmpeg_path, "-y",
        "-hide_banner", "-loglevel", "quiet",
        "-f", "mpegts",
        "-i", outPath,
        "-c", "copy",
        "-bsf:v", "h264_mp4toannexb",
        "-bsf:a", "aac_adtstoasc",
        "-movflags", "+faststart",
        "-f", "mp4",
        ffmpegOutput,
    )
    cmd.Stdout = io.Discard
    cmd.Stderr = io.Discard
    _ = cmd.Run()

    _ = os.Remove(outPath)
    _ = os.RemoveAll(tmpdir)

    _ = start // suppress unused if optimized differently in future
    return ffmpegOutput, nil
}

// RunDownloadWithProgress executes the download flow while updating a job's progress.
func RunDownloadWithProgress(inputURL string, finalMP4 string, concurrency int, retries int, auto bool, raw bool, jobID string) error {
    updateJobPhase(jobID, "init")

    workDir, err := os.MkdirTemp("", "m3u8_job_*")
    if err != nil { return err }
    defer os.RemoveAll(workDir)

    outTS := filepath.Join(workDir, "output.ts")

    if raw {
        updateJobPhase(jobID, "downloading raw")
        if err := downloadRawFile(inputURL, outTS); err != nil { return err }
        updateJobPhase(jobID, "converting")
        ffmpeg_path, err := exec.LookPath("ffmpeg")
        if err != nil { ffmpeg_path = "/opt/homebrew/bin/ffmpeg" }
        cmd := exec.Command(
            ffmpeg_path, "-y",
            "-hide_banner", "-loglevel", "quiet",
            "-f", "mpegts",
            "-i", outTS,
            "-c", "copy",
            "-bsf:v", "h264_mp4toannexb",
            "-bsf:a", "aac_adtstoasc",
            "-movflags", "+faststart",
            "-f", "mp4",
            finalMP4,
        )
        cmd.Stdout = io.Discard
        cmd.Stderr = io.Discard
        _ = cmd.Run()
        return nil
    }

    updateJobPhase(jobID, "fetching playlist")
    body, err := getText(inputURL)
    if err != nil { return err }
    u, _ := url.Parse(inputURL)
    base := getBaseURL(u)

    if strings.Contains(body, "#EXT-X-STREAM-INF") {
        variants := parseMasterVariants(body, base)
        if len(variants) == 0 { return fmt.Errorf("no variants") }
        var chosen string
        if auto {
            best := variants[0]
            for _, v := range variants { if v.bandwidth > best.bandwidth { best = v } }
            chosen = best.uri
        } else {
            chosen = variants[0].uri
        }
        body, err = getText(chosen)
        if err != nil { return err }
        vURL, _ := url.Parse(chosen)
        base = getBaseURL(vURL)
    }

    segs, keyInfo, err := parseMediaPlaylist(body, base)
    if err != nil { return err }
    if len(segs) == 0 { return fmt.Errorf("no segments") }

    updateJobSegmentsTotal(jobID, len(segs))

    os.MkdirAll(workDir, 0755)

    var keyBytes []byte
    var keyIV []byte
    if keyInfo != nil && strings.ToUpper(keyInfo.Method) == "AES-128" {
        updateJobPhase(jobID, "fetching key")
        keyBytes, keyIV, err = fetchKey(keyInfo, base)
        if err != nil { return err }
    }

    updateJobPhase(jobID, "downloading segments")
    wg := sync.WaitGroup{}
    sem := make(chan struct{}, concurrency)
    for _, seg := range segs {
        wg.Add(1)
        seg := seg
        go func() {
            defer wg.Done()
            sem <- struct{}{}
            _ = downloadSegment(seg, keyBytes, keyIV, workDir, retries)
            <-sem
            incrementJobSegment(jobID)
        }()
    }
    wg.Wait()

    updateJobPhase(jobID, "merging")
    if err := concatSegments(segs, workDir, outTS); err != nil { return err }

    updateJobPhase(jobID, "converting")
    ffmpeg_path, err := exec.LookPath("ffmpeg")
    if err != nil { ffmpeg_path = "/opt/homebrew/bin/ffmpeg" }
    cmd := exec.Command(
        ffmpeg_path, "-y",
        "-hide_banner", "-loglevel", "quiet",
        "-f", "mpegts",
        "-i", outTS,
        "-c", "copy",
        "-bsf:v", "h264_mp4toannexb",
        "-bsf:a", "aac_adtstoasc",
        "-movflags", "+faststart",
        "-f", "mp4",
        finalMP4,
    )
    cmd.Stdout = io.Discard
    cmd.Stderr = io.Discard
    _ = cmd.Run()

    return nil
}