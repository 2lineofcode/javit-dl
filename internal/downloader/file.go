package downloader

import (
	"fmt"
	"os"
	"path/filepath"
)

// Publish a complete file without ever replacing an existing destination, even in a race.
// Staging on the destination filesystem makes the hard link atomic.
func publish(out string, write func(*os.File) error) error {
	f, err := os.CreateTemp(filepath.Dir(out), ".javit-dl-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	return publishFile(f, out)
}

// Publish a completed staging file without copying or replacing the destination.
func publishFile(f *os.File, out string) error {
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), out); err != nil {
		return fmt.Errorf("publish %s: %w", out, err)
	}
	return nil
}
