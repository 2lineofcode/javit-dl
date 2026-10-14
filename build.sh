#!/bin/sh
set -eu
cd "$(dirname "$0")"
tags=production
if [ "$(go env GOOS)" = linux ]; then
    if ! command -v pkg-config >/dev/null 2>&1 || ! pkg-config --exists gtk+-3.0 gio-unix-2.0; then
        echo 'Linux GUI build requires pkg-config and GTK3 development libraries (Debian/Ubuntu: sudo apt install build-essential pkg-config libgtk-3-dev).' >&2
        exit 1
    fi
    if pkg-config --exists webkit2gtk-4.1; then
        tags="$tags,webkit2_41"
    elif ! pkg-config --exists webkit2gtk-4.0; then
        echo 'Linux GUI build requires WebKitGTK development libraries (Debian/Ubuntu: sudo apt install libwebkit2gtk-4.1-dev).' >&2
        exit 1
    fi
fi
npm --prefix frontend ci
npm --prefix frontend run build
touch frontend/dist/.gitkeep
go build -tags "$tags" -trimpath -ldflags '-s -w' -o kuraku .
