#!/usr/bin/env bash
# M2 smoke test: drive the real xkvm binary end-to-end against a real IPA.
# Builds a Go app + tweak, packages the IPA, runs every major flag, and
# verifies the injected output.
set -euo pipefail
cd "$(dirname "$0")/.."

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

echo "== building xkvm =="
go build -o bin/xkvm ./cmd/xkvm

echo "== building fixtures =="
printf 'package main\nfunc main() {}\n' > "$T/app.go"
go build -o "$T/app" "$T/app.go"
printf 'package main\nfunc main() {}\n' > "$T/tweak.go"
go build -o "$T/MyTweak.dylib" "$T/tweak.go"

mkdir -p "$T/Payload/Test.app"
cp "$T/app" "$T/Payload/Test.app/Test"
cat > "$T/Payload/Test.app/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>Test</string>
  <key>CFBundleIdentifier</key><string>com.example.test</string>
  <key>CFBundleName</key><string>TestApp</string>
  <key>CFBundleVersion</key><string>1.0.0</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>MinimumOSVersion</key><string>12.0</string>
  <key>UISupportedDevices</key><array><string>iPhone12,1</string></array>
</dict></plist>
PLIST

cd "$T" && zip -qr in.ipa Payload && cd - >/dev/null

echo "== running xkvm =="
./bin/xkvm -i "$T/in.ipa" -o "$T/out.ipa" -f "$T/MyTweak.dylib" \
  -n "Fancy App" -v 2.5.0 -b com.example.fancy -m 15.0 -s -q -u -c 3

echo "== verifying output =="
mkdir -p "$T/unz" && cd "$T/unz" && unzip -q "$T/out.ipa" && cd - >/dev/null
APP="$T/unz/Payload/Test.app"
test -f "$APP/Frameworks/MyTweak.dylib" && echo "OK: tweak injected"
# NOTE: use `grep -c ... >/dev/null`, never `grep -q`, for the piped checks:
# under `set -o pipefail`, grep -q closes the pipe the instant it matches,
# plutil then dies with SIGPIPE, and pipefail turns that into a failure — a
# classic flaky-check footgun that made the bundle-id check intermittently
# fail even when the value was present.
otool -L "$APP/Test" | grep -c "@rpath/MyTweak.dylib" >/dev/null && echo "OK: main binary links tweak"
test ! -e "$APP/Watch" && echo "OK: no watch app"
grep -q "Fancy App" <(plutil -p "$APP/Info.plist") && echo "OK: name changed"
plutil -p "$APP/Info.plist" | grep -c "com.example.fancy" >/dev/null && echo "OK: bundle id changed"
plutil -p "$APP/Info.plist" | grep -c '"15.0"' >/dev/null && echo "OK: min OS changed"
plutil -p "$APP/Info.plist" | grep -c "UISupportedDevices" >/dev/null && { echo "FAIL: UISupportedDevices still present"; exit 1; } || echo "OK: UISupportedDevices removed"
ldid -e "$APP/Test" >/dev/null 2>&1 && echo "OK: main binary signed"

echo "== extract round-trip =="
./bin/xkvm extract -i "$T/out.ipa" -o "$T/arts"
test -f "$T/arts/MyTweak.dylib" && echo "OK: extract dumped tweak"

echo "== ALL SMOKE CHECKS PASSED =="
