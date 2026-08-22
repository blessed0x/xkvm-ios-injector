# Technical Design Document (TTD)
## xKVM — Architecture & Implementation Details

**Version:** 1.0  
**Date:** 2026-08-16  
**Status:** MVP Complete  
**Repository:** `github.com/xscope0/xkvm-ios-injector`

---

## 1. System Architecture

### 1.1 High-Level Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                        CLI Layer                             │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │  Root     │  │   TUI    │  │  Device  │  │  Decrypt │   │
│  │  Command  │  │  Menu    │  │  Control │  │  Client  │   │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘   │
│       │              │              │              │         │
│       └──────────────┴──────────────┴──────────────┘         │
│                          │                                   │
│                    ┌─────┴─────┐                             │
│                    │   Options  │                             │
│                    │  Validate  │                             │
│                    └─────┬─────┘                             │
└──────────────────────────┼──────────────────────────────────┘
                           │
┌──────────────────────────┼──────────────────────────────────┐
│                    Pipeline Layer                             │
│                    ┌─────┴─────┐                             │
│                    │  app.Run  │                             │
│                    └─────┬─────┘                             │
│                          │                                   │
│  ┌─────────┬─────────┬──┴──┬─────────┬─────────┬─────────┐ │
│  │  Inject │  Fetch  │Check│ Patches │  Plist  │  Sign   │ │
│  │ Engine  │ Solver  │     │ Engine  │  Merge  │ Engine  │ │
│  └────┬────┘────┬────┘─────┘────┬────┘────┬────┘────┬────┘ │
└───────┼─────────┼───────────────┼─────────┼─────────┼──────┘
        │         │               │         │         │
┌───────┼─────────┼───────────────┼─────────┼─────────┼──────┐
│                    Mach-O Layer                              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              internal/macho                          │   │
│  │  ┌─────────┐ ┌──────────┐ ┌──────────┐ ┌────────┐ │   │
│  │  │ native  │ │ cstring  │ │  der.go  │ │  bin   │ │   │
│  │  │ (go-    │ │ (PATCH_  │ │ (code    │ │ (deps, │ │   │
│  │  │ macho)  │ │ ROOTLESS)│ │  sign)   │ │ rpaths)│ │   │
│  │  └─────────┘ └──────────┘ └──────────┘ └────────┘ │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
        │
┌───────┼─────────────────────────────────────────────────────┐
│                    Storage Layer                             │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐      │
│  │   IPA    │ │   Deb    │ │  Cyanfile│ │  Fetch   │      │
│  │  Pack/   │ │ Parse/   │ │  Parse/  │ │  Cache   │      │
│  │ Unpack   │ │ Build    │ │  Gen     │ │  (7d TTL)│      │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘      │
└────────────────────────────────────────────────────────────┘
```

### 1.2 Package Dependency Graph

```
cmd/xkvm
  └── internal/cli          (cobra CLI, flag parsing)
        ├── internal/app    (pipeline orchestration)
        │     ├── internal/inject    (tweak injection engine)
        │     ├── internal/fetch     (repo fetching + dependency resolution)
        │     ├── internal/appbundle (app bundle manipulation)
        │     ├── internal/patch     (compatibility patches)
        │     ├── internal/extras    (embedded hooking frameworks + sideload fixes)
        │     ├── internal/cyanfile  (.cyan config parsing)
        │     ├── internal/ipa       (IPA pack/unpack)
        │     ├── internal/plist     (plist manipulation)
        │     └── internal/artifact  (extraction manifests)
        ├── internal/macho  (pure-Go Mach-O manipulation)
        ├── internal/rootless (jailbreak package conversion)
        ├── internal/deb    (deb parse/build)
        ├── internal/device (go-ios device control)
        ├── internal/decrypt (App Store decryption)
        ├── internal/tui    (terminal UI)
        └── internal/log    (structured logging)
```

---

## 2. Core Engine: `internal/app`

### 2.1 Pipeline Flow

The `app.Run()` function orchestrates the entire injection pipeline in a
strict ordering that mirrors cyan's `logic.main()`:

```
1. Parse .cyan configs → materialize payloads into tmpdir
2. Validate options (input exists, output not same as input)
3. Prepare app (unzip IPA/tipa or copy .app to tmpdir)
4. Open app bundle (find main binary, parse Info.plist)
5. Check encryption (fail unless --ignore-encrypted)
6. Remove extensions (if --remove-extensions or --remove-encrypted)
7. Fetch tweaks (if --fetch: Canister/MobileAPT → .debs)
8. Inject sideload fixes (if --patch: bundle fix dylibs)
9. Inject tweaks (if -f files: expand debs, inject per-type)
10. Check references (warn on missing bundle-relative deps)
11. Modify metadata (name, version, bundle-id, minOS, icon, plist)
12. Apply compatibility patches (Liquid Glass, force fullscreen)
13. Fakesign all binaries (if --fakesign)
14. Thin all binaries (if --thin)
15. Repack output (IPA with compression, or copy .app)
```

### 2.2 Options Validation

`Options.validate()` mirrors cyan's `tbhutils.validate_inputs()`:
- Decode URL-encoded paths (spaces as `%20`)
- Verify input suffix is `.app`, `.ipa`, or `.tipa`
- Verify input file exists
- Verify output path is writable (or --overwrite)

---

## 3. Injection Engine: `internal/inject`

### 3.1 Injection Modes

Two mutually exclusive hooking runtime modes (feather-ellekit-spec.md D1/D5/D6):

| Mode | Flag | Framework | Load Command |
|---|---|---|---|
| **ModeSubstrate** | default | `CydiaSubstrate.framework` | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` |
| **ModeElleKit** | `--ellekit` | `ElleKit.framework` | `@rpath/ElleKit.framework/ElleKit` |

**Auto-switch:** If any tweak depends on `libhooker`, the entire run
automatically switches to ElleKit mode (D6). This is decided by a PRE-SCAN
before any dependency rewriting to prevent inconsistent rewrites.

### 3.2 Injection Flow

```
1. Expand .deb files → extract artifacts (dylib, framework, appex, bundle)
2. Pre-scan for libhooker → auto-switch to ElleKit if needed
3. Extract main binary entitlements
4. Remove main binary signature
5. Create PlugIns/ if appex tweaks present
6. Create Frameworks/ if dylib/framework tweaks present
7. Add @executable_path/Frameworks rpath (if frameworks present)
8. For each tweak:
   - .appex → PlugIns/
   - .dylib (root) → app root with @executable_path/{name}
   - .dylib (normal) → Frameworks/ with @rpath/{name}
   - .framework → Frameworks/
   - other → app root (copy)
9. Auto-inject missing hooking frameworks from extras/
10. Restore main binary entitlements
```

### 3.3 Dependency Rewriting

The `commonDeps` map defines canonical forms for legacy hooking dependencies:

```go
commonDeps = map[string]depInfo{
    "substrate.":      {sub: "@rpath/CydiaSubstrate.framework/CydiaSubstrate",
                        ek:  "@rpath/ElleKit.framework/ElleKit"},
    "mobilesubstrate": {sub: "@rpath/CydiaSubstrate.framework/CydiaSubstrate",
                        ek:  "@rpath/ElleKit.framework/ElleKit"},
    "libhooker":       {sub: "", ek: "@rpath/ElleKit.framework/ElleKit"},
    "orion.":          {sub: "@rpath/Orion.framework/Orion",
                        ek:  "@rpath/Orion.framework/Orion"},
    "cephei.":         {sub: "@rpath/Cephei.framework/Cephei",
                        ek:  "@rpath/Cephei.framework/Cephei"},
    // ... CepheiPrefs, CepheiUI
}
```

Keys are sorted longest-first to prevent partial matches (e.g. "cepheiui."
matches before "cephei.").

### 3.4 Root-Dylibs

Some dlopen-based tweaks (e.g. Regram) resolve resources relative to
`@executable_path` and crash when relocated to `Frameworks/`. The
`--root-dylib` flag places the dylib in the app root with an
`@executable_path/{name}` load command instead of `@rpath`.

**Auto-restore:** When `-f` files were produced by `xkvm extract`, the
extraction manifest (`xkvm-manifest.json`) records each dylib's original
placement. Root-placed dylibs are automatically re-rooted without any flag.

---

## 4. Mach-O Layer: `internal/macho`

### 4.1 Native Binary Manipulation

The `macho` package replaces all external tools (ldid, insert_dylib,
install_name_tool, otool) with pure Go via `blacktop/go-macho`:

| External Tool | Replacement | File |
|---|---|---|
| `otool -L` | `macho.Bin.Deps()` | `bin.go` |
| `install_name_tool -change` | `macho.Bin.RewriteDeps()` | `native.go` |
| `install_name_tool -add_rpath` | `macho.Bin.AddRpath()` | `native.go` |
| `ldid -e` (extract entitlements) | `macho.Bin.ExtractEntitlements()` | `native.go` |
| `ldid -S -Cadhoc` (sign) | `macho.Bin.SignWithEntitlements()` | `native.go` |
| `ldid -R` (remove sig) | `macho.Bin.RemoveSignature()` | `native.go` |

### 4.2 Growing-Rename Pattern

When a load command dependency changes to a longer string, the TOC
(Table of Contents) is re-serialized and all `__LINKEDIT`-referencing load
commands are re-offset. This is the core of `editMachO()`:

```go
func editMachO(path string, fn func(f *macho.File, orig []byte) ([]byte, error)) error {
    // 1. Read file
    // 2. Parse as fat or thin Mach-O
    // 3. Apply fn to each architecture slice
    // 4. Rebuild fat or thin output
    // 5. Atomic write (temp + rename)
}
```

### 4.3 __cstring Rewrite

For rootless conversion, dlopen strings in `__TEXT.__cstring` are rewritten
in-place when they fit, or packed into a new `__PATCH_ROOTLESS,__cstring`
segment inserted before `__LINKEDIT` with ADRP/ADD/ADR instruction
retargeting. This is handled in `cstring.go`.

### 4.4 Code Signing

Ad-hoc signing is done via `go-macho/pkg/codesign`:
- Parse existing signature
- Extract entitlements
- Build new SuperBlob with CodeDirectory + entitlements
- Write atomically

---

## 5. Jailbreak Package Conversion: `internal/rootless`

### 5.1 Conversion Matrix

```
rootful ──→ rootless (rootless-patcher semantics)
rootless ──→ rootful (inverse)
rootless ──→ roothide (RootHidePatcher semantics)
```

### 5.2 rootful → rootless Pipeline

1. **Repack** — payload moved under `/var/jb/` (DEBIAN/ stays at top)
2. **Control** — Architecture → `iphoneos-arm64`, Depends gains runtime alternation
3. **Mach-O** — load-command paths rewritten under `/var/jb`, honoring blacklist
4. **__cstring** — runtime dlopen strings rewritten, growing strings relocated
5. **Scripts** — DEBIAN control scripts and shebang payload files converted
6. **--tweakinject** — Derootifier conventions: DynamicLibraries → usr/lib/TweakInject
7. **Post-conversion audit** — warns on surviving rootful paths

### 5.3 Blacklist

Paths that must never be rewritten:
- `/System/*` — Apple system libraries
- `/usr/lib/libSystem`, `libobjc`, `libc++`, etc. — Apple runtime libraries
- `/var/jb` — the jailbreak root itself

### 5.4 rootless → roothide Pipeline

1. **Hoist** — `/var/jb` payload moved to package root
2. **rootfs** — remaining files under `rootfs/`
3. **Mach-O** — `/var/jb` load commands rewritten to `@loader_path/.jbroot/...`
4. **Control** — Architecture → `iphoneos-arm64e`
5. **Scripts/plists** — path translations
6. **pkgmirror** — optional package mirror support

---

## 6. Repo Fetching: `internal/fetch`

### 6.1 Resolution Flow

```
1. For each requested package ID:
   a. Try --apt-source repos first
   b. Try Canister API (GET /package/{id})
   c. Fall back to default repo sweep
2. Download .deb to cache dir
3. Parse Depends: from index stanza
4. Recurse for each dependency (visited-set cycle guard)
5. Return ordered list of .deb paths
```

### 6.2 Smart Cache

- **Location:** `~/Library/Caches/xkvm/fetch/` (or `$XDG_CACHE_HOME/xkvm/fetch/`)
- **Key:** `<package-id>__<version>.deb`
- **Validation:** sha256 from index must match downloaded file
- **TTL:** 7-day mtime-based pruning (lazy, on Resolve entry)
- **Scope:** Only prunes when using default cache dir (not user-supplied)

### 6.3 Dependency Resolution

- Parse `Depends:` from index stanza
- Strip version constraints
- Split `|` alternative groups
- Filter core deps (Substrate, ElleKit, coreutils, firmware, etc.)
- Recurse with visited-set cycle guard
- Per-repo index caching (one HTTP fetch per repo per run)

---

## 7. Storage Layer

### 7.1 IPA Handling (`internal/ipa`)

- **Unpack:** `archive/zip` → extract to tmpdir
- **Repack:** `archive/zip` with configurable compression (0-9)
- **Tipa support:** `.tipa` treated as `.ipa` (TrollStore format)

### 7.2 Deb Handling (`internal/deb`)

- **Parse:** `ar` archive → `data.tar.*` → extract
- **Build:** `control` + `data.tar.*` → `ar` archive
- **Compression:** gzip, zstd, xz, bz2 (auto-detect on extract)

### 7.3 Cyanfile Handling (`internal/cyanfile`)

- **Format:** ZIP with `config.json` + `inject/` payloads
- **Parse:** Extract config.json, materialize payloads to tmpdir
- **Generate:** `xkvm cgen -o out.cyan -f tweak ...`

---

## 8. Device Control: `internal/device`

### 8.1 Architecture

Uses `danielpaulus/go-ios` for USB communication:
- **Pairing:** Lockdown protocol
- **Install/Uninstall:** AFC2 + installation service
- **Syslog:** syslogd streaming
- **Battery/Apps:** Lockdown queries

### 8.2 Exit Codes

Device-control errors carry specific exit codes (64-70):
- 64: device not found
- 65: device not paired
- 66: installation failed
- 67: uninstallation failed
- 68: launch failed
- 69: kill failed
- 70: syslog failed

---

## 9. Testing Strategy

### 9.1 Test Categories

| Category | Count | Description |
|---|---|---|
| Unit tests | ~40 | Package-level function tests |
| Integration tests | ~15 | Pipeline-level tests with fixture IPsAs |
| Golden tests | ~10 | Byte-exact comparison against upstream output |
| E2E tests | ~5 | Full CLI invocation tests |
| Fuzz tests | ~3 | Deb parsing, cstring parsing |

### 9.2 Test Commands

```bash
go test -race ./...                    # All tests with race detector
make qa                                # Full gate: lint + test + cross-compile
go test -run TestInject -v             # Specific test
go test -fuzz FuzzDebExtract -fuzztime 30s  # Fuzz testing
```

### 9.3 Golden Fixtures

Converter outputs are pinned to upstream (rootless-patcher, RootHidePatcher)
byte-exact output. Deviations must be documented in `ARCHITECTURE.md`.

---

## 10. Build & Release

### 10.1 Build

```bash
go build -o xkvm ./cmd/xkvm           # Local build
make qa                                # Full gate
```

### 10.2 Cross-Compilation Matrix

```
darwin  × arm64, amd64
linux   × arm64, amd64
windows × arm64, amd64
```

### 10.3 Release

GoReleaser on git tags:
```bash
git tag v0.1.0
git push origin v0.1.0
# GoReleaser builds + publishes GitHub release
```

### 10.4 Version Injection

```bash
go build -ldflags "-X github.com/xscope0/xkvm-ios-injector/internal/app.Version=v0.1.0"
```

---

## 11. Dependencies

| Dependency | Version | Purpose |
|---|---|---|
| `blacktop/go-macho` | v1.1.282 | Mach-O parse/modify/rebuild |
| `danielpaulus/go-ios` | v1.3.2 | iOS device communication |
| `spf13/cobra` | v1.10.2 | CLI framework |
| `howett.net/plist` | v1.0.1 | Plist parsing |
| `klauspost/compress` | v1.19.2 | Zstd compression |
| `ulikunitz/xz` | v0.5.16 | XZ compression |
| `dsnet/compress` | v0.0.1 | Bzip2 compression |
| `golang.org/x/image` | v0.45.0 | Image processing (icons) |

### 11.1 Tooling (go.mod `tool` directive)

| Tool | Purpose |
|---|---|
| `golang.org/x/vuln/cmd/govulncheck` | Dependency CVE scanning |
| `honnef.co/go/tools/cmd/staticcheck` | Static analysis |

---

## 12. Security Considerations

- **No external tool execution** — all Mach-O manipulation is pure Go
- **Ad-hoc signing only** — no certificate handling
- **URL-encoded path decoding** — safe, best-effort
- **Cache validation** — sha256 verification on cached downloads
- **Blacklist protection** — system paths never rewritten in conversion
- **Entitlement preservation** — main binary entitlements preserved across edits

---

## 13. Future Work

| Area | Description |
|---|---|
| **M8: On-device decrypt** | fouldecrypt/tfp0/kernrw integration |
| **M9: Plugin system** | Custom patches via Go plugin or script |
| **Batch injection** | Multiple apps in one invocation |
| **Watch app handling** | More granular control over watch extensions |
| **Localization** | Multi-language CLI output |
