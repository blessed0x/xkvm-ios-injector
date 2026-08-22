# xKVM — Architecture Overview

**Purpose:** High-level architecture reference for AI agents and new contributors.  
**For detailed specs:** See `ARCHITECTURE.md` (66KB authoritative spec).  
**For quick onboarding:** See `docs/HANDOFF.md`.

---

## System Overview

xKVM is a pure-Go CLI tool that manipulates iOS app bundles. It parses Mach-O
binaries, injects tweak dylibs, re-signs everything, and repacks the container.
No external tools — everything runs through `blacktop/go-macho` and
`pkg/codesign`.

```
┌─────────────────────────────────────────────────────────────┐
│                        User Interface                        │
│  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌──────────┐   │
│  │  CLI     │  │   TUI    │  │  Device  │  │  Decrypt │   │
│  │  (cobra) │  │ (menu)   │  │ (go-ios) │  │ (ipatool)│   │
│  └────┬─────┘  └────┬─────┘  └────┬─────┘  └────┬─────┘   │
│       └──────────────┴──────────────┴──────────────┘         │
└──────────────────────────┬──────────────────────────────────┘
                           │
┌──────────────────────────┼──────────────────────────────────┐
│                    Pipeline Layer                             │
│                    ┌─────┴─────┐                             │
│                    │  app.Run  │  ← orchestrates everything  │
│                    └─────┬─────┘                             │
│       ┌──────────┬───────┼───────┬──────────┬──────────┐   │
│       │          │       │       │          │          │   │
│    ┌──┴──┐   ┌──┴──┐ ┌──┴──┐ ┌──┴──┐   ┌──┴──┐   ┌──┴──┐│
│    │Inject│   │Fetch│ │Check│ │Patch│   │Plist│   │Sign ││
│    └──┬──┘   └──┬──┘ └──┬──┘ └──┬──┘   └──┬──┘   └──┬──┘│
└───────┼─────────┼───────┼───────┼─────────┼─────────┼────┘
        │         │       │       │         │         │
┌───────┼─────────┼───────┼───────┼─────────┼─────────┼────┐
│                    Mach-O Layer                              │
│  ┌─────────────────────────────────────────────────────┐   │
│  │              internal/macho                          │   │
│  │  ┌─────────┐ ┌──────────┐ ┌──────────┐ ┌────────┐ │   │
│  │  │ native  │ │ cstring  │ │  der.go  │ │  bin   │ │   │
│  │  │ (edit   │ │ (PATCH_  │ │ (code    │ │ (deps, │ │   │
│  │  │ MachO)  │ │ ROOTLESS)│ │  sign)   │ │ rpaths)│ │   │
│  │  └─────────┘ └──────────┘ └──────────┘ └────────┘ │   │
│  └─────────────────────────────────────────────────────┘   │
└────────────────────────────────────────────────────────────┘
        │
┌───────┼────────────────────────────────────────────────────┐
│                    Storage Layer                             │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐     │
│  │   IPA    │ │   Deb    │ │  Cyanfile│ │  Fetch   │     │
│  │  zip     │ │  ar+tar  │ │  zip+json│ │  cache   │     │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘     │
└────────────────────────────────────────────────────────────┘
```

---

## Core Concepts

### 1. Injection Pipeline

The pipeline follows a strict ordering (mirrors cyan's `logic.main()`):

```
Parse .cyan → Validate → Prepare app → Open bundle → Check encryption
  → Remove extensions → Fetch tweaks → Inject sideload fixes
  → Inject tweaks → Check references → Modify metadata
  → Apply patches → Fakesign → Thin → Repack output
```

**Key invariant:** Every step that modifies the binary runs BEFORE signing.
Signing is always the last binary mutation.

### 2. Hooking Runtimes

Two mutually exclusive modes (feather-ellekit-spec.md D1/D5/D6):

| Mode | When | Framework |
|---|---|---|
| **Substrate** | Default | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` |
| **ElleKit** | `--ellekit` or libhooker tweak | `@rpath/ElleKit.framework/ElleKit` |

**Auto-switch:** PRE-SCAN decides mode before any rewriting. If a tweak needs
libhooker, the entire run switches to ElleKit mode.

### 3. Dependency Rewriting

Legacy hooking dependencies are rewritten to canonical forms:
- `substrate` → `CydiaSubstrate.framework` (or `ElleKit.framework`)
- `orion` → `Orion.framework`
- `cephei` → `Cephei.framework`
- etc.

Keys sorted longest-first to prevent partial matches.

### 4. Root-Dylibs

Some dlopen-based tweaks crash when relocated to `Frameworks/`. The
`--root-dylib` flag places them in the app root with `@executable_path/{name}`.

**Auto-restore:** Extraction manifest records original placement; re-injection
honors it without flags.

### 5. Growing-Rename

When a load command dependency changes to a longer string:
1. TOC is re-serialized
2. All __LINKEDIT-referencing load commands are re-offset
3. File is atomically rewritten (temp + rename)

### 6. __cstring Rewrite

For rootless conversion, dlopen strings are rewritten:
- In-place when they fit
- Otherwise packed into `__PATCH_ROOTLESS,__cstring` segment
- ADRP/ADD/ADR instructions retargeted

---

## Package Map

| Package | Lines | Responsibility |
|---|---|---|
| `cli` | ~34KB | CLI structure, flag definitions, subcommands |
| `app` | ~19KB | Pipeline orchestration, options, extract |
| `inject` | ~19KB | Tweak injection engine |
| `macho` | ~30KB | Pure-Go Mach-O manipulation |
| `rootless` | ~32KB | Jailbreak package conversion |
| `fetch` | ~10KB | Repo fetching + dependency resolution |
| `appbundle` | ~10KB | App bundle manipulation |
| `tui` | ~56KB | Terminal UI |
| `deb` | ~6KB | Deb parsing/building |
| `device` | ~7KB | go-ios device control |
| `decrypt` | ~14KB | App Store decryption |
| `cyanfile` | ~16KB | .cyan config format |
| `patch` | ~2KB | Compatibility patches |
| `plist` | ~2KB | Plist manipulation |
| `extras` | ~3KB | Embedded frameworks + sideload fixes |
| `artifact` | ~3KB | Extraction manifests |
| `ipa` | ~8KB | IPA pack/unpack |
| `log` | ~2KB | Structured logging |

---

## Data Flow

### Injection Flow

```
Input: .ipa/.tipa/.app + tweaks (.dylib/.deb/.framework/.appex)
  │
  ├─ unzip IPA → extract to tmpdir
  ├─ open app bundle → find main binary, parse Info.plist
  ├─ check encryption → fail unless --ignore-encrypted
  ├─ remove extensions (if requested)
  ├─ fetch tweaks (if --fetch: Canister/MobileAPT → .debs)
  ├─ inject sideload fixes (if --patch)
  ├─ inject tweaks:
  │   ├─ expand .debs → extract artifacts
  │   ├─ pre-scan for libhooker → auto-switch mode
  │   ├─ extract entitlements from main binary
  │   ├─ remove signature from main binary
  │   ├─ for each tweak:
  │   │   ├─ .appex → PlugIns/
  │   │   ├─ .dylib (root) → app root + @executable_path
  │   │   ├─ .dylib (normal) → Frameworks/ + @rpath
  │   │   ├─ .framework → Frameworks/
  │   │   └─ other → app root (copy)
  │   ├─ auto-inject missing hooking frameworks
  │   └─ restore entitlements
  ├─ check references → warn on missing deps
  ├─ modify metadata (name, version, bundle-id, etc.)
  ├─ apply patches (Liquid Glass, force fullscreen)
  ├─ fakesign all binaries
  ├─ thin all binaries
  └─ repack output (IPA with compression, or copy .app)
```

### Extraction Flow

```
Input: .ipa/.tipa/.app (injected)
  │
  ├─ unzip IPA → extract to tmpdir
  ├─ open app bundle
  ├─ scan for injected artifacts:
  │   ├─ Frameworks/ → dylibs, frameworks
  │   ├─ PlugIns/ → appex
  │   └─ app root → other dylibs, bundles
  ├─ copy artifacts to output dir
  ├─ write xkvm-manifest.json (placement + kind)
  └─ done (re-injection reads manifest)
```

### Conversion Flow (rootful → rootless)

```
Input: .deb (rootful)
  │
  ├─ extract deb → payload
  ├─ repack under /var/jb/ (DEBIAN/ stays at top)
  ├─ update control (Architecture, Depends, Icon)
  ├─ rewrite Mach-O load commands under /var/jb
  ├─ rewrite __cstring dlopen strings
  ├─ convert scripts (DEBIAN/ + shebang payloads)
  ├─ apply --tweakinject conventions (if requested)
  ├─ audit surviving rootful paths (warn only)
  └─ rebuild .deb
```

---

## Key Invariants

1. **No external tools** — all Mach-O manipulation is pure Go
2. **Signing is last** — every binary mutation runs before fakesign
3. **Entitlements preserved** — main binary entitlements survive all edits
4. **Atomic writes** — temp file + rename for all binary mutations
5. **Byte-golden tests** — converter outputs pinned to upstream
6. **Test before fix** — every behavior change ships with a test
7. **No emoji in commits** — `xkvm: <summary>` format only

---

## Error Handling

- **Validation errors** → fail fast with clear message
- **Encryption errors** → fail unless `--ignore-encrypted`
- **Missing deps** → warn (not fail); `xkvm check` is the hard gate
- **Device errors** → specific exit codes (64-70)
- **Conversion errors** → fail with upstream deviation documented

---

## Performance Characteristics

- **Injection:** ~5s for 10 tweaks into 50MB IPA (pure Go, no fork)
- **Extraction:** ~2s for typical injected app
- **Conversion:** ~1s per direction (rootful↔rootless↔roothide)
- **Fetching:** ~3-5s per package (network bound)
- **Binary size:** ~15MB static (darwin/arm64)
- **Memory:** ~50MB peak for typical injection

---

## Cross-Platform Matrix

| Platform | Architecture | Status |
|---|---|---|
| macOS | arm64 | Primary (CI leg) |
| macOS | amd64 | Supported |
| Linux | arm64 | Supported |
| Linux | amd64 | Supported (CI leg) |
| Windows | arm64 | Supported |
| Windows | amd64 | Supported (CI leg) |

---

## Security Model

- **No code execution** — all Mach-O work is in-process
- **No network in core** — fetching is opt-in (`--fetch`)
- **Ad-hoc signing only** — no certificate handling
- **Cache validation** — sha256 on downloaded debs
- **Blacklist protection** — system paths never rewritten
- **Entitlement preservation** — main binary entitlements intact
