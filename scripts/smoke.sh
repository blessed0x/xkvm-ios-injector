#!/usr/bin/env bash
# End-to-end smoke test: build xkvm, repack a fixture IPA, extract tweaks.
set -euo pipefail
cd "$(dirname "$0")/.."

echo '=== BUILD ==='
go build -o bin/xkvm ./cmd/xkvm

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT

echo '=== FIXTURE ==='
mkdir -p "$T/Payload/Test.app/Frameworks"
cat > "$T/Payload/Test.app/Info.plist" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>Test</string>
  <key>CFBundleIdentifier</key><string>com.example.test</string>
  <key>CFBundleName</key><string>TestApp</string>
</dict></plist>
EOF
printf 'pseudo-executable' > "$T/Payload/Test.app/Test"
printf 'dylib-bytes' > "$T/Payload/Test.app/Frameworks/MyTweak.dylib"
(cd "$T" && zip -qr in.ipa Payload)

echo '=== RUN: repack (compression level 3) ==='
./bin/xkvm -i "$T/in.ipa" -o "$T/out.ipa" -c 3

echo '=== VERIFY: output zip entries ==='
unzip -l "$T/out.ipa"

echo '=== RUN: extract tweaks from the output ==='
./bin/xkvm extract -i "$T/out.ipa" -o "$T/tweaks"
ls -la "$T/tweaks"

echo '=== SMOKE OK ==='
