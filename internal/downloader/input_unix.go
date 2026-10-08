//go:build unix

package downloader

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Inherited stdin can be a blocking descriptor that Close cannot interrupt.
// Poll before each read so cancellation also works with partial piped lines.
func contextualFile(ctx context.Context, input io.Reader) io.Reader {
	if file, ok := input.(*os.File); ok {
		return &fileInput{ctx: ctx, file: file}
	}
	return input
}

type fileInput struct {
	ctx  context.Context
	file *os.File
}

func (r *fileInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	fd := []unix.PollFd{{Fd: int32(r.file.Fd()), Events: unix.POLLIN}}
	for {
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		count, err := unix.Poll(fd, 100)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return 0, err
		}
		if count == 0 {
			continue
		}
		if err := r.ctx.Err(); err != nil {
			return 0, err
		}
		return r.file.Read(p)
	}
}
