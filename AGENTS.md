# xkvm — agent context

Pure-Go iOS tweak injector: sideload apps, inject tweaks, convert jailbreak
packages (rootful/rootless/roothide), fetch from Cydia repos, check/repair
bundles, decrypt App Store downloads. CLI + TUI share one engine
(`internal/app`).

**Read `docs/HANDOFF.md` first** (quick facts, command surface, package map,
pipeline, current state), then the relevant section of `ARCHITECTURE.md` — the
authoritative spec — before touching any subsystem. Follow
`docs/self-improve-protocol.md`.

## Hard rules (do not break)

- `internal/macho/native.go` — growing-rename: changing a load-command
  dependency/install name to a longer string re-serializes the TOC and
  re-offsets every `__LINKEDIT`-referencing load command. Byte-golden tests pin
  this.
- `internal/macho/cstring.go` — `__cstring` rewrite: dlopen strings are
  rewritten in-place when they fit, otherwise packed into a new
  `__PATCH_ROOTLESS,__cstring` segment inserted before `__LINKEDIT` with
  ADRP/ADD/ADR instruction retargeting. A dyld-loading test pins this.
- `check --fix` never mutates the input; it writes a fixed copy.
- Converters (`internal/rootless`) are byte-golden-tested against upstream
  output; deviations must be documented in `ARCHITECTURE.md`.

## Workflow

1. Write tests first, then fix; every behavior change ships with a test.
2. `make qa` must be green before committing (lint: gofmt + vet + tidy -diff +
   staticcheck + govulncheck; test: `-race` across all 18 packages;
   6-combo cross-compile darwin/linux/windows × arm64/amd64).
3. Staticcheck/govulncheck are pinned in go.mod via the `tool` directive — no
   tools.go, no manual installs.
4. Commit as `xkvm: <single-line summary>`; push after. No emoji or marketing
   fluff in docs, help text, or tests.
