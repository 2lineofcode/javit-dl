package appdata

import (
	"fmt"
	"os"
	"path/filepath"
)

func Dir(base string) (string, error) {
	path := filepath.Join(base, "kuraku-downloader")
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return path, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("application data path is not a directory: %s", path)
	}
	return path, nil
}
