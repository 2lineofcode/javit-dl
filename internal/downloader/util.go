package downloader

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func FormatBytes(b int64) string {
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

func ExpandUserPath(p string) string {
	if p == "" {
		return p
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return p
		}
		if p == "~" {
			return h
		}
		return filepath.Join(h, strings.TrimPrefix(p, "~/"))
	}
	return p
}
