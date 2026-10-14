package downloader

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestFFmpegHLSOptions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake executable uses a POSIX shell")
	}
	bin := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$TEST_HLS_HELP\"\nexit \"$TEST_HLS_EXIT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_HLS_EXIT", "0")
	base := []string{"-protocol_whitelist", "file,crypto", "-allowed_extensions", "ALL"}
	for _, tc := range []struct {
		name, help string
		extra      []string
	}{
		{"old", "-allowed_extensions <string>", nil},
		{"picky", "-extension_picky <boolean>", []string{"-extension_picky", "0"}},
		{"segments", "-allowed_segment_extensions <string>", []string{"-allowed_segment_extensions", "ALL"}},
		{"new", "-allowed_segment_extensions <string>\n-extension_picky <boolean>", []string{"-allowed_segment_extensions", "ALL", "-extension_picky", "0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TEST_HLS_HELP", tc.help)
			got, err := ffmpegHLSOptions(context.Background(), bin)
			want := append(append([]string{}, base...), tc.extra...)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("options = %v, %v; want %v", got, err, want)
			}
		})
	}
	t.Setenv("TEST_HLS_EXIT", "1")
	if _, err := ffmpegHLSOptions(context.Background(), bin); err == nil {
		t.Fatal("failed inspection must return an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ffmpegHLSOptions(ctx, bin); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled inspection: %v", err)
	}
}
