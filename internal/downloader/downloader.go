package downloader

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
	"fmt"
	"io"
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

type WriteCounter struct {
	Total      int64
	Downloaded int64
	LastUpdate time.Time
	FileName   string
}

func (wc *WriteCounter) Write(p []byte) (int, error) {
	n := len(p)
	wc.Downloaded += int64(n)
	if time.Since(wc.LastUpdate) > 200*time.Millisecond || wc.Downloaded == wc.Total {
		wc.PrintProgress()
		wc.LastUpdate = time.Now()
	}
	return n, nil
}

func (wc *WriteCounter) PrintProgress() {
	if wc.Total == 0 {
		fmt.Printf("\rDownloading %s: %s downloaded...", wc.FileName, FormatBytes(wc.Downloaded))
		return
	}
	pct := float64(wc.Downloaded) / float64(wc.Total) * 100
	fmt.Printf("\rDownloading %s: %.2f%% (%s / %s)", wc.FileName, pct, FormatBytes(wc.Downloaded), FormatBytes(wc.Total))
}

// Public API
func RunDownloadForCLI(httpClient *http.Client, userAgent string, inputURL string, outPath string, concurrency int, retries int, tmpdir string, auto bool, raw bool) (string, error) {
	start := time.Now()
	if raw {
		if err := downloadRawFile(httpClient, userAgent, inputURL, outPath); err != nil {
			return "", err
		}
		ffmpegOutput := strings.TrimSuffix(outPath, path.Ext(outPath)) + ".mp4"
		ffmpeg_path, err := exec.LookPath("ffmpeg")
		if err != nil {
			ffmpeg_path = "/opt/homebrew/bin/ffmpeg"
		}
		cmd := exec.Command(ffmpeg_path, "-y", "-hide_banner", "-loglevel", "quiet", "-f", "mpegts", "-i", outPath, "-c", "copy", "-bsf:v", "h264_mp4toannexb", "-bsf:a", "aac_adtstoasc", "-movflags", "+faststart", "-f", "mp4", ffmpegOutput)
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		_ = cmd.Run()
		return ffmpegOutput, nil
	}
	body, err := getText(httpClient, userAgent, inputURL)
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(inputURL)
	base := getBaseURL(u)
	if strings.Contains(body, "#EXT-X-STREAM-INF") {
		variants := parseMasterVariants(body, base)
		if len(variants) == 0 {
			return "", fmt.Errorf("no variants")
		}
		var chosen string
		if auto {
			best := variants[0]
			for _, v := range variants {
				if v.bandwidth > best.bandwidth {
					best = v
				}
			}
			chosen = best.uri
		} else {
			chosen = variants[0].uri
		}
		body, err = getText(httpClient, userAgent, chosen)
		if err != nil {
			return "", err
		}
		vURL, _ := url.Parse(chosen)
		base = getBaseURL(vURL)
	}
	segs, keyInfo, err := parseMediaPlaylist(body, base)
	if err != nil {
		return "", err
	}
	if len(segs) == 0 {
		return "", fmt.Errorf("no segments")
	}
	os.MkdirAll(tmpdir, 0755)
	var keyBytes []byte
	var keyIV []byte
	if keyInfo != nil && strings.ToUpper(keyInfo.Method) == "AES-128" {
		keyBytes, keyIV, err = fetchKey(httpClient, userAgent, keyInfo, base)
		if err != nil {
			return "", err
		}
	}

	// CLI progress output
	total := len(segs)
	var done int32
	fmt.Println("\nDownloading segments...")

	wg := sync.WaitGroup{}
	sem := make(chan struct{}, concurrency)
	for _, seg := range segs {
		wg.Add(1)
		seg := seg
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			_ = downloadSegment(httpClient, userAgent, seg, keyBytes, keyIV, tmpdir, retries)
			<-sem
			d := atomic.AddInt32(&done, 1)
			pct := float64(d) / float64(total) * 100
			fmt.Printf("\rProgress: %d/%d | (%.1f%%)", d, total, pct)
		}()
	}
	wg.Wait()
	fmt.Println()

	if err := concatSegments(segs, tmpdir, outPath); err != nil {
		return "", err
	}

	fmt.Println("Converting to MP4...")
	ffmpegOutput := strings.TrimSuffix(outPath, path.Ext(outPath)) + ".mp4"
	ffmpeg_path, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpeg_path = "/opt/homebrew/bin/ffmpeg"
	}
	cmd := exec.Command(ffmpeg_path, "-y", "-hide_banner", "-loglevel", "quiet", "-f", "mpegts", "-i", outPath, "-c", "copy", "-bsf:v", "h264_mp4toannexb", "-bsf:a", "aac_adtstoasc", "-movflags", "+faststart", "-f", "mp4", ffmpegOutput)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
	_ = os.Remove(outPath)
	_ = os.RemoveAll(tmpdir)
	_ = start
	return ffmpegOutput, nil
}

func RunDownloadWithForWeb(httpClient *http.Client, userAgent string, inputURL string, finalMP4 string, concurrency int, retries int, auto bool, raw bool, jobID string, cb ProgressCallbacks) error {
	cb.UpdatePhase(jobID, "init")
	workDir, err := os.MkdirTemp("", "m3u8_job_*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workDir)
	outTS := filepath.Join(workDir, "output.ts")
	if raw {
		cb.UpdatePhase(jobID, "downloading raw")
		if err := downloadRawFile(httpClient, userAgent, inputURL, outTS); err != nil {
			return err
		}
		cb.UpdatePhase(jobID, "converting")
		return convertToMp4(outTS, finalMP4)
	}
	cb.UpdatePhase(jobID, "fetching playlist")
	body, err := getText(httpClient, userAgent, inputURL)
	if err != nil {
		return err
	}
	u, _ := url.Parse(inputURL)
	base := getBaseURL(u)
	if strings.Contains(body, "#EXT-X-STREAM-INF") {
		variants := parseMasterVariants(body, base)
		if len(variants) == 0 {
			return fmt.Errorf("no variants")
		}
		var chosen string
		if auto {
			best := variants[0]
			for _, v := range variants {
				if v.bandwidth > best.bandwidth {
					best = v
				}
			}
			chosen = best.uri
		} else {
			chosen = variants[0].uri
		}
		body, err = getText(httpClient, userAgent, chosen)
		if err != nil {
			return err
		}
		vURL, _ := url.Parse(chosen)
		base = getBaseURL(vURL)
	}
	segs, keyInfo, err := parseMediaPlaylist(body, base)
	if err != nil {
		return err
	}
	if len(segs) == 0 {
		return fmt.Errorf("no segments")
	}
	cb.UpdateSegments(jobID, len(segs))
	os.MkdirAll(workDir, 0755)
	var keyBytes []byte
	var keyIV []byte
	if keyInfo != nil && strings.ToUpper(keyInfo.Method) == "AES-128" {
		cb.UpdatePhase(jobID, "fetching key")
		keyBytes, keyIV, err = fetchKey(httpClient, userAgent, keyInfo, base)
		if err != nil {
			return err
		}
	}
	cb.UpdatePhase(jobID, "downloading segments")
	wg := sync.WaitGroup{}
	sem := make(chan struct{}, concurrency)
	for _, seg := range segs {
		wg.Add(1)
		seg := seg
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			_ = downloadSegment(httpClient, userAgent, seg, keyBytes, keyIV, workDir, retries)
			<-sem
			cb.Inc(jobID)
		}()
	}
	wg.Wait()
	cb.UpdatePhase(jobID, "merging")
	if err := concatSegments(segs, workDir, outTS); err != nil {
		return err
	}
	cb.UpdatePhase(jobID, "converting")
	return convertToMp4(outTS, finalMP4)
}

type ProgressCallbacks interface {
	UpdatePhase(id, phase string)
	UpdateSegments(id string, total int)
	Inc(id string)
}

// internal helpers
func convertToMp4(inTs, outMp4 string) error {
	fmt.Println("Converting to MP4...")
	ffmpeg_path, err := exec.LookPath("ffmpeg")
	if err != nil {
		ffmpeg_path = "/opt/homebrew/bin/ffmpeg"
	}
	cmd := exec.Command(ffmpeg_path, "-y", "-hide_banner", "-loglevel", "quiet", "-f", "mpegts", "-i", inTs, "-c", "copy", "-bsf:v", "h264_mp4toannexb", "-bsf:a", "aac_adtstoasc", "-movflags", "+faststart", "-f", "mp4", outMp4)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	_ = cmd.Run()
	return nil
}

func downloadRawFile(httpClient *http.Client, userAgent, fileURL, outPath string) error {
	req, err := http.NewRequest("GET", fileURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	size := resp.ContentLength
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	counter := &WriteCounter{Total: size, FileName: path.Base(outPath), LastUpdate: time.Now()}
	_, err = io.Copy(out, io.TeeReader(resp.Body, counter))
	fmt.Println()
	return err
}

func getBaseURL(u *url.URL) *url.URL {
	basePath := path.Dir(u.Path) + "/"
	return &url.URL{Scheme: u.Scheme, Host: u.Host, Path: basePath, RawQuery: u.RawQuery}
}

func parseMasterVariants(body string, base *url.URL) []struct {
	uri       string
	bandwidth int64
} {
	sc := bufio.NewScanner(strings.NewReader(body))
	var variants []struct {
		uri       string
		bandwidth int64
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			attr := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			bw, _ := strconv.ParseInt(attr["BANDWIDTH"], 10, 64)
			if sc.Scan() {
				uri := strings.TrimSpace(sc.Text())
				full, _ := resolveURL(base, uri)
				variants = append(variants, struct {
					uri       string
					bandwidth int64
				}{uri: full, bandwidth: bw})
			}
		}
	}
	return variants
}

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

func getText(httpClient *http.Client, userAgent, u string) (string, error) {
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
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

func resolveURL(base *url.URL, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	u, err := url.Parse(ref)
	if err != nil {
		refEsc := strings.ReplaceAll(ref, " ", "%20")
		refEsc = strings.ReplaceAll(refEsc, ",", "%2C")
		u, err = url.Parse(refEsc)
		if err != nil {
			return "", err
		}
	}
	if u.IsAbs() {
		return u.String(), nil
	}
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

func fetchKey(httpClient *http.Client, userAgent string, k *KeyInfo, base *url.URL) ([]byte, []byte, error) {
	full, _ := resolveURL(base, k.URI)
	req, err := http.NewRequest("GET", full, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
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

func downloadSegment(httpClient *http.Client, userAgent string, seg Segment, key []byte, iv []byte, outDir string, retries int) error {
	outPath := path.Join(outDir, seg.Filename)
	if fi, err := os.Stat(outPath); err == nil && fi.Size() > 0 {
		return nil
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
		resp, err := httpClient.Do(req)
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
