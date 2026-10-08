//go:build unix

package downloader

import (
	"bufio"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestInheritedStdinCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd, err := unix.Dup(int(reader.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	input := os.NewFile(uintptr(fd), "inherited-stdin")
	defer input.Close()
	if _, err := writer.Write([]byte("partial")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := bufio.NewReader(contextualFile(ctx, input)).ReadString('\n'); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("inherited stdin read remained blocked")
	}
}
