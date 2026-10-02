#!/bin/sh
set -eu
cd "$(dirname "$0")"
npm --prefix frontend ci
npm --prefix frontend run build
touch frontend/dist/.gitkeep
go build -tags production -trimpath -ldflags '-s -w' -o javit_dl .
