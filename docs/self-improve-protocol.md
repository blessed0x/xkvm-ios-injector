# Self-Improve Protocol

A repeatable loop for finding bugs — including the hidden ones — and sloppy,
duplicated, and dead code before it ships. Evidence-first: every finding is
confirmed before it is fixed, and absence of evidence is not a finding.

This protocol is written for this repo. Every command runs as-is from the
repo root. The bug classes in the tables are not theory — each one has bitten
this project, with the example cited.

## When to run it

- After every milestone or feature batch (the M-series, `check --fix`, the
  converters, the fetch solver).
- Before pushing a batch that touches more than one package.
- A full pass when a platform leg, a dependency, or a converter direction is
  added — that is when the hidden classes appear (the Windows leg found six
  real bugs its first time running the suite).

## Phase 0 — Baseline

Record the ground state before touching anything.

```bash
git status --short
git log -1 --oneline
git fetch origin && git rev-list --count HEAD..origin/main   # unpushed?
make lint; echo "lint exit=$?"
go test -race ./... 2>&1 | tail -5                          # ok/FAIL counts
```

- Read the pass/fail numbers from the gate's final output, not memory.
- Note the mtime of any golden fixture you will lean on — a fixture older than
  your change makes a green result suspect.
- "No regressions" is only meaningful against this captured diff.

## Phase 1 — Static analysis

### 1a. Mechanical gates

Must be green; no judgment involved.

```bash
make lint        # gofmt-check + go vet + tool-check (go mod tidy -diff) + staticcheck + govulncheck
gofmt -l cmd internal   # must print nothing
```

staticcheck and govulncheck are pinned in `go.mod` via the `tool` directive, so `make lint` and CI run
the same binary. Pay attention to the check classes:

- `U1000` — dead/unused code (the primary dead-code detector)
- `SA*` — actual bugs (nil deref, lost errors, unreachable)
- `S*` — simplifications (usually safe to take)
- `ST*` — style (only worth fixing in code you touch)

### 1b. Heavier one-shot linters

Ephemeral `go run` invocations — no repo change required:

```bash
go run github.com/client9/misspell/cmd/misspell@latest internal/ scripts/ docs/
go run golang.org/x/vuln/cmd/govulncheck@latest ./...     # dependency CVEs
golangci-lint run                                          # if installed: dupl,
                                                           # gocyclo, ineffassign,
                                                           # unconvert, prealloc
```

### 1c. The slop / duplicate / dead pass (hand greps)

No tool finds these. This is the part most often skipped, so do it as its own
step, not as an afterthought.

```bash
# debug leftovers in the wrong place
grep -rn 'fmt.Println\|log.Printf\|os.Stdout' --include='*.go' internal/ | grep -v _test

# shipped TODOs
grep -rn 'TODO\|FIXME\|XXX\|HACK' --include='*.go' internal/ cmd/

# duplicate helpers: two implementations of the same thing
grep -rn 'func toSlash\|func.*Slash\|func human' --include='*.go' internal/
```

What to hunt:

- **Duplicated logic** — the same transformation implemented twice with slight
  drift. This repo shipped `ToSlash` fixes in two separate converters for the
  same bug class; a helper used in three places is correct, three copies of it
  are a defect you have not seen yet.
- **Dead code that can never run** — functions nothing calls, flags parsed but
  never read, branches whose condition is constant. staticcheck U1000 finds the
  compiler-visible half; the rest needs a reader.
- **Useless guards** — `[ -f x ] && . x` where x never exists, `|| true`
  swallows, error returns that are always nil. A guard that cannot fire is a
  lie about intent.
- **Leftover experiments** — vestigial backends and embed payloads. This repo
  deleted its M2 toolchain backend entirely; look for the same shape in every
  refactor (a switch arm that now points nowhere).
- **Duplicated constants** — the same path string exported six times is a smell
  even when it is runtime-free; it drifts.

### 1d. The hidden-bug trap checklist

When reviewing a change, check it against the classes its shape exposes. Each
row is a class that has actually bitten this repo.

| Class | What to look for | Real example |
|---|---|---|
| Bounds / overflow on extreme input | loops indexing with derived exponents; unguarded size math | `HumanBytes` panicked on >= 1 PiB — `"KMGT"[4]` out of range |
| Destructive side effect on user data | TTL / prune / cleanup that runs on paths the user chose | the 7-day cache prune deleted `.debs` from the user's own download folder |
| Dangling path after temp cleanup | a function returns a path inside a scratch dir it deletes before the caller copies | `check --fix` staged deb artifacts in a dir that was removed before use |
| Platform path semantics | `filepath.IsAbs` / `filepath.Dir` / `filepath.Join` differ off-UNIX | `IsAbs("/etc/passwd")` is false on Windows — the symlink-escape guard never fired |
| Line endings / encodings | golden pins, patch patterns, scripts | Windows CRLF checkout broke sha pins and LF-anchored patterns — fixed with `.gitattributes` |
| Early return skipping required work | `return nil` before re-check / re-sign / repack | unfixable apps exited 0 — the re-check was skipped |
| Test fixture conflation | one fixture satisfies two checks at once, hiding the gap | a tier-1 placement incidentally satisfied the tier-2 dlopen test |
| Swallowed errors | `|| true`, `_, _ =`, `head` after `curl -f` | the release-API probe masked a 404 — the pipe ate curl's exit code |
| Silent skips in CI | native-gated tests not running on the leg you think | verbose CI re-runs were added so skips are visible in the log |
| State that must survive re-entry | caches, manifests, placement memory | the extract manifest remembers each dylib's original placement for re-injection |

### 1e. Contract drift

Anything load-bearing that comes from outside this repo — the Canister v4
endpoint, upstream repo layouts, error strings, goreleaser asset names — verify
it live before trusting it, and re-verify when the protocol pass touches it.
Comments and READMEs go stale silently; the upstream source does not.

## Phase 2 — Dynamic analysis

Static analysis finds what the code says. Dynamic finds what the code does.

### 2a. Race detector on the whole suite

```bash
go test -race ./...
```

Shared state lives in the fetch cache, the solver, and the TUI — the places
most likely to race.

### 2b. Cross-compile matrix

Catches the platform classes before CI does:

```bash
for os in darwin linux windows; do
  for arch in arm64 amd64; do
    GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build ./... || exit 1
  done
done
```

`go build` is the check that fires on the path-separator / embed-path classes;
compiling is not proof, but the Windows leg of CI is the permanent oracle for
the rest.

### 2c. Fuzzing the parsers

There are no fuzz targets in this repo yet — the highest-value additions are:
deb ar reader, multi-codec tar decompression, ipa zip extraction (zip-slip),
the MobileAPT Packages parser, and the plist layer.

```bash
go test -fuzz=FuzzDebAr -fuzztime=30s ./internal/deb/
go test -fuzz=FuzzIPASlip -fuzztime=30s ./internal/ipa/
```

Also run round-trip properties manually where a fuzz harness does not fit:
extract -> repack -> extract byte-identical; convert -> convert-back preserves
the load-command set; `check --fix` output re-checks clean.

### 2d. Live end-to-end against real artifacts

```bash
bash scripts/e2e-real.sh      # real tweak + real app IPA pipeline
bash scripts/check-smoke.sh   # ten hermetic fixture IPAs, generic names
bash scripts/smoke.sh
```

Verify output with a second tool, never the tool's own report of itself:

```bash
otool -L <binary>                  # load commands after inject/convert
codesign --verify --verbose=2 ...  # after re-sign
dpkg-deb -c <out.deb>              # member layout after convert
plutil -p <out.plist>              # after plist rewrites
```

### 2e. CI as the oracle

Push and read the actual matrix. A green local Linux run is not proof — the
native-toolchain tests skip everywhere but macOS. The Windows leg is
first-class, not an afterthought; its first green run is when the hidden
platform bugs surface.

## Phase 3 — Skeptical self-review (before delivery)

- Read the diff as an adversary, not as its author.
- Model the candidate fix against the full case set it must not regress — the
  boundary on each side, the sibling converter direction, the parallel code
  path the report never mentioned. The obvious fix routinely breaks the case
  the report did not include.
- Confirm before flagging. Verify a suspected problem exists — grep, diff, run
  it — before reporting it. An unverified flag manufactures doubt; absence of
  evidence is not the finding.
- Name the one claim most likely to be wrong, and what only the user can
  verify from their seat: on-device launch, a real network, a real release.

## Phase 4 — Report and audit

Every pass ends with a written delta:

- Baseline -> after: "x failing {a,b} -> still x failing {a,b}", or "now y:
  +c, caused by my change". Read from the gate's final output.
- Label each claim confirmed (with evidence) vs inferred (with what would
  confirm it).
- Name the paths you exercised and the ones you did not: native-only paths,
  network fetches, device behavior.
- List the bugs found, their fixes, and a one-line rollback for each.

## Appendix — command cheat sheet

```bash
make lint                              # static gate (gofmt + vet + tidy + staticcheck)
go test -race ./...                    # dynamic gate
# cross-compile matrix (Phase 2b)
for os in darwin linux windows; do for arch in arm64 amd64; do \
  GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build ./... || exit 1; done; done
go run github.com/client9/misspell/cmd/misspell@latest internal/ scripts/ docs/
go run golang.org/x/vuln/cmd/govulncheck@latest ./...
bash scripts/e2e-real.sh
bash scripts/check-smoke.sh
```

## Appendix — the loop in one line

Baseline -> static (mechanical, then heavy, then hand) -> dynamic (race,
cross-compile, fuzz, live E2E, CI) -> skeptical review -> report with deltas.

See also: [CONTRIBUTING.md](../CONTRIBUTING.md) for the commit-side rules the
protocol gates on, and [ARCHITECTURE.md](../ARCHITECTURE.md) for the fidelity
contracts a change must not drift from.
