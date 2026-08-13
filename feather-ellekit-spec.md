# xkvm — Feather Reference & ElleKit Injection Spec

**Status:** Approved (interviewed) — implementation not started
**Date:** 2026-08-13
**Request short name:** feather-ellekit
**Related docs:** [ARCHITECTURE.md](ARCHITECTURE.md) (milestones M0–M6, module layout, CLI table), [README.md](README.md)

---

## 1. Background (grounded facts)

### 1.1 What xkvm does today

xkvm is a from-scratch Go rewrite merging **pyzule-rw / cyan** (tweak injector) with
**Azule** (repo fetch / decrypt). Current injection pipeline (`internal/inject/inject.go`):

- Expands `.deb` payloads via `internal/deb`, collecting dylib/appex/framework/bundle artifacts.
- Per-type placement: dylib/framework → `Frameworks/`, appex → `PlugIns/`, other → app root.
- **Common-dependency fixing** (`fixCommonDeps`): rewrites dependency name *fragments* to
  canonical install paths via the `commonDeps` map:
  | Fragment | Canonical (default mode) |
  |---|---|
  | `substrate.` | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` |
  | `orion.` | `@rpath/Orion.framework/Orion` |
  | `cepheiprefs.` / `cepheiui.` / `cephei.` | `@rpath/Cephei*.framework/Cephei*` |
- **Auto-injection** (`autoInject`): copies missing hooking frameworks from `go:embed`ed
  `extras/` into `Frameworks/`.
- Adds `@executable_path/Frameworks` rpath, weak `LC_LOAD_DYLIB` loads on the main binary,
  entitlement preservation + pure-Go ad-hoc re-sign (`internal/macho`, M3 — go-macho +
  `pkg/codesign`).

**Key nuance (from `ARCHITECTURE.md` §2.1):** the embedded `CydiaSubstrate.framework` is
already an **ElleKit-backed** build carrying the substrate name (that is cyan's approach —
"substrate→ElleKit `@rpath/CydiaSubstrate.framework/CydiaSubstrate`"). So today, substrate
tweaks *effectively* run on an ElleKit runtime, but it is exposed under the substrate name and
there is no explicit "ElleKit" story.

### 1.2 What Feather does differently

Feather (`claration/Feather`) is an on-device iOS app manager / sideloader (Swift, GPL-3.0,
Zsign-based signing). Relevant capabilities vs. xkvm:

- **Explicit "Replace Substrate with ElleKit"** toggle — swaps legacy CydiaSubstrate /
  Substitute runtime for the modern ElleKit runtime so old tweaks don't crash on newer iOS.
- **Generic togglable compatibility patches** (e.g. Liquid Glass for iOS 26-era apps).
- AltStore-compatible repo import, on-device signing with real certificates, files-app
  documents toggle, app rename/icon — most already exist in xkvm or are documented-only ideas.

### 1.3 ElleKit facts (for embedding)

- Project: `evelyneee/ElleKit` (a.k.a. `tealbathingsuit/ellekit`). **License: BSD-3-Clause**
  — permissive, safe to embed with attribution.
- Runtime for Dopamine-era jailbreaks and modern sideloaded tweak injection; drop-in
  substrate API compat (`MSHookFunction` etc.) plus libhooker compat.
- **arm64 / arm64e only** (no armv7). Typical framework layout for sideloads:
  `Frameworks/ElleKit.framework/ElleKit`.
- Sideloading tools conventionally bundle `Frameworks/ElleKit.framework/ElleKit` and rewrite
  tweak deps to `@rpath/ElleKit.framework/ElleKit`.
- Binary footprint is small (~50–150 KB).

---

## 2. Goals & non-goals

### Goals
1. Add an **explicit ElleKit injection mode** (`--ellekit`) that embeds a real, freshly-built
   `ElleKit.framework` and rewrites all legacy hooking-dependency spellings to it.
2. Keep default (substrate) mode **byte-identical to today**.
3. Port **Feather's generic patch framework** and land **Liquid Glass** as the first patch.
4. Ship **NOTICE + reproducible build recipe** for the embedded ElleKit artifact.

### Non-goals (documented ideas only — see §8)
- Real certificate signing (.p12 + provisioning profile) — Feather/Zsign-style. *Future idea.*
- AltStore repo import/browse. *Future idea (overlaps planned M4 Azule fetch).*
- Any on-device (iOS) behavior — xkvm remains a host tool.

---

## 3. Decisions (from user interview, 4 rounds)

### D1 — ElleKit semantics: **real ElleKit.framework, runtimes mutually exclusive**
- Embed the **real ElleKit binary** (not just the substrate-named shim).
- **Runtime choice is per-run and mutually exclusive:** a run uses *either* the substrate
  runtime *or* the ElleKit runtime — never both in one output app.
  - Default mode → deps rewrite to `@rpath/CydiaSubstrate.framework/CydiaSubstrate`
    (unchanged from today).
  - `--ellekit` mode → deps rewrite to `@rpath/ElleKit.framework/ElleKit`.
- The two frameworks coexist *inside the xkvm binary's `extras/`* (both assets embedded);
  only one is materialized into a given output app, per mode.

### D2 — Trigger: **opt-in `--ellekit` flag**
- New bool flag `--ellekit`. Default run behaves exactly as today.

### D3 — Scope: **port multiple Feather features**
- In addition to ElleKit: **generic patch framework** + **Liquid Glass patch**.

### D4 — Shim design (delegated, user answer): **mode-driven, not dual-materialized**
- User: *"if it uses ellekit then ellekit, if uses substrate then substrate, both cannot be
  combined."* → Output apps get exactly one hooking framework per run (D1). No symlink/hardlink
  dual-binary tricks in the output.

### D5 — Legacy spelling coverage: **all four**
Under `--ellekit` (and for auto-switch, see D6), rewrite all of:
1. `/Library/MobileSubstrate/MobileSubstrate.dylib`
2. `/usr/lib/substrate/libsubstrate.dylib`, `libsubstrate.dylib`,
   `@executable_path/libsubstrate.dylib`
3. `CydiaSubstrate.framework/...` forms
4. `libhooker.dylib` (Dopamine-era)

### D6 — libhooker in default mode: **auto-switch to ElleKit** (delegated decision)
User: *"either auto switch on depends or build cool automation, choose the best for me."*
**Decision:** when any tweak in the injection set (post `.deb` expansion) has a dependency
containing `libhooker`, xkvm **automatically selects ElleKit mode** for that run even without
`--ellekit`, logging a clear notice ("tweak requires libhooker → using ElleKit runtime").
Rationale: libhooker is the Dopamine/ElleKit-native runtime; the embedded default substrate
is itself ElleKit-backed, so this auto-switch has minimal blast radius and gives libhooker
tweaks the runtime they were compiled against. Explicit `--ellekit` always wins and is
idempotent with the auto-switch.

### D7 — ElleKit binary provenance: **build from source, vendor artifact**
- Build `evelyneee/ElleKit` from source during development; commit the produced framework
  into `internal/extras/extras/`.
- Exact build command must be verified against the ElleKit README/Makefile at implementation
  time (it is a Makefile/Xcode-based build producing `libellekit.dylib` + framework layout).
  Record the exact steps in `docs/ellekit-build.md` (§7).

### D8 — Architecture: **embed fat, thin on injection**
- Vendor **fat arm64 + arm64e** ElleKit.
- During `--ellekit` injection, **thin to the main executable's architecture** (see D9).

### D9 — arm64e apps: **match the app's arch**
- Parse the main executable's CPU slice (go-macho) and thin ElleKit to match: arm64 app →
  arm64 slice; arm64e app → arm64e slice. This is the correct-by-construction option chosen
  by the user over "always arm64".

### D10 — Patch framework UX: **named flags per patch**
- Each patch gets its own bool flag (e.g. `--liquid-glass`), backed by a shared internal
  patch **registry + apply pipeline**. No new positional/config-file surface.
- Naming must not collide with the existing `--patch` (sideload-fix dylibs) flag — long,
  patch-specific flags avoid ambiguity.

### D11 — Default mode: **untouched**
- Substrate default mode keeps the current embedded `CydiaSubstrate.framework` byte-identical
  and all current rewrite targets unchanged. `--ellekit` is the only switch.

### D12 — Validation: **unit tests + e2e-real.sh stage**
- Table-driven unit tests for every legacy-spelling → canonical-path rewrite in both modes.
- A new `[8/8]` e2e-real.sh stage: real iOS app IPA + a fixture tweak linked against each
  legacy spelling, `--ellekit` run, asserting (a) rewrite targets in `otool -L`,
  (b) `Frameworks/ElleKit.framework/ElleKit` present (fat → thinned to app arch),
  (c) `codesign --verify` exit 0 on main + dylib + ElleKit framework, (d) no
  `CydiaSubstrate.framework` in the output.
- A manual real-device checklist is out of scope for automation (no device in env) but should
  be noted in the e2e script's header comment for the user's later use.

### D13 — Licensing: **full NOTICE + build recipe doc**
- Update `internal/extras/extras.go` provenance comments.
- Add a **NOTICE** entry: ElleKit (BSD-3-Clause, source-build recipe → `docs/ellekit-build.md`)
  and Feather (GPL-3.0, reference-only — no code copied).
- Write `docs/ellekit-build.md` with the exact reproducible build-from-source steps.

---

## 4. Design

### 4.1 Runtime modes

```
                 substrate mode (default)              ellekit mode (--ellekit / auto)
-----------------------------------------------------------------------------------------------
runtime asset    CydiaSubstrate.framework (existing)   ElleKit.framework (new, fat→thinned)
rewrite targets  @rpath/CydiaSubstrate.framework/…     @rpath/ElleKit.framework/ElleKit
covered spell    substrate. fragment only (+orion/     MobileSubstrate.dylib, libsubstrate.dylib,
                 cephei* as today)                     CydiaSubstrate.framework, libhooker.dylib
auto-inject      CydiaSubstrate.framework              ElleKit.framework (thinned to app arch)
```

### 4.2 Dependency-rewrite mapping (new `commonDeps` shape)

Refactor `internal/inject/inject.go` from a single `commonDeps` map to a **mode-keyed**
structure:

```go
type runtimeMode int
const (
    modeSubstrate runtimeMode = iota // default
    modeElleKit
)

// legacySpellings: matched by substring, longest-first (reuse commonKeys() pattern).
// Each maps to a per-mode canonical install path.
```

| Legacy spelling (substring) | substrate mode → | ellekit mode → |
|---|---|---|
| `substrate.` | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` | `@rpath/ElleKit.framework/ElleKit` |
| `mobilesubstrate` | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` | `@rpath/ElleKit.framework/ElleKit` |
| `libsubstrate` | `@rpath/CydiaSubstrate.framework/CydiaSubstrate` | `@rpath/ElleKit.framework/ElleKit` |
| `libhooker` | auto-switch to ellekit mode (D6) | `@rpath/ElleKit.framework/ElleKit` |
| `cydiasubstrate` | (already canonical) | `@rpath/ElleKit.framework/ElleKit` |
| `orion.` | `@rpath/Orion.framework/Orion` | `@rpath/Orion.framework/Orion` (unchanged) |
| `cephei*.` | `@rpath/Cephei*.framework/Cephei*` | unchanged |

Notes:
- `fixCommonDeps` gains the mode; `needed` tracking drives `autoInject` per mode.
- In substrate mode, `libsubstrate`/`mobilesubstrate` spellings were previously *unmatched*
  (only the `substrate.` fragment hit). **This is a deliberate broadening** — substrate tweaks
  linked against the classic absolute paths should now resolve. Mark as a behavior change with
  regression coverage (D12).
- **orion/cephei* are mode-independent** — they stay substrate-named in both modes (they are
  distinct frameworks, not substrate aliases).

### 4.3 Auto-switch (D6)

- After `.deb` expansion, scan each tweak's dependencies (`macho.Bin.Dependencies()`).
- If any dependency contains `libhooker` and the user did **not** pass `--ellekit`:
  switch mode to `modeElleKit`, log `[!] tweak <name> requires libhooker → using ElleKit runtime`.
- If user passed `--ellekit`, mode is already ellekit — no extra log needed beyond the flag.

### 4.4 ElleKit asset lifecycle

1. **Build** (`docs/ellekit-build.md`): clone `evelyneee/ElleKit`, run the documented build,
   produce `ElleKit.framework` (fat arm64+arm64e, BSD-3 attribution header in `NOTICE`).
2. **Vendor**: `internal/extras/extras/ElleKit.framework/` (binary + Info.plist).
   `extras.CopyFramework("ElleKit.framework", …)` works unchanged via `go:embed all:extras`.
3. **Thin-on-inject**: new helper in `internal/macho` (or reuse M3 thin path) — parse main
   executable arch via go-macho `CPU`; slice ElleKit to the matching arch before copying into
   `Frameworks/`. arm64 → arm64 slice; arm64e → arm64e slice. (D8, D9)
4. **Sign**: ElleKit framework binary is code-signed with the rest of the bundle during
   fakesign (existing path).

### 4.5 Generic patch framework (D10)

New package `internal/patch`:

```go
package patch

type Patch interface {
    Name() string            // "liquid-glass"
    FlagName() string        // "liquid-glass"
    Apply(appDir string, opts *PatchContext) error
}

// Registry: named patches → constructors.
func Register(p Patch)
func Apply(appDir string, enabled []string) error
```

- `cli.go` binds one bool flag per registered patch; enabled patch names are collected into
  `Options.Patches []string`.
- `run.go` invokes `patch.Apply` at a defined pipeline point: **after plist ops, before
  fakesign** (gated on the patched app being a `.app` dir). **Placement rationale:** the
  Liquid Glass Mach-O edit mutates the main binary, so it must precede signing; plist-ordering
  is irrelevant because `plist.Dict` is a `map[string]any` (read-modify-write preserves all
  unknown keys).

### 4.6 First patch: Liquid Glass (patch body — VERIFIED 2026-08-13 from Feather source)

Feather's patch is **two independent toggles** (`OptionsManager.swift`
`experiment_supportLiquidGlass` / `experiment_disableLiquidGlass`), each a tiny modification
of the target app:

| Flag (xkvm) | Feather toggle | Body |
|---|---|---|
| `--liquid-glass` | supportLiquidGlass ("Modifies app to support liquid glass") | ① Info.plist `UIDesignRequiresCompatibility = false` ② main executable: `LC_BUILD_VERSION.sdk` → `0x1A0000` (26.0.0), **every slice** (thin or fat) |
| `--liquid-glass-compat` | disableLiquidGlass ("Modifies app to disable liquid glass") | ① Info.plist `UIDesignRequiresCompatibility = true` ② no Mach-O change |

**Mechanics (why it works):** iOS 26 applies the Liquid Glass design to apps whose recorded
SDK ≥ 26; older-SDK apps render in the legacy compatibility appearance. The system-level
opt-out key `UIDesignRequiresCompatibility = true` forces legacy rendering even for new-SDK
apps. Feather's enable path does **both** the plist key and the SDK bump; the disable path
only sets the key.

**Implementation notes (source-grounded):**

- Info.plist step: `plist.Open` → set the bool → `plist.Write` (binary plist; all other keys
  preserved by the map-based Dict).
- Mach-O step: new `internal/macho` helper `BumpSDK26(path)` — pure-Go via go-macho: open the
  binary, iterate slices (fat `FAT_MAGIC`/`FAT_CIGAM` or thin `MH_MAGIC_64`), find the
  `LC_BUILD_VERSION` load command, set `sdk = 0x1A0000`, write back. Follow the existing
  strip-edit-resign pattern (RemoveSignature → edit → `SignWithEntitlements` if entitlements
  exist), matching injection's approach; if `LC_BUILD_VERSION` is absent, no-op (log).
- Feather applies the SDK bump **only to the main executable** (`Bundle.executableURL`), never
  to dylibs/frameworks — mirror that.
- **Attribution:** the SDK-26 patch logic is derived from a LiveContainer commit (Apache-2.0,
  credited in `MachOUtils.m`); keep the attribution in a code comment.
- Feather source refs for implementation: `SigningHandler.swift:91-92, 204-205, 398-402`;
  `MachOUtils.m:81-128` (`SDK_VERSION_26_0_0 0x1A0000`); `OptionsManager.swift:108-111`.

**Related (not in scope, noted):** Feather always runs `LCPatchMachOFixupARM64eSlice` on every
`.dylib`/`.framework` (adds `CPU_SUBTYPE_LIB64` to arm64e slices) for iOS 26 loadability. Our
Xcode-built `ElleKit.framework` arm64e slice **already carries `CPU_SUBTYPE_LIB64`** (verified
`otool -h`: caps `0x80`), so it needs no fixup. Consider a future opt-in fixup for user-supplied
dylibs if real-world iOS 26 failures surface.

### 4.7 CLI surface changes

| Flag | Meaning | Status |
|---|---|---|
| `--ellekit` | use the real ElleKit runtime; rewrite all legacy hooking spellings to `@rpath/ElleKit.framework/ElleKit`; implies fat→app-arch thinning | new |
| `--liquid-glass` | apply the Liquid Glass compat patch | new (patch #1) |
| (future) `--<patch-name>` | each registered patch gets its own flag | extensible |

No collisions: `--patch` (sideload fixes) is untouched; patch flags are long and specific.

---

## 5. File-by-file change plan

| File | Change |
|---|---|
| `internal/inject/inject.go` | mode enum; `commonDeps` → mode-keyed map incl. new spellings; `fixCommonDeps`/`autoInject` take mode; auto-switch scan (D6) |
| `internal/inject/inject_test.go` | extend: per-mode rewrite table tests, libhooker auto-switch test, "already canonical" regressions |
| `internal/extras/extras/ElleKit.framework/…` | vendored fat ElleKit binary + Info.plist (D7) |
| `internal/extras/extras.go` | provenance comment for ElleKit; keep `CopyFramework` generic |
| `internal/macho/` | slice-thinning helper: main-exe arch → thin framework (D8/D9); test |
| `internal/macho/sdk26.go` + `_test.go` | `BumpSDK26` helper (§4.6): LC_BUILD_VERSION.sdk → 0x1A0000 on thin/fat, strip-edit-resign, no-op when absent |
| `internal/patch/patch.go` + `_test.go` | registry, flag binding helper, apply pipeline, **liquid-glass + liquid-glass-compat patches (§4.6)** |
| `internal/app/options.go` | `ElleKit bool`, `Patches []string` |
| `internal/app/run.go` | mode resolution (flag/auto-switch); `patch.Apply` hook; pass mode through injection |
| `internal/cli/cli.go` | `--ellekit` + per-patch flags; help text |
| `scripts/e2e-real.sh` | new `[8/8]` stage (D12); header note re: manual device checklist |
| `NOTICE` | ElleKit BSD-3 + build recipe ref; Feather GPL-3.0 reference-only (D13) |
| `docs/ellekit-build.md` | reproducible build-from-source steps (D13) |
| `ARCHITECTURE.md` / `README.md` | milestone table + CLI table + status updates |

---

## 6. Test plan

1. **Unit — rewrite tables:** for each legacy spelling × each mode, assert the exact rewritten
   `LC_LOAD_DYLIB` path; assert "already canonical" entries are left alone (no-op) — the cyan
   v1.4/v1.4.1 regression class.
2. **Unit — auto-switch:** tweak dep `libhooker.dylib` → mode flips to ellekit, notice logged;
   explicit `--ellekit` with a substrate-only tweak → ellekit mode, no auto-log.
3. **Unit — thinning:** fat ElleKit fixture → thinned to arm64 for an arm64 main exe; arm64e
   slice selected for arm64e main exe.
4. **Unit — patch registry:** `--liquid-glass` enabled → `patch.Apply` called with correct name;
   unknown patch name rejected at flag-parse time.
5. **Unit — Liquid Glass body (§4.6):** (a) `--liquid-glass` sets `UIDesignRequiresCompatibility`
   to `false` and `--liquid-glass-compat` to `true` in Info.plist, other keys untouched;
   (b) `BumpSDK26` sets `sdk == 0x1A0000` on a fixture thin arm64 Mach-O and on every slice of
   a fat fixture; (c) no-op (no error) when `LC_BUILD_VERSION` is absent; (d) signature
   re-validates after the edit.
6. **E2E:** extend `[8/8]` (or a `[9/9]`) stage — real app + `--liquid-glass` run → assert
   `plutil -p Info.plist` shows the key, `otool -l` on the main exe shows `sdk 26.0`, and
   `codesign --verify` exits 0 (patch ran before fakesign).
5. **E2E `[8/8]`:** real IPA + fixture tweaks linked against MobileSubstrate.dylib,
   libsubstrate.dylib, CydiaSubstrate.framework, libhooker.dylib → `--ellekit` run →
   assert rewrite targets (`otool -L`), `Frameworks/ElleKit.framework/ElleKit` present and
   thinned, **no** CydiaSubstrate.framework in output, `codesign --verify` exit 0 on main +
   dylib + framework. Also run default mode asserting zero behavior change.
6. **Gate:** `gofmt -l .`, `go vet ./...`, `go test ./...`, full `e2e-real.sh` green.

---

## 7. Deliverables & ordering

1. `docs/ellekit-build.md` + build the ElleKit artifact (verify exact build command from repo).
   **✅ DONE 2026-08-13** — recipe written from a verified end-to-end build; open question #1
   resolved (§9).
2. Vendored `ElleKit.framework` + NOTICE + extras.go provenance.
   **✅ DONE 2026-08-13** — fat arm64+arm64e v1.1.3 at `internal/extras/extras/ElleKit.framework/`
   (verified: lipo, install name `@rpath/ElleKit.framework/ElleKit`, ldid-signed); repo-level
   `NOTICE` written; `extras.go` package comment updated; embed visibility + full suite green.
3. Mode-keyed dependency rewrite + auto-switch + unit tables (substrate default proven
   byte-identical). **✅ DONE 2026-08-13** — mode-keyed `commonDeps` (4 legacy spellings ×
   2 modes), libhooker **pre-scan** auto-switch (order-independent; mixed-set regression
   test), thin-on-inject, all inject tests green. Also fixed a latent `serializeTOC` bug
   (growing renames left stale `sizeofcmds` → re-parse failure; regression test added).
4. Slice-thinning helper + tests. **✅ DONE 2026-08-13** — `ThinToArch` + `BumpSDK26` +
   `SDKVersion` in `internal/macho`, thin/fat tests green.
5. `internal/patch` registry + `--liquid-glass`. **✅ DONE 2026-08-13** — registry +
   `liquid-glass`/`liquid-glass-compat` patches (§4.6 body: plist key + `LC_BUILD_VERSION.sdk`
   → 0x1A0000, always re-signs after edit, mutual-exclusion validated).
6. `e2e-real.sh [8/10]` + `[9/10]` + `[10/10]` + full validation. **✅ DONE 2026-08-13** —
   ElleKit, Liquid Glass, and force-fullscreen stages pass against a real iOS app +
   real tweak; full suite green (11/11 pkgs), gofmt/vet clean, e2e exit 0.
7. Docs (ARCHITECTURE/README). **✅ DONE 2026-08-13** — ARCHITECTURE.md CLI table + FE milestone
   row updated. README flag list is kept minimal by design (points to ARCHITECTURE.md).

---

## 8. Documented future ideas (not in scope)

- **Real certificate signing** (`.p12` + provisioning profile) as an alternative to ad-hoc
  fakesign — Feather/Zsign-style. Design note: would slot alongside `Fakesign` in
  `internal/macho`, keyed by cert path; needs `openssl`-free PKCS#12 parsing research.
- **AltStore-compatible repo import** — overlaps planned M4 Canister/MobileAPT fetch;
  re-evaluate after M4.
- **Substitute-specific detection** (beyond the spellings above) — add only if a real-world
  tweak is found needing it.

---

## 9. Open questions (to resolve at implementation)

1. **ElleKit build command** — **RESOLVED 2026-08-13** (verified end-to-end on this machine):
   Release configuration + `xcodebuild -scheme ellekit -sdk iphoneos ARCHS="arm64 arm64e"
   ONLY_ACTIVE_ARCH=NO` (the Makefile's `-destination` form fails with *"iOS 17.0 is not
   installed"* even when the SDK exists), then hand-assemble the framework (rename
   `libellekit.dylib` → `ElleKit.framework/ElleKit`, Info.plist template, `install_name_tool
   -id @rpath/ElleKit.framework/ElleKit`, `ldid -S`). Full recipe: `docs/ellekit-build.md`.
   The fat arm64+arm64e output, install-name rewrite, ldid signing, and thin-slice survival
   were all verified.
2. **arm64e slice validity** — confirm the fat build actually contains arm64e (not arm64 only
   with a compat flag); if arm64e slice is unavailable, document and fall back to arm64.
3. **Liquid Glass payload** — **RESOLVED 2026-08-13** (verified from Feather source, see §4.6):
   Info.plist key `UIDesignRequiresCompatibility` (true=compat / false=liquid glass) +
   `LC_BUILD_VERSION.sdk → 0x1A0000` on the main executable for the enable path. No asset or
   dylib involvement. Fully specified in §4.6 with source references.
4. **`libsubstrate`/`mobilesubstrate` broadening in default mode** — confirm this is desired
   as an explicit behavior change (it is recommended; regression coverage mandated).
5. **`Info.plist` inside vendored `ElleKit.framework`** — must match the framework layout
   conventions xkvm already handles for `CydiaSubstrate.framework` (CFBundleExecutable etc.).

---

## 10. Risk notes

- **Default-mode regression risk:** the new spellings (`mobilesubstrate`, `libsubstrate`)
  broaden default-mode rewrites — mitigated by unit tables + e2e asserting default output is
  unchanged for existing inputs.
- **Fat binary round-trip:** embedding fat and thinning on injection touches the M3 native
  signer's fat handling — mitigated by slicing *before* signing (thin → then normal sign path,
  which is already proven for arm64).
- **License:** ElleKit BSD-3-Clause is embedding-safe; Feather GPL-3.0 must remain
  reference-only (no code copied) — enforced by NOTICE + build-recipe doc.
