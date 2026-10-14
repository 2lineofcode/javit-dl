package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildPlatformTags(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("build script uses a POSIX shell")
	}
	script, err := os.ReadFile("build.sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, os, webkit, gtk, tags string
		fail                        bool
	}{
		{"mac", "darwin", "", "", "production", false},
		{"webkit41", "linux", "4.1", "yes", "production,webkit2_41", false},
		{"webkit40", "linux", "4.0", "yes", "production", false},
		{"missing-webkit", "linux", "", "yes", "", true},
		{"missing-gtk", "linux", "4.1", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"build.sh":   string(script),
				"go":         "#!/bin/sh\nif [ \"$1\" = env ]; then printf '%s' \"$TEST_BUILD_OS\"; else printf '%s\\n' \"$@\"; fi\n",
				"npm":        "#!/bin/sh\nexit 0\n",
				"pkg-config": "#!/bin/sh\ncase \"$2\" in\n gtk+-3.0) [ \"$TEST_BUILD_GTK\" = yes ] ;;\n webkit2gtk-*) [ \"$2\" = \"webkit2gtk-$TEST_BUILD_WEBKIT\" ] ;;\n *) exit 1 ;;\nesac\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(dir, "frontend", "dist"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TEST_BUILD_OS", tc.os)
			t.Setenv("TEST_BUILD_WEBKIT", tc.webkit)
			t.Setenv("TEST_BUILD_GTK", tc.gtk)
			output, err := exec.Command("/bin/sh", filepath.Join(dir, "build.sh")).CombinedOutput()
			if tc.fail {
				if err == nil || !strings.Contains(string(output), "Linux GUI build requires") {
					t.Fatalf("missing dependency: %v, %s", err, output)
				}
			} else if err != nil || !strings.Contains(string(output), "-tags\n"+tc.tags+"\n") {
				t.Fatalf("build tags: %v, %s", err, output)
			}
			if !tc.fail && !strings.Contains(string(output), "-o\nkuraku\n.") {
				t.Fatalf("CLI binary name: %s", output)
			}
		})
	}
}
