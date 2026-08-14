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

**xkvm-native extension: root-placed dylibs (`--root-dylib`).** cyan always ships every
`.dylib` into `Frameworks/` with an `@rpath` load command. Some dlopen-based tweaks — the
licensing/registration dylib of Regram-family mods is the canonical case — resolve their own
resources relative to `@executable_path` and crash when relocated (the mod that ships them
loads them from the app root with an `@executable_path/{name}` weak load). `--root-dylib`
selects that contract per file: placement in the app root, `@executable_path/{name}` load on
the main binary, and `fixInjectedDeps` rewrites other tweaks' references to it as
`@executable_path/{name}` (never `@rpath`). A root-only run never creates `Frameworks/` nor
adds the `@executable_path/Frameworks` rpath. Regression-pinned in
`internal/inject/inject_test.go` (`TestInjectRootDylibUsesExecutablePath` et al.).

**Extraction placement memory (`xkvm extract`, xkvm-native).** `xkvm extract` writes a
`xkvm-manifest.json` sidecar next to the extracted artifacts recording each artifact's
original placement in the source bundle (`root` / `frameworks` / `plugins` / `other`, plus a
kind). Re-injection auto-restores it: when a `-f` dylib lives under a directory with a
manifest, a dylib recorded as `root` is re-rooted with its `@executable_path` contract
without any `--root-dylib` flag — the Regram extract→re-inject round-trip is automatic.
Frameworks/plugins/other placements need no action (the injector already sends those to
their canonical locations by default). Explicit `--root-dylib` entries merge in.
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
| `--root-dylib` (new) | inject dylib(s) to the **app root** with an `@executable_path` load command instead of Frameworks/`@rpath` — for dlopen-based tweaks (e.g. Regram) that resolve resources relative to `@executable_path` and crash when relocated to Frameworks; use together with `-f <same file>` to also carry the file into the injection set | ✔ |
| `xkvm cgen -o out.cyan` (new, implemented) | generate a shareable `.cyan` config (`-f` payloads → `inject/`, `--root-dylib` marks app-root payloads, `-n`/`-s`/`--ellekit`/`--patch` baked) — upstream pyzule-rw cgen parity + the xkvm `root_dylibs` key | ✔ |
| `xkvm cyan-check <file.cyan>...` (new, implemented) | validate `.cyan` config(s) **without applying**: read-only report of errors (root_dylibs↔payload mismatches, missing k/l/x payloads, unsafe paths, unknown patch names) and warnings (unknown keys, odd types); exit 1 on any error, 0 with warnings only | ✔ |
| `xkvm extract -i <app> -o <dir>` (new, implemented) | dump tweak artifacts (dylibs/frameworks/bundles/appex) from an app/ipa/tipa **and write `xkvm-manifest.json`** recording each artifact's original placement; re-injecting those files honors it automatically (§5.3) | ✔ |
| `xkvm check -i <app|ipa>` (new, implemented) | **merge-completeness check**: scan every bundle-relative load-command dependency (`@rpath/`/`@executable_path`/`@loader_path`) against the bundle's Frameworks/root inventory and report any unresolved reference — the ffmpegkit-gap detector; exit 1 on any missing | ✔ |
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

### 5.1 `.cyan` config format (confirmed from upstream + `root_dylibs` extension)

`.cyan` is a zip with `config.json` + payloads — the exact upstream pyzule-rw shape:

- `config.json` is a **flat dict of flag values**; file-valued flags (`-f/-k/-l/-x`) are stored
  as `true` and their payload files ship in the archive (`inject/`, `icon.idk`, `merge.plist`,
  `new.entitlements`).
- **Merge semantics (upstream `parse_cyans`, confirmed):** `inject/` payloads **append** to the
  `-f` file set (basename-dedup resolves collisions in the config's favor — config wins);
  `icon.idk`/`merge.plist`/`new.entitlements` are extracted by key; **every remaining key
  overrides the corresponding CLI arg** (`args[k] = v`).
- **xkvm extension: `root_dylibs`** — an array of `inject/` payload basenames that must be
  placed in the **app root** with an `@executable_path` load command (the `--root-dylib`
  contract). Values are basenames, matching how `inject/` payloads are referenced; a basename
  with no matching payload is a config error.
- Unknown keys are ignored (forward compatibility: a config from a newer xkvm still applies
  the keys this version knows). Zip-slip payload paths are rejected.
- Configs are applied in `-z` order; later configs win over earlier ones for scalar keys.

Implemented in `internal/cyanfile` (`Parse`/`Generate`/`Validate`), consumed by `app.Run` **before**
`validate()` (payloads must be materialized before the file-existence gate), by
`xkvm cgen` (implemented — replaces the M4 stub), and by `xkvm cyan-check`
(`Validate` — read-only, no payload materialization). Round-trip-pinned in
`internal/cyanfile/cyanfile_test.go` and `TestCGenGeneratesRootDylibConfig`;
cyan-check exit semantics pinned in `TestCyanCheck{Valid,Invalid,WarningsOnly}`.

**`xkvm cyan-check` contract.** `Validate(path, knownPatches)` opens the archive
and reports issues without extracting anything: **errors** are exactly the
conditions `Parse`/apply would fail on (a `root_dylibs` basename with no
matching `inject/` payload — same message as Parse; a `k`/`l`/`x` key whose
archive payload is absent; an unsafe `inject/` path; an unknown patch name when
the registry is passed). **warnings** are forward-compatible or benign (unknown
config keys — a newer-xkvm config must still pass; wrong-typed scalars that
Parse silently ignores; `f` set with no payloads). CLI: one line per finding,
`0` errors → exit 0, any error → exit 1; unreadable/not-a-zip is a hard error.

**`xkvm check` — merge-completeness gate (xkvm-native).** Two tiers of
bundle-relative dependency scanning across every Mach-O in the app:

1. **Tier 1 (deterministic, load commands):** every `@rpath/X.framework`,
   `@rpath/X.dylib`, `@executable_path/X`, `@loader_path/X` dependency must
   resolve inside the bundle — `@rpath` against `Frameworks/`, `@executable_path`
   against the app root (or `Frameworks/` for a leading `Frameworks/` component),
   `@loader_path` against the referencing binary's own directory. OS-provided
   Swift runtime shims (`@rpath/libswift*.dylib`) are exempt — dyld resolves
   those via `/usr/lib/swift` on iOS 12.2+, apps never bundle them. System
   paths (`/usr/lib`, `/System/Library`) are ignored.
2. **Tier 2 (heuristic, runtime dlopen):** bare `NAME.framework` tokens in the
   strings of **non-main** binaries — the runtime-dlopen signature with no
   load command (RyukGram's settings UI `dlopen`s `ffmpegkit.framework` by
   string; this is the gap the check exists to catch). Tokens inside paths
   (`@rpath/…`, `/System/…`) are exempt. Reachability is scoped by the
   referencing binary's **tweak family** (the stem of the `.bundle`/`.appex`
   it lives in, or of a root-level `<Name>.dylib`): frameworks in
   `Frameworks/` and at the app root are reachable from any binary; a
   framework nested inside a `.bundle` is reachable only from binaries of the
   same family. This is the distinction between the two real cases — Regram
   loads FLEX from its own `Regram.bundle` (reachable; the RC build worked),
   while RyukGram's ffmpegkit dlopen searches Frameworks//its own bundle, so
   a copy nested inside `Regram.bundle` is **not** reachable (the FIXED build
   crashed on device despite carrying that copy). Apple/OS frameworks are
   exempt. Findings are marked `Suspected` (heuristic, not as certain as a
   tier-1 load command).

`app.Run` auto-warns after every injection (both tiers); the `xkvm check`
command is the hard gate (exit 1 on any missing ref — tier 2 included, since
the dlopen class is exactly the runtime crash this catches). Pinned by
`TestCheckReferences*` (appbundle: tier-1 gap/shipped/root, Swift-shim
allowlist, tier-2 dlopen flagged + shipped-at-root/system clean),
`TestRunCheckCatchesMissingInjectedFramework` (app e2e), and
`TestCheckCmdExitCodes` (cli).

**Apply-flow hook (auto-validation).** `app.Run` runs the same `Validate` on
every `-z` config inside `mergeCyans` **before** `Parse`, so a broken config is
rejected before any extraction or injection happens. Error-level findings log
the full report and abort the run with `".cyan failed cyan-check (N error(s));
nothing was applied"` — this catches conditions `Parse` can't (a bad patch
name, which would otherwise fail mid-pipeline in `patch.Apply` after partial
work). Warning-level findings are logged and the config still applies. Pinned
by `TestRunRejectsInvalidCyanConfig`, `TestRunRejectsUnknownPatchInCyanConfig`,
and `TestRunAcceptsWarningsOnlyCyanConfig`.

### 5.2 Extraction placement memory (`xkvm extract`)

`xkvm extract` copies every injectable artifact (dylib/framework/bundle/appex) out of an
app/ipa/tipa and writes `xkvm-manifest.json` next to them:

```json
{
  "format": 1,
  "source": "in.ipa",
  "artifacts": [
    { "name": "Regram.dylib",              "kind": "dylib",     "placement": "root" },
    { "name": "Sparkle.dylib",             "kind": "dylib",     "placement": "frameworks" },
    { "name": "Sparkle.bundle",            "kind": "bundle",    "placement": "root" },
    { "name": "OpenInRegramExtension.appex", "kind": "appex",  "placement": "plugins" }
  ]
}
```

- **Placement** is decided by the artifact's path relative to the bundle: first-component
  `Frameworks/` → `frameworks`, `PlugIns/` → `plugins`, a top-level entry → `root`, else
  `other`.
- **Re-injection auto-honors it** (`app.Run` → `rootDylibsFromManifests`): each `-f` `.dylib`
  walks up (≤4 ancestor levels, cached) to the nearest `xkvm-manifest.json`; a dylib recorded
  as `root` is added to `RootDylibs` automatically — the `@executable_path` contract is
  restored with zero flags. A corrupt/unreadable manifest logs a warning and falls back to
  default placement; it never aborts the injection.
- **Design note:** the manifest only needs to *remember root dylibs* — frameworks/bundles/
  appex already re-inject to their canonical places by default. The `root` flag is the one
  placement the injector would otherwise destroy, and it's exactly the Regram-class crash
  this prevents.

Implemented in `internal/artifact/manifest.go` (types + read/write) and
`internal/app/run.go` (`ExtractArtifacts` writes; `rootDylibsFromManifests` reads).
Pinned by `TestExtractReinjectHonorsPlacement` (full extract → re-inject round-trip on real
Mach-O fixtures: `@executable_path` restored with no `--root-dylib`).

### 5.3 Compatibility-patch registry (xkvm-native extension point)

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

### 5.4 Package commands: `debify`, `undeb`, `rootless` (xkvm-native, three upstream references)

Three subcommands implement the tweak-package lifecycle around the existing
ipa/deb layers, porting three upstream tools:

| Command | Upstream reference | What it does |
|---|---|---|
| `xkvm debify` | `cs59/Dylib-to-Deb-Converter` (GPL-3.0, format reference only) | dylib (or payload dir) → standard MobileSubstrate `.deb` |
| `xkvm undeb` | `Dr-Sauce/Forte` (no license — a Shortcut that unarchives a deb) | `.deb` → extracted dylibs + resources + placement manifest |
| `xkvm rootless` | `NightwindDev/rootless-patcher` (MIT — semantics ported) | rootful `.deb` → rootless `.deb` |

All three are **format/semantics ports — no upstream code is copied** (see
NOTICE). `debify` produces the Dylib-to-Deb-Converter layout: `DEBIAN/control`
+ `Library/MobileSubstrate/DynamicLibraries/{pkg}.dylib` + a `Filter/Bundles`
plist (from `--bundle-id`, or `--filter` for an exact one); `--resource
src:dest` adds arbitrary payload files; `--depends` replaces the default
`mobilesubstrate`. `undeb` is `xkvm extract` for debs: it reuses
`deb.Extract` + the §5.2 placement manifest, so a deb-extracted dylib
re-injects with its original placement remembered.

**The deb writer** (`internal/deb/build.go`): `deb.Build(stagingDir, dest)`
packages `DEBIAN/` → `control.tar.gz` and everything else → `data.tar.gz`
with the canonical `./` entry prefix, symlinks preserved, timestamps zeroed
(reproducible builds). `deb.Unpack` extracts BOTH tars (the reader's
`Extract` only handles data.tar) — the full-payload path the rootless
converter needs.

**`xkvm rootless`** ports the rootless-patcher pipeline (`internal/rootless`):

1. **Repack** — every payload entry moves under `var/jb/` (`DEBIAN/` stays at
the top). Already-rootless payloads are rebuilt unchanged (upstream's
"skipping and exiting cleanly").
2. **Control** — `Architecture → iphoneos-arm64`, `Depends` gains
`cy+cpu.arm64v8 | oldabi-xina | oldabi`, an `Icon` path is converted.
Upstream's Depends append is buggy (crashes on string Depends); xkvm
appends correctly in all cases.
3. **Mach-O** — signatures are removed (rootless installs re-sign), then
every load-command dylib path and the LC_ID_DYLIB whose first path
component is a bootstrap root (`Library`, `usr`, `var`, …) is rewritten
under `/var/jb` via the existing growing-rename machinery
(`ChangeDependency`/`SetInstallName`), honoring the ConversionRuleset
blacklist and special cases. `@rpath`/`@executable_path` deps are never
touched (their first component isn't a bootstrap root).

**Documented boundary (deviation):** upstream's string scanner rewrites
`__TEXT.__cstring` + `__DATA.__data` strings with pointer patching
(CFStrings, ADRP/ADD, chained-fixup aware) — Apple's `/usr/lib` libraries
never appear there, so its blacklist omits them. xkvm converts the
**load-command layer only**, where `libSystem.B.dylib` etc. DO appear, so the
blacklist adds the Apple `/usr/lib` families (`libSystem`, `libobjc`,
`libc++`, `libz`, `libsqlite3`, …) — a corrupted libc dependency would break
the whole tweak, while a not-converted jailbreak lib merely keeps a rootful
path (`xkvm check` can't yet see it — same as before conversion). Runtime
dlopen strings compiled into `__TEXT` are out of scope; `--thin` thins every
Mach-O to arm64 best-effort.

**Tests** (all native-gated where they build Mach-Os): deb Build/Unpack
round-trip incl. symlink-in-tar; ShouldConvert/ConvertString fidelity tables
pinned against the upstream ruleset; `TestConvertEndToEnd` converts a real
dylib with substrate-style weak deps and asserts converted + untouched load
commands; app-level debify→undeb round trip with manifest; rootless-on-
debify-output with `var/jb` layout + control assertions.

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
  └─ inject dylib/framework → Frameworks (unless marked `--root-dylib` **or recorded as app-root in an extraction manifest** → app root + `@executable_path`), appex → PlugIns, other → app root
parse .cyan config(s) → merge into args (inject/ + root_dylibs payloads, icon/plist/ents, scalar overrides)
extract command (optional): dump artifacts + write xkvm-manifest.json (placement memory — §5.2)
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
- **Placement memory:** manifest round-trip + placement/kind mapping
  (`internal/artifact/manifest_test.go`); extract writes correct placements
  (`TestExtractArtifactsWritesManifest`); auto-detect honors root dylibs from a manifest and
  ignores frameworks/absent/corrupt manifests (`TestRootDylibsFromManifests`); the full
  extract → re-inject round-trip restores `@executable_path` with no flag
  (`TestExtractReinjectHonorsPlacement`, native-toolchain-gated).
- **E2E golden tests:** small committed fixture IPA → run pipeline → assert (a) zip structure,
  (b) injected load command present, (c) Info.plist keys, (d) code signature parses.
- **cyan-check golden (macos-14/arm64 leg):** `TestCyanCheckGoldenExtractedSet` builds real
  Mach-O fixtures carrying a root dylib + a Frameworks dylib + a bundle, runs the production
  `ExtractArtifacts`, generates a `.cyan` from the extracted dylibs (`cyanfile.Generate`), and
  `Validate`s it with the real patch registry — zero error-level issues; the golden negative
  rewrites `root_dylibs` in the same archive and asserts exactly one error.
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
