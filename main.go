package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"syscall"

	"kuraku-downloader/internal/cli"
	"kuraku-downloader/internal/gui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cli.RunWithGUI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, func(ctx context.Context, initial map[string]any) error {
		assets, err := fs.Sub(frontendAssets, "frontend/dist")
		if err != nil {
			return err
		}
		return gui.Run(ctx, assets, initial)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
