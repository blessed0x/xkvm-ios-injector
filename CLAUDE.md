# CLAUDE.md — AI Agent Context for xKVM

**Repository:** `github.com/xscope0/xkvm-ios-injector`  
**Binary:** `xkvm` (pure-Go iOS tweak injector & app modifier)  
**Language:** Go 1.26 | **License:** MIT

---

## What xKVM Does

xkvm is a CLI tool that:
1. **Injects tweaks** into iOS apps (.ipa/.tipa/.app) — dylibs, debs, frameworks, bundles
2. **Extracts tweaks** back out of modified apps (with placement manifest for round-trip)
3. **Converts jailbreak packages** between rootful/rootless/roothide formats
4. **Fetches tweaks** from Cydia repos by bundle ID (Canister + MobileAPT)
5. **Checks apps** for missing dependencies (auto-fix with `--fix`)
6. **Generates configs** (.cyan files) for repeatable injection setups
7. **Controls devices** — install/uninstall/launch apps on connected iPhones
8. **Decrypts App Store apps** by Apple ID

---

## Project Structure

```
cmd/xkvm/main.go              Entry point → internal/cli.Main()
internal/
├── cli/                       Cobra CLI, flag definitions, subcommands
├── app/                       Pipeline orchestration (Run, Options, extract)
├── inject/                    Tweak injection engine (per-type placement, dep rewriting)
├── macho/                     Pure-Go Mach-O manipulation (go-macho + pkg/codesign)
│   ├── native.go              Core: editMachO, growing-rename, signing
│   ├── cstring.go             __cstring rewrite for rootless conversion
│   ├── bin.go                 Deps(), RewriteDeps(), AddRpath()
│   ├── der.go                 Code signature DER encoding
│   └── arm64asm.go            ARM64 instruction encoding (ADRP/ADD/ADR)
├── rootless/                  Jailbreak package conversion (rootful↔rootless↔roothide)
│   ├── rootful.go             rootful→rootless
│   ├── rootless.go            rootless→rootful
│   ├── roothide.go            rootless→roothide
│   └── script_rootless.go     Script/plist path conversion
├── fetch/                     Repo fetching (Canister API + MobileAPT)
│   ├── fetch.go               Main resolver
│   ├── canister.go            Canister API client
│   ├── packages.go            Packages.gz/zst/xz/bz2 parsing
│   ├── repos.go               Default repo list
│   └── deps.go                Dependency closure
├── appbundle/                 App bundle manipulation (Info.plist, CgBI, icons)
├── deb/                       .deb parsing and building
├── device/                    go-ios device control (install, syslog, battery)
├── decrypt/                   App Store decryption (ipatool flow)
├── cyanfile/                  .cyan config format
├── tui/                       Terminal UI (interactive menu)
├── patch/                     Compatibility patches (Liquid Glass, force fullscreen)
├── plist/                     Plist manipulation
├── extras/                    Embedded hooking frameworks + sideload fix dylibs
├── artifact/                  Extraction manifests
├── ipa/                       IPA pack/unpack
├── log/                       Structured logging
└── testutil/                  Test fixtures
docs/
├── HANDOFF.md                 Quick-start for new agents (read this first)
├── PRD.md                     Product Requirements Document
├── TTD.md                     Technical Design Document
├── self-improve-protocol.md   Bug-finding protocol
├── ellekit-build.md           ElleKit build instructions
└── roothide-install.md        Roothide installation guide
```

---

## Quick Commands

```bash
# Build
go build -o xkvm ./cmd/xkvm

# Test (all, with race detector)
go test -race ./...

# Full gate (lint + test + cross-compile)
make qa

# Lint only
make lint

# Run
./xkvm -i App.ipa -f Tweak.dylib -o App-Tweaked.ipa
./xkvm tui                          # Interactive mode
./xkvm extract -i App.ipa -o out/   # Extract tweaks
./xkvm check -i App.ipa             # Check for missing deps
./xkvm rootless -i rootful.deb -o rootless.deb
```

---

## Key Architecture Decisions

### Pure-Go Mach-O (M3)
- Replaced ALL external tools (ldid, insert_dylib, install_name_tool, otool)
- `blacktop/go-macho` for parse/modify/rebuild
- `pkg/codesign` for ad-hoc signing
- No platform-specific binary bundles

### Two Hooking Runtimes
- **CydiaSubstrate** (default): `@rpath/CydiaSubstrate.framework/CydiaSubstrate`
- **ElleKit** (`--ellekit`): `@rpath/ElleKit.framework/ElleKit`
- Auto-switch if any tweak needs libhooker (PRE-SCAN before rewriting)

### Root-Dylibs
- `--root-dylib` places dylib in app root with `@executable_path/{name}`
- For dlopen-based tweaks (e.g. Regram) that crash in Frameworks/
- Auto-restore from extraction manifest

### Growing-Rename Pattern
- When a load command dep changes to a longer string, TOC is re-serialized
- All __LINKEDIT-referencing load commands are re-offset
- Atomic write (temp + rename)

### __cstring Rewrite
- Rootless conversion rewrites dlopen strings in __TEXT.__cstring
- In-place when they fit, otherwise packed into __PATCH_ROOTLESS segment
- ADRP/ADD/ADR instruction retargeting

---

## Development Workflow

### Before Any Change
1. Read `docs/HANDOFF.md` (quick facts, command surface, package map)
2. Read relevant section of `ARCHITECTURE.md` (authoritative spec)
3. Follow `docs/self-improve-protocol.md`

### Hard Rules (Do Not Break)
- `internal/macho/native.go` — growing-rename is byte-golden tested
- `internal/macho/cstring.go` — __cstring rewrite is dyld-loading tested
- `check --fix` never mutates input; writes fixed copy
- Converters are byte-golden-tested against upstream output
- Every behavior change ships with a test

### Commit Format
```
xkvm: <single-line summary>
```
No emoji or marketing fluff in docs, help text, or tests.

### Quality Gate
```bash
make qa  # Must be green before committing
```
- Lint: gofmt + vet + tidy -diff + staticcheck + govulncheck
- Test: `-race` across all 18 packages
- Cross-compile: darwin/linux/windows × arm64/amd64

---

## Flag Reference (cyan-compatible)

| Flag | Short | Description |
|---|---|---|
| `--input` | `-i` | App to modify (.app/.ipa/.tipa) |
| `--output` | `-o` | Output path |
| `--file` | `-f` | Tweak to inject (repeatable) |
| `--root-dylib` | — | Place dylib at app root with @executable_path |
| `--name` | `-n` | Modify app name |
| `--app-version` | `-v` | Modify app version |
| `--bundle-id` | `-b` | Modify bundle ID |
| `--minimum-os` | `-m` | Modify minimum OS |
| `--icon` | `-k` | Modify app icon |
| `--plist` | `-l` | Merge plist with Info.plist |
| `--entitlements` | `-x` | Add/modify entitlements |
| `--remove-supported-devices` | `-u` | Remove UISupportedDevices |
| `--no-watch` | `-w` | Remove watch apps |
| `--enable-documents` | `-d` | Enable documents support |
| `--fakesign` | `-s` | Fakesign all binaries |
| `--thin` | `-q` | Thin all binaries to arm64 |
| `--remove-extensions` | `-e` | Remove all extensions |
| `--remove-encrypted` | `-g` | Remove encrypted extensions |
| `--compress` | `-c` | IPA compression level (0-9) |
| `--cyan` | `-z` | .cyan config files (repeatable) |
| `--patch` | — | Inject bundled sideload fixes |
| `--ellekit` | — | Use ElleKit runtime |
| `--fetch` | — | Fetch tweaks by bundle ID |
| `--apt-source` | `-A` | Extra APT repo URLs |
| `--no-recurse` | — | Skip dependency recursion |
| `--decrypt` | — | App Store decrypt |
| `--country` | `-C` | Country code for decrypt |
| `--ignore-encrypted` | — | Skip encryption check |
| `--overwrite` | — | Overwrite without confirming |
| `--silent` | — | Silence everything but errors |
| `--version` | — | Print version and exit |

---

## Subcommands

| Command | Description |
|---|---|
| `xkvm tui` | Interactive menu |
| `xkvm extract` | Pull tweaks out of app |
| `xkvm check` | Check for missing deps |
| `xkvm rootless` | rootful → rootless conversion |
| `xkvm rootful` | rootless → rootful conversion |
| `xkvm roothide` | rootless → roothide conversion |
| `xkvm debify` | Build .deb from dylib |
| `xkvm undeb` | Unpack .deb to artifacts |
| `xkvm cgen` | Generate .cyan config |
| `xkvm cyan-check` | Validate .cyan configs |
| `xkvm cache` | Show/clear fetch cache |
| `xkvm decrypt` | App Store decrypt |
| `xkvm device` | Device control |

---

## Key Files to Read

| File | Why |
|---|---|
| `docs/HANDOFF.md` | Quick-start for new agents |
| `ARCHITECTURE.md` | Authoritative spec (66KB) |
| `internal/app/run.go` | Pipeline orchestration |
| `internal/inject/inject.go` | Core injection engine |
| `internal/macho/native.go` | Mach-O manipulation |
| `internal/rootless/rootless.go` | Package conversion |
| `internal/fetch/fetch.go` | Repo fetching |
| `internal/cli/cli.go` | CLI structure |

---

## Testing

```bash
# All tests with race detector
go test -race ./...

# Specific package
go test -race ./internal/inject/...

# Specific test
go test -race -run TestInjectRootDylib ./internal/inject/...

# Fuzz testing
go test -fuzz FuzzDebExtract -fuzztime 30s ./internal/deb/...

# Golden tests (converter output comparison)
go test -run TestGolden ./internal/rootless/...
```

---

## Common Patterns

### Adding a New Flag
1. Add field to `Options` struct in `internal/app/options.go`
2. Add flag binding in `internal/cli/cli.go`
3. Add validation in `Options.validate()`
4. Add logic in `app.Run()` or relevant handler
5. Add tests

### Adding a New Patch
1. Register in `internal/patch/patch.go`
2. Implement in new file under `internal/patch/`
3. Add tests
4. Document in ARCHITECTURE.md §4.5

### Adding a New Converter
1. Implement in `internal/rootless/`
2. Add byte-golden tests against upstream output
3. Document deviations in ARCHITECTURE.md

---

## References

- [cyan source](https://github.com/xscope0/cyan) — upstream Python tool
- [Azule](https://github.com/itsnotsosimple/azule) — archived, features ported
- [go-macho](https://github.com/blacktop/go-macho) — Mach-O library
- [feather-ellekit-spec.md](feather-ellekit-spec.md) — ElleKit integration spec
