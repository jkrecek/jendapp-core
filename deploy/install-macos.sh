#!/bin/sh
# Build, sign and (re)start the server on this Mac.
#
# Signing with a stable identity + identifier keeps macOS privacy grants (Full Disk Access,
# iCloud Drive) across rebuilds; an ad-hoc signed binary loses them every time it changes.
#   SIGN_IDENTITY="Apple Development: …" deploy/install-macos.sh
set -eu
cd "$(dirname "$0")/../server"

IDENTITY=${SIGN_IDENTITY:-$(security find-identity -v -p codesigning | sed -n 's/.*"\(Apple Development:[^"]*\)".*/\1/p' | head -1)}
[ -n "$IDENTITY" ] || { echo "no Apple Development signing identity found; set SIGN_IDENTITY" >&2; exit 1; }

go build -o bin/jendapp ./cmd/jendapp
codesign --force --sign "$IDENTITY" --identifier dev.jendapp.server --timestamp=none bin/jendapp
mkdir -p ~/.jendapp/bin
cp bin/jendapp ~/.jendapp/bin/jendapp.new && mv ~/.jendapp/bin/jendapp.new ~/.jendapp/bin/jendapp

PLIST=~/Library/LaunchAgents/dev.jendapp.server.plist
if [ ! -f "$PLIST" ]; then
	sed "s|__HOME__|$HOME|g" ../deploy/dev.jendapp.server.plist > "$PLIST"
	launchctl bootstrap "gui/$(id -u)" "$PLIST"
else
	launchctl kickstart -k "gui/$(id -u)/dev.jendapp.server"
fi
codesign -dr - ~/.jendapp/bin/jendapp 2>&1 | sed -n 's/^designated => /signed: /p'
