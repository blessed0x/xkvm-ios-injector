# Building ElleKit.framework for xkvm

How to produce the vendored fat **arm64 + arm64e** `ElleKit.framework` that the `--ellekit`
injection mode embeds and thins at inject time.

**Feature spec:** [feather-ellekit-spec.md](../feather-ellekit-spec.md) (decisions D7–D9).
**Upstream:** [evelyneee/ElleKit](https://github.com/evelyneee/ElleKit) — **BSD-3-Clause**.
**Verified on:** macOS, Xcode 15.0.1 (15A507), iPhoneOS 17.0 SDK, Homebrew `ldid` — 2026-08-13.

---

## 1. Toolchain requirements

| Tool | Why | How to check |
|---|---|---|
| Xcode **14+** (CI upstream uses 15.1) | `xcodebuild` compiles the project (C + Swift + arm64 asm) | `xcodebuild -version` |
| iPhoneOS SDK | target platform | `xcodebuild -showsdks \| grep iphoneos` |
| `ldid` | ad-hoc (pseudo) signing of the framework binary | `brew install ldid` |
| `install_name_tool` | rewrites the dylib's LC_ID_DYLIB to the `@rpath` framework path | bundled with Xcode |

**No Theos, no procursus, no extra SDKs.** The upstream CI bootstraps procursus only for
packaging utilities (`ldid sed coreutils findutils`); the library itself is a plain
`xcodebuild` build. A Homebrew `ldid` covers the only external need.

---

## 2. Build the library (fat arm64 + arm64e)

```bash
# 1. Clone at a pinned tag for reproducibility
git clone --depth 1 --branch v1.1.3 https://github.com/evelyneee/ElleKit
cd ElleKit

# 2. Build ONLY the `ellekit` scheme, Release, for the device SDK
xcodebuild -scheme ellekit -configuration Release \
  -sdk iphoneos \
  ARCHS="arm64 arm64e" \
  ONLY_ACTIVE_ARCH=NO \
  BUILD_DIR="build/" \
  CODE_SIGNING_ALLOWED=NO \
  CODE_SIGNING_REQUIRED=NO \
  CODE_SIGN_IDENTITY= \
  2>&1 | tail -20          # expect: ** BUILD SUCCEEDED **

# 3. Result
file build/Release-iphoneos/libellekit.dylib
# → Mach-O universal binary with 2 architectures: [arm64] [arm64e]
```

### Why these exact flags

- **`-configuration Release` is required for the fat build.** The Xcode project declares
  `ARCHS = (arm64e, arm64)` only in its **Release** configurations; **Debug is arm64-only**
  (`project.pbxproj`). So `make` / `make build-ios` with the default Debug configuration
  yields a thin arm64 library even though the source supports arm64e.
- **`ARCHS="arm64 arm64e" ONLY_ACTIVE_ARCH=NO`** makes the fat slice explicit and
  deterministic regardless of the project's per-config defaults.
- **`-sdk iphoneos` instead of `-destination 'generic/platform=iOS'`.** On this machine the
  Makefile's destination form fails with *"Unable to find a destination … error:iOS 17.0 is
  not installed"* even though the SDK files exist and `xcodebuild -showsdks` lists them.
  Passing `-sdk iphoneos` skips destination matching entirely and builds the device SDK
  directly. (Upstream CI — macos-13 + Xcode 15.1 — has no such issue and runs plain `make`.)
- **`CODE_SIGNING_ALLOWED=NO …`** mirrors the Makefile's `COMMON_OPTIONS`; the final artifact
  is pseudo-signed with `ldid` in step 4.

### The Makefile (for reference)

| Target | Produces | Notes |
|---|---|---|
| `make build-ios` | `build/{Debug\|Release}-iphoneos/libellekit.dylib` + injector/launchd/loader/safemode-ui | `RELEASE=1` → Release; default Debug (thin arm64) |
| `make deb-ios-rootless` / `deb-ios-rootful` | `.deb` with `libellekit.dylib` + `libsubstrate.dylib`/`libhooker.dylib`/`CydiaSubstrate.framework` **symlinks** | needs `dpkg-deb`; uses `-destination` (may hit the gotcha above) |
| `MAC=1 make deb` | macOS tar.gz | macOS target, out of scope here |

There is **no `make framework` target** — the framework bundle is assembled by hand (step 4).

---

## 3. Assemble `ElleKit.framework`

The upstream `libellekit.dylib` has LC_ID_DYLIB `/usr/local/lib/libellekit.dylib`. For a
bundle-loadable framework it must be renamed and its install name rewritten to the `@rpath`
path xkvm injects, mirroring the existing embedded `CydiaSubstrate.framework` convention.

```bash
cd ElleKit
VER=$(git describe --tags --abbrev=0 | sed 's/^v//')   # e.g. 1.1.3

rm -rf build/ElleKit.framework
mkdir -p build/ElleKit.framework
cp build/Release-iphoneos/libellekit.dylib build/ElleKit.framework/ElleKit

# Info.plist (full template below)
install_name_tool -id @rpath/ElleKit.framework/ElleKit build/ElleKit.framework/ElleKit

# Ad-hoc pseudo-sign (both slices), matching how the existing extras frameworks ship
ldid -S build/ElleKit.framework/ElleKit
```

### `ElleKit.framework/Info.plist`

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key>
	<string>en</string>
	<key>CFBundleExecutable</key>
	<string>ElleKit</string>
	<key>CFBundleIdentifier</key>
	<string>ellekit</string>
	<key>CFBundleInfoDictionaryVersion</key>
	<string>6.0</string>
	<key>CFBundleName</key>
	<string>ElleKit</string>
	<key>CFBundleShortVersionString</key>
	<string>${VER}</string>
	<key>CFBundleVersion</key>
	<string>${VER}</string>
	<key>UIRequiredDeviceCapabilities</key>
	<array>
		<string>arm64</string>
	</array>
</dict>
</plist>
```

> This mirrors the Info.plist already embedded in xkvm's `CydiaSubstrate.framework` — which is
> itself ElleKit 1.1.3 renamed (its `codesign` Identifier is `libellekit.dylib`). The two
> frameworks must stay **separate assets**: default mode serves the substrate-named copy,
> `--ellekit` mode serves this one (spec D1).

---

## 4. Verify the artifact

```bash
file    build/ElleKit.framework/ElleKit        # universal: arm64 + arm64e
lipo -info build/ElleKit.framework/ElleKit     # arm64 arm64e
otool -D build/ElleKit.framework/ElleKit       # @rpath/ElleKit.framework/ElleKit
otool -L build/ElleKit.framework/ElleKit       # only system deps (Foundation, libobjc, libSystem, CoreFoundation)
codesign -dv build/ElleKit.framework/ElleKit   # Identifier=ElleKit, universal
du -sh  build/ElleKit.framework                # ≈ 800 KB
```

Verified outputs on 2026-08-13:

```
file:    Mach-O universal binary with 2 architectures: [arm64] [arm64e]
otool -D:@rpath/ElleKit.framework/ElleKit
codesign:Identifier=ElleKit, Format=bundle with Mach-O universal (arm64e arm64)
size:    808K  build/ElleKit.framework
```

**Thin-on-inject safety** (spec D8/D9 — xkvm slices to the app's arch at inject time):
`lipo -thin arm64` and `lipo -thin arm64e` both produce clean single-slice dylibs; the
`@rpath/ElleKit.framework/ElleKit` install name survives thinning, and the thin slice
re-signs fine with `ldid -S` (re-verified 2026-08-13). xkvm's own fakesign pass re-signs the
thinned slice anyway.

---

## 5. Vendor into xkvm

```bash
# After the feature lands: place the assembled framework into the go:embed tree
mkdir -p internal/extras/extras/ElleKit.framework
cp -R build/ElleKit.framework/ internal/extras/extras/ElleKit.framework/
```

- Keep the **fat** artifact vendored; thinning happens at injection time (`internal/macho`
  slice-selection helper, spec D8/D9).
- The build output under `upstream/ElleKit/build/` is git-ignored (`/upstream/` in
  `.gitignore`) — only the copied asset in `internal/extras/extras/` is tracked.
- Record provenance in `NOTICE` and in `internal/extras/extras.go` comments (spec D13).

---

## 6. Version pinning & license

- Pin to a release tag (`git clone --depth 1 --branch v1.1.3`). Latest tag verified: **v1.1.3**
  (2026-08-13) — same version as the embedded substrate-named copy.
- **Bump procedure:** checkout the new tag → repeat §2–§3 → update `Info.plist` version →
  update `NOTICE` + `docs/ellekit-build.md`.
- **License:** BSD-3-Clause. Permissive; redistribution requires retaining the copyright
  notice + disclaimer (include `ElleKit/LICENSE` in `NOTICE`). Feather (GPL-3.0) is
  reference-only and contributes no code here.

---

## 7. Gotchas recap

1. **Destination error** — `-destination 'generic/platform=iOS'` can fail with *"iOS 17.0 is
   not installed"* despite the SDK being present; use `-sdk iphoneos` (verified fix).
2. **Debug is thin arm64** — the fat `(arm64e, arm64)` ARCHS live in the **Release**
   configurations only. Always build `-configuration Release`.
3. **No `make framework`** — the framework bundle is hand-assembled (rename + Info.plist +
   `install_name_tool -id`). Don't confuse `libellekit.dylib` with a bundle-ready framework.
4. **Install name** — the upstream LC_ID_DYLIB is `/usr/local/lib/libellekit.dylib`;
   forgetting the `install_name_tool -id` step makes dyld fail to resolve
   `@rpath/ElleKit.framework/ElleKit` at load time.
5. **Do not strip** — keep the binary as-built; xkvm's fakesign pass handles signature
   regeneration.
