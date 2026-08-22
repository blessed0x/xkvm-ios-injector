# Product Requirements Document (PRD)
## xKVM — iOS Tweak Injector & App Modifier

**Version:** 1.0  
**Date:** 2026-08-16  
**Status:** MVP Complete (M0–M5), Production Ready  
**Repository:** `github.com/xscope0/xkvm-ios-injector`

---

## 1. Executive Summary

xkvm is a pure-Go command-line tool that replaces the fragmented Python-based
iOS tweak injection ecosystem (cyan + Azule) with a single, fast, cross-platform
binary. It injects tweaks into iOS apps, extracts them back out, converts
jailbreak packages between formats, fetches tweaks from Cydia repos, and
decrypts App Store downloads.

**Target users:** iOS developers, jailbreak community members, and AI coding
agents automating iOS app modification workflows.

---

## 2. Problem Statement

The iOS tweak injection ecosystem relies on:

1. **cyan** — a ~1,260-line Python tool that shells out to prebuilt binaries
   (`insert_dylib`, `install_name_tool`, `ldid`, `lipo`, `otool`) for all
   Mach-O manipulation. The author explicitly requested a rewrite in a compiled
   language.

2. **Azule** — an archived tool with features cyan lacks (repo-based tweak
   fetching, App Store decryption).

3. **Multiple fragmented tools** — each with platform-specific binary bundles,
   no unified interface, no dependency resolution, no extraction round-trip.

**Pain points:**
- Python runtime dependency + prebuilt binary bundles per platform
- No dependency resolution when fetching tweaks from repos
- No extraction/re-injection round-trip (extracted tweaks lose placement info)
- No shareable configuration format
- No compatibility checking before installation
- No TUI for non-technical users

---

## 3. Product Goals

| Goal | Success Metric |
|---|---|
| **Zero external dependencies** | Single static binary, no ldid/insert_dylib/otool needed |
| **Cross-platform** | Builds and tests on macOS (arm64), Linux (x64), Windows (x64) |
| **Feature parity with cyan** | All cyan flags work identically |
| **Feature parity with Azule** | Repo fetching + App Store decrypt |
| **Extraction round-trip** | `xkvm extract` → `xkvm inject` preserves placement automatically |
| **Dependency resolution** | Recursive dependency closure with caching |
| **User-friendly TUI** | `xkvm tui` asks questions in plain English |
| **Shareable configs** | `.cyan` files capture full injection state |
| **Proactive checking** | `xkvm check` finds missing deps before crash |

---

## 4. User Stories

### 4.1 Core Injection
- **As a** developer, **I want to** inject a dylib tweak into an IPA **so that**
  I can test modifications without a jailbroken device.
- **As a** developer, **I want to** inject multiple tweaks at once (dylib, deb,
  framework, bundle) **so that** I can compose complex modifications.

### 4.2 Extraction
- **As a** developer, **I want to** extract tweaks from a modified app **so that**
  I can reverse-engineer modifications or migrate them to a new version.

### 4.3 Package Conversion
- **As a** jailbreak user, **I want to** convert a rootful tweak to rootless
  format **so that** I can use it on a modern jailbreak (Dopamine, XinaA15).
- **As a** jailbreak user, **I want to** convert between deb and dylib formats
  **so that** I can adapt tweaks to different injection methods.

### 4.4 Repo Fetching
- **As a** user, **I want to** fetch a tweak by its bundle ID **so that** I don't
  have to manually find and download debs from Cydia repos.
- **As a** user, **I want** dependency resolution **so that** all required
  frameworks (Substrate, ElleKit, Cephei) are installed automatically.

### 4.5 Configuration
- **As a** power user, **I want to** save injection configurations as `.cyan`
  files **so that** I can repeat complex setups without retyping flags.
- **As a** power user, **I want to** share `.cyan` files **so that** others can
  reproduce my exact injection setup.

### 4.6 Checking & Repair
- **As a** user, **I want to** check an app for missing dependencies **so that**
  I can fix issues before installing on a device.
- **As a** user, **I want** auto-repair **so that** `xkvm check --fix` resolves
  issues automatically.

### 4.7 TUI
- **As a** non-technical user, **I want** an interactive menu **so that** I can
  inject tweaks without memorizing flag syntax.

### 4.8 Device Control
- **As a** developer, **I want to** install/unlaunch apps on a connected device
  **so that** I can test modified apps directly.

### 4.9 App Store Decrypt
- **As a** user, **I want to** decrypt an App Store app **so that** I can modify
  apps that would otherwise be encrypted and uneditable.

---

## 5. Feature Specifications

### 5.1 Tweak Injection (`xkvm inject`)

| Aspect | Specification |
|---|---|
| **Input formats** | `.ipa`, `.tipa`, `.app` |
| **Tweak formats** | `.dylib`, `.deb`, `.framework`, `.appex`, `.bundle`, directory |
| **Load commands** | `@rpath` (default), `@executable_path` (root-dylibs) |
| **Placement** | dylib/framework → `Frameworks/`, appex → `PlugIns/`, other → root |
| **Hooking runtime** | CydiaSubstrate (default) or ElleKit (`--ellekit`) |
| **Auto-inject** | Missing hooking frameworks injected from embedded extras |
| **Re-signing** | Ad-hoc signing via `pkg/codesign` (no ldid) |
| **Entitlements** | Preserved across binary edits; `--entitlements` to add/modify |
| **Root-dylibs** | `--root-dylib` places dylib in app root with `@executable_path` |
| **Extraction manifest** | Auto-restore placement from `xkvm-manifest.json` |

### 5.2 Tweak Extraction (`xkvm extract`)

| Aspect | Specification |
|---|---|
| **Output** | Directory with extracted artifacts + `xkvm-manifest.json` |
| **Manifest records** | Original placement (root/frameworks/plugins/other) + kind |
| **Round-trip** | Re-injection reads manifest, auto-restores placement |
| **Scope** | All injected dylibs, frameworks, appex, bundles |

### 5.3 Package Conversion

| Conversion | Flag | Notes |
|---|---|---|
| rootful → rootless | `xkvm rootless` | Repack under `/var/jb`, control update, Mach-O rewrite, cstring rewrite, script conversion |
| rootless → rootful | `xkvm rootful` | Inverse conversion |
| rootless → roothide | `xkvm roothide` | Hoist payload, `@loader_path/.jbroot/` rewrite, pkgmirror |
| deb → dylib | `xkvm undeb` | Extract deb payload artifacts |
| dylib → deb | `xkvm debify` | Build MobileSubstrate .deb from dylib |

### 5.4 Repo Fetching (`--fetch`)

| Aspect | Specification |
|---|---|
| **Backends** | Canister API + MobileAPT (Packages.gz/zst/xz/bz2) |
| **Resolution** | Bundle ID → repo → download URL → .deb |
| **Dependencies** | Recursive closure from `Depends:` field |
| **Caching** | Persistent cache (`~/Library/Caches/xkvm/fetch`), 7-day TTL, sha256 validation |
| **Custom repos** | `--apt-source` for additional APT sources |
| **Fallback** | Built-in default repo sweep when explicit repos miss |

### 5.5 Compatibility Checking (`xkvm check`)

| Aspect | Specification |
|---|---|
| **Scope** | All bundle-relative load command dependencies |
| **Output** | Missing file list with source references |
| **Auto-fix** | `--fix` resolves from: fix-dir, deb, cache, online repos |
| **Safe** | Never mutates input; writes fixed copy to `-o` |

### 5.6 Configuration (`.cyan`)

| Aspect | Specification |
|---|---|
| **Format** | ZIP archive with `config.json` + `inject/` payloads |
| **Generation** | `xkvm cgen -o out.cyan -f tweak ...` |
| **Validation** | `xkvm cyan-check file.cyan` |
| **Application** | `xkvm -z file.cyan -i app.ipa` |

### 5.7 Terminal UI (`xkvm tui`)

| Aspect | Specification |
|---|---|
| **Engine** | Drives same `internal/app` entry points as CLI |
| **Questions** | Plain English, one at a time |
| **Default** | Bare `xkvm` (no flags) opens TUI |

### 5.8 Device Control (`xkvm device`)

| Operation | Description |
|---|---|
| `list` | List connected iOS devices |
| `pair` | Pair a device |
| `info` | Device info (name, model, version) |
| `battery` | Battery level |
| `apps` | List installed apps |
| `install` | Install IPA on device |
| `uninstall` | Uninstall app |
| `launch` | Launch app |
| `kill` | Kill app |
| `syslog` | Stream device logs |
| `restart` | Restart device |
| `shutdown` | Shutdown device |

### 5.9 App Store Decrypt (`xkvm decrypt`)

| Aspect | Specification |
|---|---|
| **Auth** | Apple ID + password via `ipatool` |
| **Flow** | iTunes lookup → buy → sinfs + metadata download |
| **Platform** | iOS only (requires `fouldecrypt` or equivalent) |

---

## 6. Non-Functional Requirements

| Requirement | Target |
|---|---|
| **Performance** | Inject 10 tweaks into 50MB IPA in < 5 seconds |
| **Binary size** | < 15MB static binary (darwin/arm64) |
| **Test coverage** | > 40% line coverage, 58 test files |
| **CI** | GitHub Actions: lint + test matrix (macOS/Linux/Windows) |
| **Release** | GoReleaser on git tags |
| **License** | MIT |

---

## 7. Constraints

- **No external tools** for Mach-O manipulation (pure Go via go-macho + pkg/codesign)
- **No Python runtime** required
- **No platform-specific binary bundles** (unlike cyan's per-platform tool dirs)
- **Backward compatible** with cyan flag names and semantics
- **Byte-golden tests** for converter outputs (rootful/rootless/roothide)

---

## 8. Milestones

| Milestone | Status | Description |
|---|---|---|
| M0 | ✅ | Core injection engine (dylib, framework, appex) |
| M1 | ✅ | Extraction + manifest round-trip |
| M2 | ✅ | Package conversion (rootful/rootless/roothide) |
| M3 | ✅ | Pure-Go Mach-O (go-macho + pkg/codesign) |
| M4 | ✅ | Repo fetching (Canister + MobileAPT) |
| M5 | ✅ | App Store decrypt (ipatool flow) |
| M6 | ✅ | TUI + device control |
| M7 | ✅ | Compatibility patches (Liquid Glass, force fullscreen) |
| M8 | 🔲 | On-device App Store decrypt (fouldecrypt) |
| M9 | 🔲 | Plugin system for custom patches |

---

## 9. Success Criteria

1. **Feature parity**: All cyan flags work identically in xkvm
2. **Zero dependencies**: Single binary, no external tools needed
3. **Cross-platform**: Builds and passes CI on macOS, Linux, Windows
4. **Extraction round-trip**: `extract` → `inject` preserves placement
5. **Dependency resolution**: Recursive closure with caching
6. **User-friendly**: TUI works for non-technical users
7. **Extensible**: New patches, converters, and backends can be added without
   modifying core injection logic
