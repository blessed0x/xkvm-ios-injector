#!/bin/bash
# check-smoke.sh — generality smoke for `xkvm check`.
#
# Builds ten hermetic fixture IPAs and asserts the exit code `xkvm check`
# returns on each. Every case uses fully generic app/tweak/framework names
# (StockApp / CoolTweak / MissingMedia — no Instagram-family naming), so CI
# proves the merge-completeness check is app-agnostic without needing the
# real downloaded mod IPAs. Runs on the macos-14 leg (needs go + Xcode
# toolchain for the Mach-O fixtures).
#
# Cases cover every check mechanism:
#   1-3  clean stock apps                              -> exit 0
#   4-5  load-command gap (tier 1): missing framework  -> 1 ; shipped -> 0
#   6-7  dlopen gap (tier 2): bare token missing       -> 1 ; shipped -> 0
#   8    same-family nested dlopen (resolves)          -> 0
#   9    cross-family nested dlopen (unreachable)      -> 1
#   10   OS Swift runtime shim allowlisted             -> 0
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1
X="$ROOT/bin/xkvm"
T=$(mktemp -d /tmp/xkvm-check-smoke-XXXX)
trap 'rm -rf "$T"' EXIT

echo "== building xkvm =="
go build -o "$X" ./cmd/xkvm || exit 1

PASS=0
FAIL=0

# make_app NAME BUNDLEID DIR -> DIR/NAME.ipa (plain stock app)
make_app() {
  local name="$1" bid="$2" dir="$3"
  mkdir -p "$dir/Payload/$name.app"
  printf 'package main\nfunc main() {}\n' > "$dir/a.go"
  ( cd "$dir" && go build -o "Payload/$name.app/$name" a.go )
  printf '<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleExecutable</key><string>%s</string><key>CFBundleIdentifier</key><string>%s</string></dict></plist>' "$name" "$bid" > "$dir/Payload/$name.app/Info.plist"
  ( cd "$dir" && zip -qr "$name.ipa" Payload )
}

# mkdylib NAME DIR -> DIR/NAME.dylib (Go c-shared dylib, also writes x.go)
mkdylib() {
  local name="$1" dir="$2"
  printf 'package main\nfunc main() {}\n' > "$dir/x.go"
  ( cd "$dir" && go build -buildmode=c-shared -o "$name.dylib" x.go )
}

# make_framework NAME INSTALL_NAME DIR -> DIR/NAME.framework/NAME whose
# LC_ID_DYLIB is the @rpath install name a real tweak's load command records
make_framework() {
  local name="$1" install="$2" dir="$3"
  printf 'package main\nfunc main() {}\n' > "$dir/x.go"
  mkdir -p "$dir/$name.framework"
  ( cd "$dir" && go build -buildmode=c-shared -o "$name.framework/$name" x.go )
  xcrun install_name_tool -id "$install" "$dir/$name.framework/$name"
}

# weak_tweak NAME INSTALL_NAME FRAMEWORK_PATH DIR -> DIR/NAME.dylib with an
# LC_LOAD_WEAK_DYLIB referencing the install name (the tier-1 signature)
weak_tweak() {
  local name="$1" install="$2" fwpath="$3" dir="$4"
  printf 'int xkvm_smoke_%s = 1;\n' "$name" > "$dir/w.c"
  ( cd "$dir" && xcrun clang -dynamiclib -Wl,-undefined,dynamic_lookup -Wl,-weak_library,"$fwpath" -o "$name.dylib" w.c )
}

# dlopen_tweak NAME FRAMEWORK DIR -> DIR/NAME.dylib with a bare
# FRAMEWORK.framework/FRAMEWORK token COMPILED into __TEXT (referenced from
# init() so the linker keeps it; NUL-prefixed so the token sits at a printable
# run start). Compiled, not appended: xkvm's injection + fakesign rewrite the
# Mach-O, which drops trailing bytes but preserves __TEXT — the realistic
# case, since real tweaks' dlopen strings are compiled into the binary.
dlopen_tweak() {
  local name="$1" fw="$2" dir="$3"
  cat > "$dir/dlopen.go" <<EOF
package main

import "os"

func init() { _ = os.Getenv("\x00$fw.framework/$fw") }

func main() {}
EOF
  ( cd "$dir" && go build -buildmode=c-shared -o "$name.dylib" dlopen.go )
}

# check NAME IPA WANT_EXIT
check() {
  local name="$1" ipa="$2" want="$3"
  "$X" check -i "$ipa" > "$T/last.log" 2>&1
  local got=$?
  if [ "$got" = "$want" ]; then
    echo "PASS  $name (exit $got)"
    PASS=$((PASS+1))
  else
    echo "FAIL  $name (exit $got, want $want)"
    FAIL=$((FAIL+1))
  fi
}

# check_flags NAME IPA WANT_EXIT FRAMEWORK  (also asserts the name is named)
check_flags() {
  local name="$1" ipa="$2" want="$3" fw="$4"
  "$X" check -i "$ipa" > "$T/last.log" 2>&1
  local got=$?
  if [ "$got" = "$want" ] && grep -q "$fw" "$T/last.log"; then
    echo "PASS  $name (exit $got, flags $fw)"
    PASS=$((PASS+1))
  else
    echo "FAIL  $name (exit $got, want $want, flags $fw)"
    FAIL=$((FAIL+1))
  fi
}

echo "== building 10 fixture IPAs =="

# 1-3. Clean stock apps — different names/bundle ids.
make_app StockApp    com.example.stock   "$T/c1"
make_app NewsApp     com.example.news    "$T/c2"
make_app BrowserLite com.example.browser "$T/c3"

# 4-5. Tier-1 load-command gap: CoolTweak weakly references MissingMedia.
D=$T/c4
make_app MediaApp com.example.media "$D"
mkdylib stub "$D"
make_framework MissingMedia "@rpath/MissingMedia.framework/MissingMedia" "$D"
weak_tweak CoolTweak "@rpath/MissingMedia.framework/MissingMedia" "$D/MissingMedia.framework/MissingMedia" "$D"
"$X" -i "$D/MediaApp.ipa" -o "$D/gap.ipa" -f "$D/CoolTweak.dylib" -s > /dev/null 2>&1
"$X" -i "$D/MediaApp.ipa" -o "$D/complete.ipa" -f "$D/CoolTweak.dylib" -f "$D/MissingMedia.framework" -s > /dev/null 2>&1

# 6-7. Tier-2 dlopen gap: CheatEngine carries a bare HacksKit.framework token.
D=$T/c6
make_app GameApp com.example.game "$D"
dlopen_tweak CheatEngine HacksKit "$D"
mkdylib stub "$D"
mkdir -p "$D/HacksKit.framework"
( cd "$D" && go build -buildmode=c-shared -o "HacksKit.framework/HacksKit" x.go )
"$X" -i "$D/GameApp.ipa" -o "$D/gap.ipa" -f "$D/CheatEngine.dylib" -s > /dev/null 2>&1
"$X" -i "$D/GameApp.ipa" -o "$D/complete.ipa" -f "$D/CheatEngine.dylib" -f "$D/HacksKit.framework" -s > /dev/null 2>&1

# 8. Same-family nested dlopen: ThemeTweak (root) loads BadgeKit from its own
#    ThemeTweak.bundle (real tweaks share the dylib/bundle stem — Regram.dylib
#    + Regram.bundle) — same family, reachable, must resolve.
D=$T/c8
make_app ThemeApp com.example.theme "$D"
dlopen_tweak ThemeTweak BadgeKit "$D"
mkdylib stub "$D"
mkdir -p "$D/ThemeTweak.bundle/BadgeKit.framework"
( cd "$D" && go build -buildmode=c-shared -o "ThemeTweak.bundle/BadgeKit.framework/BadgeKit" x.go )
"$X" -i "$D/ThemeApp.ipa" -o "$D/out.ipa" -f "$D/ThemeTweak.dylib" --root-dylib "$D/ThemeTweak.dylib" -f "$D/ThemeTweak.bundle" -s > /dev/null 2>&1

# 9. Cross-family nested dlopen: WidgetTweak (Frameworks) dlopens BadgeKit
#    nested only in ThemeTweak.bundle — a different family, unreachable, flagged.
D=$T/c9
make_app ThemeApp2 com.example.theme2 "$D"
dlopen_tweak WidgetTweak BadgeKit "$D"
mkdylib stub "$D"
mkdir -p "$D/ThemeTweak.bundle/BadgeKit.framework"
( cd "$D" && go build -buildmode=c-shared -o "ThemeTweak.bundle/BadgeKit.framework/BadgeKit" x.go )
"$X" -i "$D/ThemeApp2.ipa" -o "$D/out.ipa" -f "$D/WidgetTweak.dylib" -f "$D/ThemeTweak.bundle" -s > /dev/null 2>&1

# 10. OS Swift runtime shim: @rpath/libswiftFoundation.dylib must be exempt.
D=$T/c10
make_app SwiftApp com.example.swift "$D"
mkdylib stub "$D"
( cd "$D" && go build -buildmode=c-shared -o "libswiftFoundation.dylib" x.go )
xcrun install_name_tool -id "@rpath/libswiftFoundation.dylib" "$D/libswiftFoundation.dylib"
weak_tweak SwiftTweak "@rpath/libswiftFoundation.dylib" "$D/libswiftFoundation.dylib" "$D"
"$X" -i "$D/SwiftApp.ipa" -o "$D/out.ipa" -f "$D/SwiftTweak.dylib" -s > /dev/null 2>&1

echo "== running xkvm check =="
check        "1  StockApp clean"              "$T/c1/StockApp.ipa"    0
check        "2  NewsApp clean"               "$T/c2/NewsApp.ipa"     0
check        "3  BrowserLite clean"           "$T/c3/BrowserLite.ipa" 0
check_flags  "4  MediaApp load gap"           "$T/c4/gap.ipa"         1 MissingMedia
check        "5  MediaApp load shipped"       "$T/c4/complete.ipa"    0
check_flags  "6  GameApp dlopen gap"          "$T/c6/gap.ipa"         1 HacksKit
check        "7  GameApp dlopen shipped"      "$T/c6/complete.ipa"    0
check        "8  ThemeApp same-family nested" "$T/c8/out.ipa"         0
check_flags  "9  ThemeApp2 cross-family"      "$T/c9/out.ipa"         1 BadgeKit
check        "10 SwiftApp shim allowlist"     "$T/c10/out.ipa"        0

echo "== $PASS passed, $FAIL failed =="
[ "$FAIL" -eq 0 ]
