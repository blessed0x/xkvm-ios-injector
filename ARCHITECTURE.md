# xKVM — Architecture & Implementation Plan

**Status:** MVP (M0–M3) + M4 fetch implemented and green — **native-only**: the M2 hybrid toolchain was deleted · **Date:** 2026-08-13 · **Language:** Go · **Binary name:** `xkvm`

xKVM is a from-scratch Go rewrite that merges **pyzule-rw / cyan** (the actively maintained
iOS app modifier / tweak injector) with the missing features of the now-**archived** Azule
(repo-based tweak fetching via Canister/MobileAPT, and on-device App Store decryption).

This document is the implementation blueprint. Facts about the upstream codebases are marked
**confirmed** (read from cloned/fetched source); design calls are marked **decision**.

---

## 1. Why this is worth doing (grounded)

The cyan author's own note — *"PYTHON SUCKS SOMEONE PLEASE REWRITE IN A COMPILED, STATICALLY
TYPED LANGUAGE"* — is backed by hard numbers:

- **Confirmed (from `wc -l` on the clone):** the entire cyan application is **~1,260 lines of
  Python** across 11 modules. This is a small, tractable rewrite — not a big-codebase port.
- **Confirmed (from `cyan/tbhtypes/*.py`):** cyan does *no* binary surgery in Python. It shells
  out to prebuilt compiled tools it ships per-platform:
  `insert_dylib`, `install_name_tool`, `ldid`, `lipo`, `otool`
  (`cyan/tools/{Darwin|Linux}/{x86_64|arm64|iOS}/`), plus hooking frameworks in `cyan/extras/`
  (CydiaSubstrate, Orion, Cephei, CepheiUI, CepheiPrefs) and a `zero.requirements` blob for
  ad-hoc signing.
- **Confirmed (from `main_executable.py`):** LIEF is only a *fallback* used on Linux/aarch64
  where no `insert_dylib` binary ships. Everywhere else the "hard" injection is `insert_dylib --weak --inplace --all-yes`.
- **Confirmed (research):** `blacktop/go-macho` supports **creating/modifying** Mach-O files
  (not just parsing) and `pkg/codesign` does read/write of code signatures including ad-hoc
  signing — the same machinery `ipsw` uses in production.

**Conclusion (as built):** the project shipped **hybrid-first** (M2 embedded the proven
compiled tools cyan uses), then migrated to **pure-Go Mach-O manipulation** (M3: go-macho /
pkg/codesign), and the M2 embedded toolchain has since been **deleted**. The shipped artifact
embeds only the extras payload (hooking frameworks + sideload-fix dylibs) and its Mach-O work
runs natively on any GOOS/GOARCH — no external or embedded tooling.

---

## 2. What we inherit (parity targets)

### 2.1 From cyan (confirmed against source)
| Area | Module | Notes |
|---|---|---|
| CLI | `cyan/__main__.py` | argparse; flags `-i -o -z -f -n -v -b -m -k -l -x -u -w -d -s -q -e -g -c` + `--ignore-encrypted --overwrite --version` |
| Pipeline | `logic.py` | extract → encrypt check → cyans → extensions → inject → plist ops → icon → fakesign/thin → repack |
| Container | `tbhutils.py` | IPA via `unzip`/`zipfile`, `zip -{level}` repack (excludes hidden files), `.app` input, `.tipa` in/out |
| Deb | `tbhutils.extract_deb` | `ar -x` + `tar -xf data.*`; collects dylib/appex/bundle/framework |
| Executable | `tbhtypes/executable.py` | `otool -L` deps; `install_name_tool -change`; **common-dependency fixing** (substrate→ElleKit `@rpath/CydiaSubstrate.framework/CydiaSubstrate`, Orion, Cephei*); `ldid -R/-S -M`; `lipo -thin arm64` |
| Inject | `tbhtypes/main_executable.py` | write entitlements (`ldid -e`), strip sig, add `@executable_path/Frameworks` rpath, per-type inject (appex→PlugIns, dylib/framework→Frameworks, other→app root), **auto-inject missing hooking frameworks from extras/**, sign `-S <ents> -M -Cadhoc -Q zero.requirements` |
| Plist | `tbhtypes/plist.py` | name (incl. `.lproj` localized strings), version, bundle id (incl. appex rewrite), minOS, UISupportedDevices, documents, merge plist |
| AppBundle | `tbhtypes/app_bundle.py` | watch apps, extensions (all/encrypted), icon via Pillow (120/152px + CFBundleIcons), mass fakesign/thin |
| Config | `cgen/__main__.py`, `tbhutils.parse_cyans` | `.cyan` = zip with `config.json` + `inject/` payloads |

### 2.2 From Azule (missing in cyan — confirmed from `azule` + `modules/azule_apt` + `azule_decrypt`)
| Feature | Logic to port |
|---|---|
| **Canister fetch** | **Implemented (M4).** `GET {canisterBase}/package/{name}` → `.data[]` filtered to visible + Free + latest/highest-quality → `repository_id` + `package_filename`; then `GET {canisterBase}/repository/{repo_id}` → `.data.uri`. Endpoint + redirect handling: **see the contract below** |
| **MobileAPT fetch** | **Implemented (M4).** Try `Packages.gz → .zst → (plain) → .xz → .bz2` at `{repo}/Packages*`; parse blank-line stanzas; match by `Package:` name; download `Filename:` as `.deb`. `Depends:` and `SHA256:` are read from the index stanza — no deb download needed for dependency discovery |
| **Dependency recursion** | **Implemented (M4).** Parse `Depends:` from the index stanza, strip version constraints, split `|` alternative groups, filter core deps (`com.ex.substitute`, `org.coolstar.libhooker`, `mobilesubstrate`, `coreutils`, `firmware`, `cy+cpu.arm64`, `gsc.ipad`), recurse via index lookup with a visited-set cycle guard and per-repo index caching; `--no-recurse` disables |
| **App Store decrypt (iOS only)** | `ipatool auth login` + `ipatool download -b <bundleid> -c <country> --purchase`; iTunes lookup for version; on-device `fouldecrypt.{tfp0,kernrw,krw}` selection by jailbreak markers (`/.installed_taurine`, `/.installed_unc0ver`, iOS version) |

**Canister endpoint & redirect contract (verified live 2026-08-13):**

- **Live base:** `https://api.tale.me/v4/canister-services/jailbreak` — held in `canisterBase`
  (a package **var**, not const, so tests point it at an httptest server).
- **Legacy base:** `https://api.canister.me/v2/jailbreak` issues a single **308 Permanent
  Redirect** to the live base with the **URL path suffix preserved**: `{legacy}/{package|repository}/{id}`
  → `{live}/{package|repository}/{id}`. Observed chain (HEAD, 2026-08-13):
  `api.canister.me/v2/jailbreak/package/ws.hbang.alderis` → `308` →
  `api.tale.me/v4/canister-services/jailbreak/package/ws.hbang.alderis` → `200`.
- **Redirect handling (client):** `getJSON` uses Go's default `http.Client` policy — **no custom
  `CheckRedirect`** — so a 308 (or 301/302) is followed automatically, up to the default 10-hop
  cap, preserving method and body. The default path never *needs* the redirect because
  `canisterBase` already targets the live host.
- **Why it's robust:** every derived URL (`/package/{id}`, `/repository/{repoID}`) is constructed
  from `canisterBase`; the client **never parses a redirect `Location:`** to build a follow-up
  request. A future redirect to a differently-shaped URL therefore cannot corrupt the second
  (repo-lookup) hop. If the API moves again, pointing `canisterBase` at the new host — or relying
  on the old domain's 308, should it persist — is the whole migration; the `XKVM_LIVE_FETCH=1`
  smoke test is the tripwire that catches drift.

---

## 3. Language & dependency decisions

**Decision: Go, stdlib `flag`-style single-binary + `spf13/cobra` for subcommands.**
- `xkvm` main command (cyan-compatible flags) and `xkvm cgen` subcommand (`.cyan` generator).
- Cobra gives free `--help`, shell completion, and a clean home for the future `xkvm decrypt`
  command (fetch was folded into `-f`/`--fetch` per §11.2).

**Dependencies (all justified, none speculative):**
| Purpose | Library | Why |
|---|---|---|
| Mach-O parse/modify | `github.com/blacktop/go-macho` | read/write load commands, fat binaries, code signature info (ipsw-proven) |
| Code signature write | `go-macho/pkg/codesign` | ad-hoc SuperBlob / CodeDirectory / entitlements — replaces `ldid` in the pure-Go migration |
| Plist | `howett.net/plist` | binary + XML + openstep plists (Info.plist, entitlements, control) |
| zip | stdlib `archive/zip` | extract/repack; register per-level deflate compressor for `-c 0..9` parity |
| tar / gzip | stdlib `archive/tar`, `compress/gzip` | deb `data.tar.gz` |
| zstd / xz / bzip2 | `github.com/klauspost/compress/zstd`, `github.com/ulikunitz/xz`, `github.com/dsnet/compress/bzip2` | `Packages.{zst,xz,bz2}` + modern debs (`data.tar.zst`, `data.tar.xz`) |
| ar | hand-rolled (~60 lines) | deb container; no dependency needed |
| image | stdlib `image/png` + `golang.org/x/image` | icon conversion (replaces Pillow) |
| YAML/json | stdlib `encoding/json` + `gopkg.in/yaml.v3` | `.cyan` config.json, Canister JSON |

**Decision (superseded):** the original plan embedded the vendored Darwin toolchain via
`go:embed` behind per-platform build tags. **Resolved in M3:** the toolchain embedding was
**deleted** — all Mach-O work goes through go-macho / pkg/codesign, which also closes the
Linux/aarch64 gap that forced cyan to fall back to LIEF. The only remaining `go:embed` is the
extras payload (`internal/extras/extras/`: hooking frameworks + sideload-fix dylibs), which is
injection *content*, not tooling.

**Licensing flag (open decision):** `extras/` contains Cephei (GPLv3-family), CydiaSubstrate,
Orion frameworks. Re-embedding them into xKVM reproduces the same redistribution posture cyan
already has, but we should document provenance + licenses in NOTICE before any public release.

---

## 4. Module layout

```
xKVM/
├── go.mod                        # module github.com/xkvm/xkvm (rename on publish)
├── cmd/xkvm/
│   └── main.go                   # cobra root: cyan-compatible flags
├── internal/
│   ├── cli/                       # cobra root + all flags incl. --fetch/-A/--no-recurse (M0/M4)
│   ├── app/                       # pipeline orchestrator (≈ logic.py); M4 fetch stage lives here
│   ├── appbundle/                # AppBundle ops: watch, extensions, icon, mass ops (≈ app_bundle.py)
│   ├── ipa/                      # zip extract/repack, compression level, hidden-file exclusion
│   ├── deb/                      # ar + tar(.gz/.zst/.xz/.bz2) extraction
│   ├── plist/                    # Info.plist edits: name/version/bundleid/minOS/uisd/documents/merge (≈ plist.py)
│   ├── macho/
│   │   ├── bin.go                # Bin surface — native-only (M3; the M2 toolchain was deleted)
│   │   ├── native.go             # pure-Go ops via go-macho/pkg/codesign (≈ insert_dylib/ldid/otool/lipo)
│   │   ├── der.go                # entitlements DER encoder for ad-hoc signing (≈ ldid)
│   │   └── sdk26.go              # LC_BUILD_VERSION.sdk → 26.0 (Liquid Glass patch)
│   ├── fetch/                    # [M4 ✅] MobileAPT Packages parser + Canister client + dep recursion
│   ├── cyanfile/                 # .cyan zip parse/generate (≈ parse_cyans + cgen)
│   ├── extras/                   # go:embed of hooking frameworks + sideload-fix dylibs
│   └── log/                      # `[*]`/`[?]`/`[!]`/`[<]` output formatter, --silent
└── testdata/                     # fixture IPAs, debs, Mach-Os, .cyan files (committed)
```

**Key interface** (enables the staged migration):

```go
// internal/macho — the seam. Native-only (go-macho + pkg/codesign); the M2 embedded
// toolchain was removed.
type BinaryOps interface {
    InjectDylib(path, dylibPath string, weak bool) error
    ChangeDependency(path, old, new string) error
    AddRpath(path, rpath string) error
    StripSignature(path string) error
    Fakesign(path, entitlements string) error   // adhoc + merge
    ThintoArm64(path string) error
    ListDependencies(path string) ([]string, error)
    IsEncrypted(path string) (bool, error)
}
```

---

## 5. CLI surface (compatibility-first)

**Rule:** cyan's single-letter flags keep **cyan's semantics** — it is the living tool. Azule's
exclusive features get long flags (Azule's `-x`=Apple ID collides with cyan's `-x`=entitlements;
Azule's `-m`=skip-hooking collides with cyan's `-m`=minOS; both resolved in cyan's favor).

| Flag | Meaning (from cyan) | xKVM |
|---|---|---|
| `-i, --input` | app (.app/.ipa/.tipa) | ✔ |
| `-o, --output` | output; default overwrite input | ✔ |
| `-z, --cyan` | `.cyan` config file(s) | ✔ |
| `-f` | tweak/item(s) to inject | ✔ (+ accept **repo package id** when `--fetch` used) |
| `-n -v -b -m` | name / version / bundle id / minOS | ✔ |
| `-k` | icon | ✔ (Go image, drops Pillow) |
| `-l` | plist merge | ✔ |
| `-x` | entitlements | ✔ |
| `-u -w -d -s -q -e -g -c` | uisd / no-watch / documents / fakesign / thin / ext / enc-ext / compress-level | ✔ |
| `--ignore-encrypted --overwrite` | | ✔ |
| `--patch` (new) | inject the bundled sideload dylib set — App Store/keychain repairs + bundled tweaks (`zxPluginsInject`); implies `--fakesign` | ✔ |
| `--ellekit` (new) | use the real ElleKit runtime: rewrite all legacy hooking spellings to `@rpath/ElleKit.framework/ElleKit`, auto-inject + thin the framework to the app's arch | ✔ (feather-ellekit) |
| `--liquid-glass` / `--liquid-glass-compat` (new) | iOS 26 Liquid Glass compat patch: `UIDesignRequiresCompatibility` ± `LC_BUILD_VERSION.sdk → 26.0` (mutually exclusive) | ✔ (feather-ellekit) |
| `--force-fullscreen` (new) | set `UIRequiresFullScreen=true` (sideloaded apps that break under Split View); template for future patches — the CLI binds flags automatically from the `internal/patch` registry | ✔ (feather-ellekit) |
| `--fetch` (new) | fetch tweak by bundle id via Canister/MobileAPT | M4 |
| `--apt-source, -A` (new) | extra repo URL(s) | M4 |
| `--no-recurse` (new) | skip dependency recursion | M4 |
| `--decrypt <email> <pass>` (new) | iOS-only App Store decrypt | M5 |
| `--country, -C` (new) | country code for ipatool/iTunes lookup | M5 |

### 5.1 Compatibility-patch registry (xkvm-native extension point)

Compatibility patches are Feather-style toggles that mutate an extracted app
bundle (`*.app`) before signing. They are **xkvm-native** (not ported from
cyan/Azule), live in `internal/patch`, and are spec'd in `feather-ellekit-spec.md`
§4.5–4.6 (referred to in code as D10).

**Interface** (`patch.go`):

```go
type Patch interface {
    Name() string            // flag name (--<name>) and registry key, kebab-case
    Apply(appDir string) error // in-place mutation of the *.app directory
}
```

**Registration is the only wiring step.** Each patch self-registers in an
`init()` via `Register(&myPatch{})`; duplicate names **panic at startup**
(fail-fast — a mis-registration cannot silently override). The CLI then:

1. binds one `--<name>` bool flag per registered patch (`internal/cli/cli.go`,
   iterating `patch.Names()` in **sorted** order → stable flag order), and
2. collects the enabled set into `opts.Patches` in that same sorted order, so
   `patch.Apply` runs deterministically.

**Pipeline position** (`internal/app/run.go`): after plist/bundle operations,
**before fakesign** — a Mach-O-touching patch mutates the main binary, so it
must precede signing. `Apply` errors on an **unknown name** so a typo'd config
surfaces instead of silently doing nothing.

**Mutual exclusivity** is enforced in `Options.validate()`
(`internal/app/options.go`), not in the registry: `--liquid-glass` and
`--liquid-glass-compat` are rejected together.

**Signature discipline for Mach-O-touching patches** (from `liquidglass.go`):
`ExtractEntitlements → RemoveSignature → edit → SignWithEntitlements` — never
leave the binary unsigned. Plist-only patches need none of this.

**Inventory (3 registered):**

| Flag | Effect | Mach-O? |
|---|---|---|
| `--force-fullscreen` | `UIRequiresFullScreen=true` (kills Split View/Slide Over for broken sideloads) | no — **reference template** |
| `--liquid-glass` | `UIDesignRequiresCompatibility=false` **+** `LC_BUILD_VERSION.sdk → 26.0` on every slice | yes (`bumpSDK26`) |
| `--liquid-glass-compat` | `UIDesignRequiresCompatibility=true` (force legacy appearance) | no |

**Adding a new patch** (canonical recipe, documented in `forcefullscreen.go`):
create `internal/patch/<name>.go` with an `init()` registering it; implement
`Name()`/`Apply()`; if it touches a Mach-O follow the strip-edit-resign
discipline above; add a unit test mirroring `TestForceFullScreen` (assert the
key, assert unrelated keys survived, `SkipUnlessNativeToolchain`); enforce any
mutual exclusivity in `Options.validate()`. e2e: the liquid-glass pair is
stage [9/10] and force-fullscreen stage [10/10] in `scripts/e2e-real.sh`.

---

## 6. Pipeline (parity with `logic.py`, extended)

```
validate inputs
extract/copy app (.ipa → unzip-style, .tipa, .app)
encryption check on main executable        (--ignore-encrypted overrides)
parse .cyan config(s)                       → merge into args (inject/, icon, plist, entitlements)
remove extensions (all | encrypted only)
  ├─ [M4 ✅] fetch stage: resolve any -f entries that are repo bundle ids → .debs
  ├─ extract .debs                          (ar + data.tar.*, collect dylib/framework/appex/bundle)
  ├─ fix common dependencies (substrate→ElleKit, orion, cephei*)
  ├─ auto-inject missing hooking frameworks from extras/
  └─ inject dylib/framework → Frameworks, appex → PlugIns, other → app root
apply plist ops (name/version/bundleid/minOS/merge/uisd/documents)
apply compatibility patches (internal/patch registry — §5.1; after plist ops, before fakesign)
change icon                                 (120/152px + CFBundleIcons)
mass fakesign all binaries (or thin to arm64)
repack .ipa (compression level, exclude hidden files) | emit .app
```

---

## 7. Test strategy

- **Unit:** plist round-trips (binary/xml fixtures); deb extraction (build synthetic `.deb`
  fixtures in-test with `ar` + tar); Packages-file parsing (gzip/zstd/xz/bz2 fixtures);
  dependency-path fixing (table tests — the `@executable_path/libsubstrate.dylib` →
  `@rpath/CydiaSubstrate...` and "already-fixed name" regressions from cyan v1.4/v1.4.1 are
  **mandatory test cases**); `.cyan` parse/generate round-trip.
- **Mach-O (pure-Go migration):** fixture Mach-Os built by tests + committed testdata binaries;
  assert load-command sets after inject/change/sign/thin via go-macho parse.
- **E2E golden tests:** small committed fixture IPA → run pipeline → assert (a) zip structure,
  (b) injected load command present, (c) Info.plist keys, (d) code signature parses.
- **Differential harness (highest-value):** run the *same* input through cyan (reference) and
  xkvm; diff the semantic output (file sets, load commands, plist keys — not bytes). Every
  feature-port milestone must pass differential parity before being marked done.
- **CI:** GitHub Actions matrix (macos-14 arm64, ubuntu-latest x64); a lint job runs
  `make lint` (gofmt + `go vet` + staticcheck pinned in go.mod via `tools.go`) and the test
  matrix runs `go test -race ./...`; on-release `goreleaser`.

---

## 8. Milestones

| # | Milestone | Scope | Est. | Depends on |
|---|---|---|---|---|
| M0 | **Scaffold** | go.mod, cobra CLI (full flag surface), logging, exit codes, CI skeleton | 0.5 d | — |
| M1 | **Containers** | `ipa`, `deb`, `plist` packages; extract/repack; `.app` in; compression level; golden tests | 1 d | M0 |
| M2 | **Injection parity (hybrid)** | `macho/toolchain` embedding (**deleted** — superseded by M3); full `inject()` parity: dep fixing, extras auto-inject, entitlements, fakesign, thin, icon, watch/extensions, plist ops — **differential parity vs cyan** | 2–3 d | M1 |
| M3 | **Pure-Go Mach-O** ✅ | insert_dylib → `macho/native`; ldid → `pkg/codesign`; otool→deps; lipo→thin; **fixes Linux/aarch64 LIEF hole**; the M2 toolchain embed was removed after M3 landed | 3–5 d | M2 |
| M4 | **Azule fetch** ✅ | `fetch` package: MobileAPT + Canister + dep recursion; `xkvm --fetch`/`-A`/`--no-recurse`; live smoke test (Canister v4 + real repo) gated behind `XKVM_LIVE_FETCH=1` | 2–3 d | M1 |
| M5 | **iOS on-device** | cross-compile `GOOS=darwin GOARCH=arm64`; `--decrypt` (ipatool + fouldecrypt variants); Roothide fs caveats documented | 1–2 d | M3 |
| M6 | **Ship** | Homebrew tap, goreleaser release flow, shell completion, README, NOTICE/licenses | 1 d | M4/M5 |
| FE | **Feather reference + ElleKit** | mode-keyed `commonDeps` (substrate vs real ElleKit runtime), libhooker auto-switch pre-scan, `internal/patch` registry + Liquid Glass patches, `BumpSDK26`, vendored fat ElleKit.framework, e2e-real patch stages [9/10]/[10/10] | 1 d | M3 |

**MVP = M0–M3** (~4–5 focused days): a globally-installable, **native-only** `xkvm` with full cyan parity.
**Full Azule parity = M4–M5.** Pure-Go endgame = M3 (the payoff, but non-blocking).
**Feather/ElleKit milestone (FE)** — spec: `feather-ellekit-spec.md`; build recipe: `docs/ellekit-build.md`.

---

## 9. Distribution & install

- `go install github.com/xkvm/xkvm@latest` (or `./cmd/xkvm` build) → global binary.
- Release flow: goreleaser (darwin arm64/x86_64 + linux amd64/arm64 tarballs), Homebrew tap.
- Jailbroken iOS: single static arm64 Mach-O, ad-hoc signed, no runtime deps — the upgrade over
  cyan's Python-on-device story. M5 scope.

---

## 10. Risks & mitigations

| Risk | Mitigation |
|---|---|
| Mach-O injection correctness on real IPAs | Hybrid-first (proven `insert_dylib`/`ldid`); pure-Go migration gated on differential parity tests |
| Dep-path fixing edge cases (substrate as `libsubstrate.dylib` vs framework vs `CydiaSubstrate.dylib`) | Table-driven tests encoding cyan's `common{}` map + v1.4/v1.4.1 regression cases |
| Binary plist corner cases | `howett.net/plist` (mature); round-trip golden fixtures |
| ~~Vendored toolchain size in binary~~ | **Resolved:** the M2 embedded toolchain (~11 MB) was deleted; the binary embeds only the extras payload |
| License obligations for embedded frameworks | NOTICE + provenance doc before public release (M6) |
| Canister/MobileAPT API drift | **Drift realized and absorbed:** the spec'd `api.canister.me/v2` endpoint 308s to `api.tale.me/v4` (single hop, suffix-preserving — see the redirect contract in §2.2); `canisterBase` is a package var so the live endpoint is re-pointable without code changes. Hermetic httptest coverage + a network-gated live smoke test (`XKVM_LIVE_FETCH=1`) stay out of CI; `--no-recurse` escape hatch |
| Roothide jailbreak fs (`/rootfs` + `/.jbroot`) | Documented limitation (cyan marks it wontfix); scope M5 to standard rootless/rootful |

---

## 11. Open decisions (resolved at M0)

1. **Module path** — placeholder `github.com/xkvm/xkvm`; set real org on publish.
2. **`xkvm fetch` UX** — **resolved (M4):** folded into `-f` + `--fetch` (tweak ids resolve through Canister/MobileAPT into debs before injection).
3. **`.cyan` compat** — must accept existing cyan-generated files verbatim (backward compat is a
   hard requirement; `xkvm cgen` output stays byte-compatible).
4. **Toolchain provenance** — **resolved:** M3 replaced the embedded binaries with
go-macho/pkg/codesign and the toolchain embedding was deleted; xkvm ships native-only.
