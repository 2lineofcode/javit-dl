package appdata

import (
	"fmt"
	"os"
	"path/filepath"
)

// Dir reuses existing data so a rename preserves browser sessions and HLS caches.
func Dir(base string) (string, error) {
	for _, name := range []string{"kuraku-downloader", "JavIt-DL"} {
		path := filepath.Join(base, name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("application data path is not a directory: %s", path)
		}
		return path, nil
	}
	return filepath.Join(base, "kuraku-downloader"), nil
}
