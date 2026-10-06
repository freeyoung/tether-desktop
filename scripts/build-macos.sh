#!/usr/bin/env bash
# Builds build/TetherDesktop.app (Apple silicon) from go/cmd/tetherdesktop
# and signs it.
#
#   TETHER_REQUIRE_SIGNING=1  fail rather than sign ad hoc
#
# The bundle is signed with the self-signed "Tether Desktop Local Code
# Signing" certificate when the login keychain has it. A fixed certificate
# keeps the app's identity across builds, so macOS keeps its Local Network
# permission. Ad hoc signing works too, but then each build asks again.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(dirname "$here")
cd "$root"

version=${TETHER_VERSION:-0.2.0}
# The oldest version that may still run: see scripts/min-version.
min_version=$(tr -d '[:space:]' < "$here/min-version")
# The build number grows with each commit. macOS keys cached app icons (for
# notifications, for example) to the bundle version, so a new icon needs it.
build=$(git -C "$root" rev-list --count HEAD 2>/dev/null || echo 1)
app="$root/build/TetherDesktop.app"
bin="$root/build/go/TetherDesktop-darwin"

(cd "$root/go" && CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 MACOSX_DEPLOYMENT_TARGET=15.0 \
    CGO_CFLAGS="-O2 -mmacosx-version-min=15.0" \
    go build -trimpath -o "$bin" -ldflags "-s -w -X tetherdesktop/core.Version=$version \
    -X tetherdesktop/core.MinPeerVersion=$min_version" ./cmd/tetherdesktop)

rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$bin" "$app/Contents/MacOS/TetherDesktop"
# scripts/make-icons.swift draws it.
cp "$root/assets/AppIcon.icns" "$app/Contents/Resources/AppIcon.icns"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key><string>local.TetherDesktop</string>
    <key>CFBundleName</key><string>Tether Desktop</string>
    <key>CFBundleExecutable</key><string>TetherDesktop</string>
    <key>CFBundleIconFile</key><string>AppIcon</string>
    <key>CFBundlePackageType</key><string>APPL</string>
    <key>CFBundleShortVersionString</key><string>$version</string>
    <key>NSHumanReadableCopyright</key><string>© 2026 Eric Qian. MIT License.</string>
    <key>CFBundleVersion</key><string>$build</string>
    <key>LSMinimumSystemVersion</key><string>15.0</string>
    <key>LSUIElement</key><true/>
    <key>NSLocalNetworkUsageDescription</key>
    <string>Tether Desktop talks to your iPhone and your other computers on the local network.</string>
    <key>NSBonjourServices</key>
    <array><string>_tether._tcp</string></array>
    <key>NSServices</key>
    <array>
        <dict>
            <key>NSMenuItem</key>
            <dict><key>default</key><string>Send with Tether Desktop</string></dict>
            <key>NSMessage</key><string>sendFiles</string>
            <key>NSPortName</key><string>Tether Desktop</string>
            <key>NSSendFileTypes</key>
            <array><string>public.item</string></array>
            <key>NSRequiredContext</key><dict/>
        </dict>
    </array>
</dict>
</plist>
PLIST

identity=C37910EA5BF5F7F0FB465FF84844B066627C0D65
if security find-identity -p codesigning 2>/dev/null | grep -q "$identity" &&
    codesign --force --sign "$identity" "$app"; then
    echo "signed with Tether Desktop Local Code Signing"
else
    if [ "${TETHER_REQUIRE_SIGNING:-}" = 1 ]; then
        echo "error: could not sign with the Tether Desktop Local Code Signing certificate" >&2
        exit 1
    fi
    echo "warning: signed ad hoc: macOS will ask for Local Network access after every build" >&2
    codesign --force --sign - "$app"
fi
echo "built $app"
