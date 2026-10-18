#!/usr/bin/env bash
# Builds build/go/TetherDesktop.exe: the Windows tray app, 1 file with no
# runtime to install. Runs on macOS, Linux or Windows.
#
#   TETHER_VERSION=0.2.0  the version in the exe (the default)
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
cd "$root/go/cmd/tetherdesktop"
# Icon, manifest and version info (winres/winres.json) as a .syso file.
version=${TETHER_VERSION:-0.2.0}
# The oldest version that may still run: see scripts/min-version.
min_version=$(tr -d '[:space:]' < "$root/scripts/min-version")
go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64 \
    --product-version "$version" --file-version "$version"
cd "$root/go"
GOOS=windows GOARCH=amd64 go build -trimpath -o "$root/build/go/TetherDesktop.exe" -ldflags "-s -w -H windowsgui \
    -X github.com/freeyoung/tether-core-go/core.Version=$version -X github.com/freeyoung/tether-core-go/core.MinPeerVersion=$min_version" ./cmd/tetherdesktop
echo "built $root/build/go/TetherDesktop.exe"
