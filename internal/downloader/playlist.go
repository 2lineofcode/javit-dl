package downloader

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

const maxPlaylist = 8 << 20

type variant struct {
	uri           string
	bandwidth     int64
	externalAudio bool
}
type asset struct {
	uri, name      string
	offset, length int64
	key            bool
	encrypted      bool
}
type playlist struct {
	text     string
	assets   []asset
	segments int
}

func parseHTTPURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return nil, errors.New("URL must be an absolute HTTP(S) URL without embedded credentials")
	}
	return u, nil
}

func resolveURL(base *url.URL, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", errors.New("empty playlist URI")
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	full := base.ResolveReference(u).String()
	_, err = parseHTTPURL(full)
	return full, err
}

func playlistLines(body string) ([]string, error) {
	body = strings.TrimPrefix(body, "\ufeff")
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 4096), maxPlaylist)
	var lines []string
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return nil, errors.New("response is not an M3U8 playlist")
	}
	return lines, nil
}

// HLS attributes allow commas inside quoted values, unlike strings.Split.
func parseAttributes(s string) (map[string]string, error) {
	attrs := make(map[string]string)
	for s != "" {
		k, rest, ok := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, errors.New("invalid HLS attribute")
		}
		rest = strings.TrimSpace(rest)
		var value string
		if strings.HasPrefix(rest, `"`) {
			value, rest, ok = strings.Cut(rest[1:], `"`)
			if !ok || (rest != "" && !strings.HasPrefix(rest, ",")) {
				return nil, errors.New("invalid quoted HLS attribute")
			}
			rest = strings.TrimPrefix(rest, ",")
		} else {
			value, rest, _ = strings.Cut(rest, ",")
		}
		if _, exists := attrs[k]; exists {
			return nil, fmt.Errorf("duplicate HLS attribute %s", k)
		}
		attrs[k], s = strings.TrimSpace(value), rest
	}
	return attrs, nil
}

func parseVariants(lines []string, base *url.URL) ([]variant, error) {
	var variants []variant
	externalGroups := make(map[string]bool)
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-MEDIA:") {
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MEDIA:"))
			if err != nil {
				return nil, err
			}
			if attrs["TYPE"] == "AUDIO" && attrs["URI"] != "" {
				externalGroups[attrs["GROUP-ID"]] = true
			}
		}
	}
	var pending map[string]string
	for _, line := range lines {
		if strings.HasPrefix(line, "#EXT-X-STREAM-INF:") {
			if pending != nil {
				return nil, errors.New("variant is missing its URI")
			}
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-STREAM-INF:"))
			if err != nil {
				return nil, err
			}
			pending = attrs
		} else if line != "" && !strings.HasPrefix(line, "#") && pending != nil {
			uri, err := resolveURL(base, line)
			if err != nil {
				return nil, err
			}
			bw, err := strconv.ParseInt(pending["BANDWIDTH"], 10, 64)
			if err != nil || bw <= 0 {
				return nil, errors.New("variant has invalid BANDWIDTH")
			}
			variants = append(variants, variant{uri, bw, externalGroups[pending["AUDIO"]]})
			pending = nil
		}
	}
	if pending != nil {
		return nil, errors.New("variant is missing its URI")
	}
	return variants, nil
}

func byteRange(value string, previous asset, uri string) (int64, int64, error) {
	size, start, explicit := strings.Cut(value, "@")
	length, err := strconv.ParseInt(size, 10, 64)
	if err != nil || length <= 0 || length == math.MaxInt64 {
		return 0, 0, errors.New("invalid HLS byte range length")
	}
	offset := previous.offset + previous.length
	if explicit {
		offset, err = strconv.ParseInt(start, 10, 64)
	} else if previous.uri != uri || previous.length == 0 {
		return 0, 0, errors.New("implicit byte range requires a preceding range of the same resource")
	}
	if err != nil || offset < 0 || offset > math.MaxInt64-length {
		return 0, 0, errors.New("invalid HLS byte range offset")
	}
	return offset, length, nil
}

func localPlaylist(lines []string, base *url.URL) (playlist, error) {
	var p playlist
	var result []string
	var rangeValue string
	var previous asset
	var encrypted, explicitIV, end, hasDuration bool
	add := func(ref, ext, rangeText string, key bool) (asset, error) {
		uri, err := resolveURL(base, ref)
		if err != nil {
			return asset{}, err
		}
		a := asset{uri: uri, name: fmt.Sprintf("%06d%s", len(p.assets), ext), key: key, encrypted: encrypted && !key}
		if rangeText != "" {
			a.offset, a.length, err = byteRange(rangeText, previous, uri)
			if err != nil {
				return asset{}, err
			}
		}
		p.assets = append(p.assets, a)
		return a, nil
	}
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "#EXT-X-KEY:"):
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-KEY:"))
			if err != nil {
				return p, err
			}
			method := attrs["METHOD"]
			encrypted, explicitIV = method != "NONE", attrs["IV"] != ""
			if method == "NONE" {
				line = "#EXT-X-KEY:METHOD=NONE"
				break
			}
			if method != "AES-128" || (attrs["KEYFORMAT"] != "" && attrs["KEYFORMAT"] != "identity") {
				return p, errors.New("only identity AES-128 encryption is supported; DRM/SAMPLE-AES is unsupported")
			}
			if explicitIV {
				iv := strings.TrimPrefix(attrs["IV"], "0x")
				if iv == attrs["IV"] || len(iv) == 0 || len(iv) > 32 || strings.ContainsAny(iv, "\r\n") {
					return p, errors.New("invalid AES IV")
				}
				for _, c := range iv {
					if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
						return p, errors.New("invalid AES IV")
					}
				}
				attrs["IV"] = "0x" + strings.Repeat("0", 32-len(iv)) + iv
			}
			a, err := add(attrs["URI"], ".key", "", true)
			if err != nil {
				return p, err
			}
			line = `#EXT-X-KEY:METHOD=AES-128,URI="` + a.name + `"`
			if explicitIV {
				line += ",IV=" + attrs["IV"]
			}
		case strings.HasPrefix(line, "#EXT-X-MAP:"):
			attrs, err := parseAttributes(strings.TrimPrefix(line, "#EXT-X-MAP:"))
			if err != nil {
				return p, err
			}
			if encrypted && !explicitIV {
				return p, errors.New("encrypted initialization segment requires an explicit IV")
			}
			a, err := add(attrs["URI"], ".mp4", attrs["BYTERANGE"], false)
			if err != nil {
				return p, err
			}
			line = `#EXT-X-MAP:URI="` + a.name + `"`
		case strings.HasPrefix(line, "#EXT-X-BYTERANGE:"):
			if rangeValue != "" {
				return p, errors.New("duplicate segment byte range")
			}
			rangeValue = strings.TrimPrefix(line, "#EXT-X-BYTERANGE:")
			continue // Resources are downloaded as individual ranges; local files start at zero.
		case strings.HasPrefix(line, "#EXTINF:"):
			if hasDuration {
				return p, errors.New("segment is missing its URI")
			}
			hasDuration = true
		case line == "#EXT-X-ENDLIST":
			end = true
		case line == "#EXT-X-GAP", line == "#EXT-X-I-FRAMES-ONLY", strings.HasPrefix(line, "#EXT-X-PART"), strings.HasPrefix(line, "#EXT-X-PRELOAD-HINT"), strings.HasPrefix(line, "#EXT-X-DEFINE"), strings.HasPrefix(line, "#EXT-X-STREAM-INF"), strings.HasPrefix(line, "#EXT-X-MEDIA:"), strings.HasPrefix(line, "#EXT-X-SESSION-KEY"), strings.HasPrefix(line, "#EXT-X-") && strings.Contains(line, "URI="):
			return p, fmt.Errorf("unsupported playlist tag: %s", strings.SplitN(line, ":", 2)[0])
		case line != "" && !strings.HasPrefix(line, "#"):
			if !hasDuration {
				return p, errors.New("segment is missing EXTINF")
			}
			// The local demuxer probes the content; the extension does not dictate its format.
			a, err := add(line, ".ts", rangeValue, false)
			if err != nil {
				return p, err
			}
			previous, rangeValue, hasDuration = a, "", false
			line = a.name
			p.segments++
		}
		result = append(result, line)
	}
	if p.segments == 0 || hasDuration || rangeValue != "" {
		return p, errors.New("playlist has no complete media segments")
	}
	if !end {
		return p, errors.New("live playlists are unsupported; a finished playlist with EXT-X-ENDLIST is required")
	}
	p.text = strings.Join(result, "\n") + "\n"
	return p, nil
}
