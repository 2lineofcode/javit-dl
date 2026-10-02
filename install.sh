#!/bin/sh
set -eu
cd "$(dirname "$0")"
./build.sh
bin_dir=$(go env GOBIN)
if [ -z "$bin_dir" ]; then
    go_path=$(go env GOPATH)
    bin_dir="${go_path%%:*}/bin"
fi
mkdir -p "$bin_dir"
install -m 755 javit_dl "$bin_dir/javit_dl"
printf 'Installed %s/javit_dl\n' "$bin_dir"
