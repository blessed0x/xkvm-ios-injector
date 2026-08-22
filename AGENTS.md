# xkvm — AI Agent Context

Pure-Go iOS tweak injector: sideload apps, inject tweaks, convert jailbreak
packages (rootful/rootless/roothide), fetch from Cydia repos, check/repair
bundles, decrypt App Store downloads. CLI + TUI share one engine
(`internal/app`).

**Repository:** `github.com/xscope0/xkvm-ios-injector`  
**Binary:** `xkvm`  
**Language:** Go 1.26 | **License:** MIT

---

## Quick Start

**Read `docs/HANDOFF.md` first** (quick facts, command surface, package map,
pipeline, current state), then the relevant section of `ARCHITECTURE.md` — the
authoritative spec — before touching any subsystem. Follow
`docs/self-improve-protocol.md`.

**See also:**
- `CLAUDE.md` — AI agent context (flags, subcommands, key files, patterns)
- `docs/PRD.md` — Product Requirements Document
- `docs/TTD.md` — Technical Design Document
- `docs/ARCHITECTURE_OVERVIEW.md` — High-level architecture overview

---

## Hard Rules (Do Not Break)

1. **`internal/macho/native.go`** — growing-rename: changing a load-command
   dependency/install name to a longer string re-serializes the TOC and
   re-offsets every `__LINKEDIT`-referencing load command. Byte-golden tests pin
   this.

2. **`internal/macho/cstring.go`** — `__cstring` rewrite: dlopen strings are
   rewritten in-place when they fit, otherwise packed into a new
   `__PATCH_ROOTLESS,__cstring` segment inserted before `__LINKEDIT` with
   ADRP/ADD/ADR instruction retargeting. A dyld-loading test pins this.

3. **`check --fix`** never mutates the input; it writes a fixed copy.

4. **Converters** (`internal/rootless`) are byte-golden-tested against upstream
   output; deviations must be documented in `ARCHITECTURE.md`.

---

## Workflow

1. **Write tests first**, then fix; every behavior change ships with a test.
2. **`make qa` must be green** before committing:
   - Lint: gofmt + vet + tidy -diff + staticcheck + govulncheck
   - Test: `-race` across all 18 packages
   - Cross-compile: darwin/linux/windows × arm64/amd64
3. **Staticcheck/govulncheck** are pinned in `go.mod` via the `tool` directive
   — no tools.go, no manual installs.
4. **Commit format:** `xkvm: <single-line summary>`; push after. No emoji or
   marketing fluff in docs, help text, or tests.

---

## Key Commands

```bash
# Build & Test
go build -o xkvm ./cmd/xkvm        # Build
go test -race ./...                  # All tests
make qa                              # Full gate
make lint                            # Lint only

# Run
./xkvm -i App.ipa -f Tweak.dylib -o App-Tweaked.ipa
./xkvm tui                          # Interactive mode
./xkvm extract -i App.ipa -o out/   # Extract tweaks
./xkvm check -i App.ipa             # Check for missing deps
./xkvm rootless -i rootful.deb -o rootless.deb
```

---

## Project Structure

```
cmd/xkvm/main.go              Entry point → internal/cli.Main()
internal/
├── cli/                       Cobra CLI, flag definitions
├── app/                       Pipeline orchestration (Run, Options, extract)
├── inject/                    Tweak injection engine
├── macho/                     Pure-Go Mach-O manipulation
├── rootless/                  Jailbreak package conversion
├── fetch/                     Repo fetching + dependency resolution
├── appbundle/                 App bundle manipulation
├── deb/                       Deb parsing/building
├── device/                    go-ios device control
├── decrypt/                   App Store decryption
├── cyanfile/                  .cyan config format
├── tui/                       Terminal UI
├── patch/                     Compatibility patches
├── plist/                     Plist manipulation
├── extras/                    Embedded frameworks + sideload fixes
├── artifact/                  Extraction manifests
├── ipa/                       IPA pack/unpack
├── log/                       Structured logging
└── testutil/                  Test fixtures
```

---

## Key Invariants

- **No external tools** — all Mach-O manipulation is pure Go
- **Signing is last** — every binary mutation runs before fakesign
- **Entitlements preserved** — main binary entitlements survive all edits
- **Atomic writes** — temp file + rename for all binary mutations
- **Byte-golden tests** — converter outputs pinned to upstream
- **Test before fix** — every behavior change ships with a test
- **No emoji in commits** — `xkvm: <summary>` format only

---

## Key Files

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
