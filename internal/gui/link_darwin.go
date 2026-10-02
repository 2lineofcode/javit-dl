//go:build darwin && (production || dev)

package gui

// #cgo LDFLAGS: -framework UniformTypeIdentifiers
import "C"

// Wails 2.16 uses UTType in native dialogs but omits its framework from linker flags.
