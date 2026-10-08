//go:build !unix

package downloader

import (
	"context"
	"io"
)

func contextualFile(_ context.Context, input io.Reader) io.Reader { return input }
