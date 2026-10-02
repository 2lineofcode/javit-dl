package gui

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/mac"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

func Run(ctx context.Context, assets fs.FS, initial map[string]any) error {
	if _, err := fs.Stat(assets, "index.html"); err != nil {
		return fmt.Errorf("GUI assets are missing; run ./build.sh first")
	}
	a := newApp(ctx)
	a.initial = initial
	if _, err := a.Defaults(); err != nil {
		return err
	}
	err := wails.Run(&options.App{
		Title: "JavIt Downloader", Width: 600, Height: 500, DisableResize: true,
		BackgroundColour: options.NewRGB(17, 20, 27), AssetServer: &assetserver.Options{Assets: assets},
		Mac:  &mac.Options{Appearance: mac.NSAppearanceNameDarkAqua, TitleBar: mac.TitleBarDefault(), DisableZoom: true},
		Bind: []interface{}{a},
		OnStartup: func(wctx context.Context) {
			a.mu.Lock()
			a.windowCtx = wctx
			a.mu.Unlock()
			context.AfterFunc(ctx, func() { runtime.Quit(wctx) })
		},
		OnShutdown: func(context.Context) { a.shutdown() },
	})
	if err != nil {
		return fmt.Errorf("%w; build the desktop binary with ./build.sh", err)
	}
	return nil
}

func (a *App) SavePath(current string) (string, error) {
	a.mu.Lock()
	ctx := a.windowCtx
	a.mu.Unlock()
	if ctx == nil {
		return "", fmt.Errorf("desktop window is not ready")
	}
	return runtime.SaveFileDialog(ctx, runtime.SaveDialogOptions{DefaultDirectory: filepath.Dir(current), DefaultFilename: filepath.Base(current), Title: "Save download as", CanCreateDirectories: true})
}
