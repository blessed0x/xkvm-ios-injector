#!/bin/bash
# e2e-real.sh — test xkvm against a REAL iOS app IPA and a REAL tweak .deb,
# both built from source with Apple's own SDK toolchain (no Go fixtures).
set -euo pipefail
cd "$(dirname "$0")/.."
XKVM=./bin/xkvm
for t in xcrun clang ldid plutil zip unzip dpkg-deb codesign; do
    command -v "$t" >/dev/null 2>&1 || { echo "e2e-real.sh requires $t (not found)"; exit 1; }
done
SDK=$(xcrun --sdk iphoneos --show-sdk-path)
[ -n "$SDK" ] || { echo "e2e-real.sh requires the iOS SDK (Xcode)"; exit 1; }
MINIOS=13.0
ARCH=arm64

T=$(mktemp -d)
trap 'rm -rf "$T"' EXIT
echo "workspace: $T"

# Manual real-device checklist (out of scope for automation — no device in
# this environment; run after the script passes):
#   1. Install Out-ellekit.ipa with a sideloader (AltStore/SideStore/TrollStore).
#   2. Confirm the injected tweak loads (look for its NSLog in Console.app).
#   3. On iOS 26: confirm the Liquid Glass appearance for Out-liquid.ipa and the
#      legacy appearance for Out-liquid-compat.ipa.
#   4. Confirm Out-fullscreen.ipa runs full-screen (no Split View / Slide Over)
#      in the iPad multitasking picker.

echo
echo "=== [1/10] build real iOS app (arm64, UIKit) with Apple SDK ==="
cat > "$T/main.m" <<'EOF'
#import <UIKit/UIKit.h>
@interface AppDelegate : UIResponder <UIApplicationDelegate> @end
@implementation AppDelegate
- (BOOL)application:(UIApplication *)application didFinishLaunchingWithOptions:(NSDictionary *)launchOptions {
    return YES;
}
@end
int main(int argc, char *argv[]) {
    @autoreleasepool {
        return UIApplicationMain(argc, argv, nil, NSStringFromClass([AppDelegate class]));
    }
}
EOF
xcrun -sdk iphoneos clang -arch "$ARCH" -miphoneos-version-min="$MINIOS" \
    -fobjc-arc -O2 "$T/main.m" -framework UIKit -framework Foundation \
    -o "$T/RealApp"
file "$T/RealApp"

APP="$T/RealApp.app"
mkdir -p "$APP"
cp "$T/RealApp" "$APP/RealApp"
cat > "$T/Info.plist.tpl" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleExecutable</key><string>RealApp</string>
  <key>CFBundleIdentifier</key><string>com.example.RealApp</string>
  <key>CFBundleName</key><string>RealApp</string>
  <key>CFBundleDisplayName</key><string>RealApp</string>
  <key>CFBundleVersion</key><string>1.0.0</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>MinimumOSVersion</key><string>13.0</string>
  <key>UILaunchScreen</key><dict/>
</dict></plist>
EOF
plutil -convert binary1 "$T/Info.plist.tpl" -o "$APP/Info.plist"
# ad-hoc sign the app binary so it's a realistic signed, unencrypted app
ldid -S "$APP/RealApp"

ROOT="$T/ipa-root"
mkdir -p "$ROOT/Payload"
mv "$APP" "$ROOT/Payload/RealApp.app"
( cd "$ROOT" && zip -qrX "$T/RealApp.ipa" Payload )
echo "RealApp.ipa: $(ls -la "$T/RealApp.ipa" | awk '{print $5}') bytes"
unzip -l "$T/RealApp.ipa"

echo
echo "=== [2/10] build real tweak dylib (Cydia-style) ==="
cat > "$T/CoolTweak.m" <<'EOF'
#import <Foundation/Foundation.h>
__attribute__((constructor))
static void CoolTweakInit(void) {
    NSLog(@"[CoolTweak] loaded into %@", NSProcessInfo.processInfo.processName);
}
EOF
xcrun -sdk iphoneos clang -arch "$ARCH" -miphoneos-version-min="$MINIOS" \
    -dynamiclib -install_name @rpath/CoolTweak.dylib -O2 \
    "$T/CoolTweak.m" -framework Foundation \
    -o "$T/CoolTweak.dylib"
printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>get-task-allow</key><true/></dict></plist>\n' > "$T/ents.plist"
ldid -S"$T/ents.plist" "$T/CoolTweak.dylib"
file "$T/CoolTweak.dylib"
otool -L "$T/CoolTweak.dylib" | head -4

echo
echo "=== [3/10] package tweak as a real .deb ==="
DEBROOT="$T/deb-root"
mkdir -p "$DEBROOT/DEBIAN"
mkdir -p "$DEBROOT/Library/MobileSubstrate/DynamicLibraries"
cat > "$DEBROOT/DEBIAN/control" <<'EOF'
Package: com.example.cooltweak
Name: CoolTweak
Version: 1.0.0
Architecture: iphoneos-arm64
Maintainer: xkvm e2e <noreply@example.com>
Section: Tweaks
Depends: mobilesubstrate (>= 0.9.5000)
Description: A real tweak built for the xkvm end-to-end test
EOF
cp "$T/CoolTweak.dylib" "$DEBROOT/Library/MobileSubstrate/DynamicLibraries/"
cat > "$DEBROOT/Library/MobileSubstrate/DynamicLibraries/CoolTweak.plist" <<'EOF'
{ Filter = { Bundles = ( "com.example.RealApp" ); }; }
EOF
dpkg-deb -b --root-owner-group "$DEBROOT" "$T/CoolTweak.deb" >/dev/null
dpkg-deb -c "$T/CoolTweak.deb"

echo
echo "=== [4/10] xkvm inject dylib + deb + metadata, fakesign ==="
set -x
"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-dylib.ipa" -f "$T/CoolTweak.dylib" -s
"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-deb.ipa" -f "$T/CoolTweak.deb" -n "Fancy App" -b com.fancy.realapp -s
set +x

echo
echo "=== [5/10] verify dylib injection output ==="
mkdir -p "$T/check1" && ( cd "$T/check1" && unzip -q "$T/Out-dylib.ipa" )
APP1="$T/check1/Payload/RealApp.app"
echo "-- Frameworks listing --"
ls -la "$APP1/Frameworks/"
echo "-- main binary deps (otool) --"
otool -L "$APP1/RealApp" | grep -i cooltweak || { echo "FAIL: no CoolTweak in otool -L"; exit 1; }
echo "-- main binary deps (jtool2) --"
jtool2 -L "$APP1/RealApp" 2>/dev/null | grep -i cooltweak || echo "(jtool2 grep no-op, otool already confirmed)"
echo "-- codesign verify (embedded signature on a bare copy) --"
# codesign on a main-executable path inside .app validates bundle-level
# _CodeSignature/CodeResources, which ldid/AppSync-style fakesigning does not
# produce; verify the embedded signature on a bare copy instead.
cp "$APP1/RealApp" "$T/main-copy.bin"
codesign --verify --verbose=2 "$T/main-copy.bin" 2>&1; echo "codesign exit=$?"
cp "$APP1/Frameworks/CoolTweak.dylib" "$T/dylib-copy.bin"
codesign --verify --verbose=2 "$T/dylib-copy.bin" 2>&1; echo "dylib codesign exit=$?"

echo
echo "=== [6/10] verify deb injection + metadata, then extract ==="
mkdir -p "$T/check2" && ( cd "$T/check2" && unzip -q "$T/Out-deb.ipa" )
APP2="$T/check2/Payload/RealApp.app"
ls -la "$APP2/Frameworks/" | grep -i cooltweak
otool -L "$APP2/RealApp" | grep -i cooltweak
plutil -p "$APP2/Info.plist" | grep -E 'CFBundleDisplayName|CFBundleIdentifier'
cp "$APP2/RealApp" "$T/main-copy2.bin"
codesign --verify --verbose=2 "$T/main-copy2.bin" 2>&1; echo "codesign exit=$?"

"$XKVM" extract -i "$T/Out-dylib.ipa" -o "$T/arts"
echo "-- extracted artifacts --"
find "$T/arts" -type f | sort
test -f "$T/arts/CoolTweak.dylib" && echo "OK: extract recovered CoolTweak.dylib"

echo
echo "=== [7/10] xkvm --patch (bundled sideload fixes, fakesign implied) ==="
# No -s here: --patch must imply fakesign, or the injected dylibs would be
# unsigned and iOS would refuse to load them.
out=$("$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-patch.ipa" --patch 2>&1)
grep -q 'implies --fakesign' <<<"$out" || { echo "FAIL: --patch did not imply fakesign"; exit 1; }
mkdir -p "$T/check3" && ( cd "$T/check3" && unzip -q "$T/Out-patch.ipa" )
APP3="$T/check3/Payload/RealApp.app"
echo "-- Frameworks listing --"
ls "$APP3/Frameworks/"
for fix in sideloadFixerLol.dylib Sideloadbypass1.dylib Sideloadbypass2.dylib sideloadKeychainFix.dylib; do
  test -f "$APP3/Frameworks/$fix" || { echo "FAIL: $fix missing from Frameworks"; exit 1; }
done
loads=$(otool -L "$APP3/RealApp" | grep -cE '@rpath/(sideloadFixerLol|Sideloadbypass1|Sideloadbypass2|sideloadKeychainFix)')
test "$loads" -eq 4 || { echo "FAIL: expected 4 sideload loads, got $loads"; exit 1; }
echo "main binary has $loads/4 sideload load commands"
cp "$APP3/RealApp" "$T/patch-main.bin"
codesign --verify --verbose=2 "$T/patch-main.bin" 2>&1; echo "patch codesign exit=$?"
cp "$APP3/Frameworks/sideloadKeychainFix.dylib" "$T/patch-dylib.bin"
codesign --verify --verbose=2 "$T/patch-dylib.bin" 2>&1; echo "patch dylib codesign exit=$?"

echo
echo "=== [8/10] xkvm --ellekit (real ElleKit runtime, MobileSubstrate tweak) ==="
# Build a stub dylib whose install name is the classic MobileSubstrate
# absolute path, then link the tweak against it so otool -L shows the legacy
# spelling xkvm must rewrite to @rpath/ElleKit.framework/ElleKit.
cat > "$T/SubstrateStub.m" <<'EOF'
#import <Foundation/Foundation.h>
__attribute__((constructor)) static void SubstrateStubInit(void) {}
EOF
xcrun -sdk iphoneos clang -arch "$ARCH" -miphoneos-version-min="$MINIOS" \
    -dynamiclib -install_name /Library/MobileSubstrate/MobileSubstrate.dylib \
    -O2 "$T/SubstrateStub.m" -framework Foundation -o "$T/libSubstrateStub.dylib"
cat > "$T/SubstrateTweak.m" <<'EOF'
#import <Foundation/Foundation.h>
__attribute__((constructor)) static void SubstrateTweakInit(void) {
    NSLog(@"[SubstrateTweak] loaded");
}
EOF
xcrun -sdk iphoneos clang -arch "$ARCH" -miphoneos-version-min="$MINIOS" \
    -dynamiclib -install_name @rpath/SubstrateTweak.dylib -O2 \
    "$T/SubstrateTweak.m" -framework Foundation \
    -L "$T" -lSubstrateStub -o "$T/SubstrateTweak.dylib"
ldid -S"$T/ents.plist" "$T/SubstrateTweak.dylib"
# The tweak must really carry the legacy spelling in its load commands.
# Note: capture output to a variable first — with set -o pipefail, grep -q
# closing the pipe early SIGPIPEs the producer and makes the pipeline exit
# non-zero even on a successful match.
stub_deps=$(otool -L "$T/SubstrateTweak.dylib")
grep -q '/Library/MobileSubstrate/MobileSubstrate.dylib' <<<"$stub_deps" \
    || { echo "FAIL: SubstrateTweak.dylib lacks the legacy spelling"; exit 1; }

"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-ellekit.ipa" -f "$T/SubstrateTweak.dylib" -s --ellekit
mkdir -p "$T/check4" && ( cd "$T/check4" && unzip -q "$T/Out-ellekit.ipa" )
APP4="$T/check4/Payload/RealApp.app"
echo "-- Frameworks listing --"
ls -la "$APP4/Frameworks/"
test -f "$APP4/Frameworks/ElleKit.framework/ElleKit" || { echo "FAIL: ElleKit.framework missing"; exit 1; }
test ! -e "$APP4/Frameworks/CydiaSubstrate.framework" || { echo "FAIL: CydiaSubstrate.framework present in --ellekit output"; exit 1; }
# The fat (arm64+arm64e) vendored ElleKit must be thinned to the app's arm64.
archs=$(lipo -archs "$APP4/Frameworks/ElleKit.framework/ElleKit")
[ "$archs" = "arm64" ] || { echo "FAIL: ElleKit not thinned to app arch (got $archs)"; exit 1; }
# The tweak's MobileSubstrate dependency was rewritten to the ElleKit runtime.
check_deps=$(otool -L "$APP4/Frameworks/SubstrateTweak.dylib")
grep -q '@rpath/ElleKit.framework/ElleKit' <<<"$check_deps" \
    || { echo "FAIL: tweak dep not rewritten to ElleKit runtime"; exit 1; }
grep -q 'MobileSubstrate' <<<"$check_deps" \
    && { echo "FAIL: tweak still references MobileSubstrate"; exit 1; }
cp "$APP4/RealApp" "$T/ellekit-main.bin"
codesign --verify --verbose=2 "$T/ellekit-main.bin" 2>&1; echo "ellekit main codesign exit=$?"
cp "$APP4/Frameworks/SubstrateTweak.dylib" "$T/ellekit-tweak.bin"
codesign --verify --verbose=2 "$T/ellekit-tweak.bin" 2>&1; echo "ellekit tweak codesign exit=$?"
cp "$APP4/Frameworks/ElleKit.framework/ElleKit" "$T/ellekit-rt.bin"
codesign --verify --verbose=2 "$T/ellekit-rt.bin" 2>&1; echo "ellekit runtime codesign exit=$?"

echo
echo "=== [9/10] xkvm --liquid-glass (+ compat variant) ==="
"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-liquid.ipa" -s --liquid-glass
mkdir -p "$T/check5" && ( cd "$T/check5" && unzip -q "$T/Out-liquid.ipa" )
APP5="$T/check5/Payload/RealApp.app"
# Enable path: UIDesignRequiresCompatibility=false and the main executable's
# LC_BUILD_VERSION.sdk bumped to 26.0 (0x1A0000).
# plutil -p prints booleans as 0/1 (false/true). Capture output first (see
# the pipefail/SIGPIPE note above).
info5=$(plutil -p "$APP5/Info.plist")
grep -q 'UIDesignRequiresCompatibility.*=> 0' <<<"$info5" \
    || { echo "FAIL: UIDesignRequiresCompatibility not false"; exit 1; }
otool5=$(otool -l "$APP5/RealApp")
grep -q 'sdk 26.0' <<<"$otool5" \
    || { echo "FAIL: main executable sdk not 26.0"; exit 1; }
cp "$APP5/RealApp" "$T/liquid-main.bin"
codesign --verify --verbose=2 "$T/liquid-main.bin" 2>&1; echo "liquid main codesign exit=$?"

"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-liquid-compat.ipa" -s --liquid-glass-compat
mkdir -p "$T/check6" && ( cd "$T/check6" && unzip -q "$T/Out-liquid-compat.ipa" )
APP6="$T/check6/Payload/RealApp.app"
# Disable path: the key flips to true and no Mach-O change happens.
info6=$(plutil -p "$APP6/Info.plist")
grep -q 'UIDesignRequiresCompatibility.*=> 1' <<<"$info6" \
    || { echo "FAIL: compat UIDesignRequiresCompatibility not true"; exit 1; }
cp "$APP6/RealApp" "$T/liquid-compat-main.bin"
codesign --verify --verbose=2 "$T/liquid-compat-main.bin" 2>&1; echo "liquid-compat main codesign exit=$?"

echo
echo "=== [10/10] xkvm --force-fullscreen (plist-only patch template) ==="
# The plist-only patch template: one Info.plist key, no Mach-O surgery. This
# stage proves the full loop on a real IPA — flag → pipeline slot (after plist
# ops, before fakesign) → output plist carries the key, unrelated keys survive,
# and the app still validates.
"$XKVM" -i "$T/RealApp.ipa" -o "$T/Out-fullscreen.ipa" -s --force-fullscreen
mkdir -p "$T/check7" && ( cd "$T/check7" && unzip -q "$T/Out-fullscreen.ipa" )
APP7="$T/check7/Payload/RealApp.app"
info7=$(plutil -p "$APP7/Info.plist")
grep -q 'UIRequiresFullScreen.*=> 1' <<<"$info7" \
    || { echo "FAIL: UIRequiresFullScreen not true"; exit 1; }
# Unrelated keys must survive the binary-plist round-trip (plist.Dict is a map).
grep -q 'CFBundleIdentifier.*=> "com.example.RealApp"' <<<"$info7" \
    || { echo "FAIL: unrelated plist key dropped by the patch"; exit 1; }
cp "$APP7/RealApp" "$T/fullscreen-main.bin"
codesign --verify --verbose=2 "$T/fullscreen-main.bin" 2>&1; echo "fullscreen main codesign exit=$?"

echo
echo "== ALL REAL-ASSET CHECKS PASSED =="
