package appdata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		dirs []string
		want string
	}{
		{"fresh", nil, "kuraku-downloader"},
		{"renamed", []string{"kuraku-downloader"}, "kuraku-downloader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			for _, dir := range tc.dirs {
				if err := os.Mkdir(filepath.Join(base, dir), 0700); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Dir(base)
			if err != nil || got != filepath.Join(base, tc.want) {
				t.Fatalf("Dir = %q, %v", got, err)
			}
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != len(tc.dirs) {
				t.Fatal("data directories changed")
			}
		})
	}
}

func TestDirRejectsFile(t *testing.T) {
	for _, name := range []string{"kuraku-downloader"} {
		base := t.TempDir()
		if err := os.WriteFile(filepath.Join(base, name), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Dir(base); err == nil {
			t.Fatalf("accepted file %s as data directory", name)
		}
	}
}
