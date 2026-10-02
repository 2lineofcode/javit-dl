package downloader

import (
	"fmt"
	"os"
	"path/filepath"
)

// Stage on the destination filesystem; replace only after explicit confirmation.
func publish(out string, overwrite bool, write func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(out), ".javit-dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	return publishFile(f, out, overwrite)
}

// Hard-link new files without clobbering; atomically rename confirmed replacements.
func publishFile(f *os.File, out string, overwrite bool) error {
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if overwrite {
		if err := os.Rename(f.Name(), out); err != nil {
			return fmt.Errorf("replace %s: %w", out, err)
		}
		return nil
	}
	if err := os.Link(f.Name(), out); err != nil {
		return fmt.Errorf("publish %s: %w", out, err)
	}
	return nil
}
