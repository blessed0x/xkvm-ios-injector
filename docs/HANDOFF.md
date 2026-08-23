# xkvm — Handoff Document

**Purpose of this file:** bring a new agent/session up to speed on the xkvm
project without re-reading the whole tree. Read this first, then follow the
links. Last updated: 2026-08-23.

---

## 1. What xkvm is (one paragraph)

xkvm is a **command-line tool for sideloading iOS apps and tweaking them with
jailbreak tweaks**, written entirely in Go. Give it an app (`.ipa`, `.tipa`,
`.app`) and a tweak (`.dylib`, `.deb`, framework, bundle, or a repo package
id), and it handles the error-prone parts: wiring the tweak into the Mach-O
binary with correct load commands, re-signing everything so the app still
installs, and repacking the container. It also **extracts** tweaks back out of
modified apps (remembering where each one lived so re-injection is automatic),
**converts** jailbreak packages between the rootful / rootless / roothide
conventions (and `.deb` ↔ `.dylib`), **fetches** tweaks by bundle id from Cydia
repos (Canister / MobileAPT) with dependency recursion, **checks** an app for
missing files that would crash it at launch (and can auto-fix them), and
**decrypts** App Store downloads by Apple ID (the ipatool/PancakeStore flow).

It is a from-scratch Go rewrite that merges **pyzule-rw / cyan** (the
actively-maintained Python tweak injector — its author famously asked for a
rewrite in a compiled language) with the missing features of the **archived
Azule** tool (repo-based fetching, App Store decrypt). The repo is
`github.com/xscope0/xkvm-ios-injector`; the binary is `xkvm`.

**Two surfaces, one engine.** The `xkvm` root command is the flag-driven CLI
(for scripted/batch use and AI agents). `xkvm tui` (also bare `xkvm` when no
flags are given) is a dependency-free interactive menu that asks the same
questions in plain words and drives the **exact same** `internal/app` entry
points — no separate code path, so the TUI can't drift from the CLI.

## 2. Quick facts

| Fact | Value |
|---|---|
| Module path | `github.com/xscope0/xkvm-ios-injector` |
| Language / Go version | Go 1.26.6 (go.mod; staticcheck + govulncheck pinned via the tool directive) |
| Binary | `xkvm` (single static binary; builds darwin/linux/windows × arm64/amd64) |
| CLI framework | `spf13/cobra` |
| Mach-O | `github.com/blacktop/go-macho` v1.1.282 + `pkg/codesign` (pure Go, no ldid/insert_dylib/otool) |
| Plist | `howett.net/plist` |
| Compressors | stdlib zip/tar/gzip + `klauspost/compress` (zstd), `ulikunitz/xz`, `dsnet/compress` (bzip2) |
| Icon images | stdlib `image/png` + `golang.org/x/image` |
| License | MIT (`LICENSE`); third-party embed provenance in `NOTICE` |
| Primary platform | macOS (heavy lifting is pure Go; macOS-14 CI leg runs the native-toolchain tests) |
| CI | GitHub Actions: lint job (`make lint`) + test matrix (macOS 14 arm64, Ubuntu x64, Windows x64) |
| Gate | `make qa` = `make lint` + `go test -race ./...` + 6-combo cross-compile |

**Current HEAD:** `5e9f4d1` — "xkvm: add goreleaser release workflow on tags"
(pushed; tree clean).

## 3. Command surface

Root: `xkvm [flags] -i <app>` (inject). Every other verb is a subcommand.

| Command | What it does |
|---|---|
| `xkvm -i App.ipa -o Out.ipa -f Tweak.dylib ...` | **Inject** tweaks into an app (dylibs, debs, frameworks, bundles, `.cyan` configs) and re-sign |
| `xkvm tui` (or bare `xkvm`) | **Menu mode** — asks questions in plain words |
| `xkvm extract -i App.ipa -o dir/` | Pull tweaks out of an app; writes `xkvm-manifest.json` recording original placement |
| `xkvm check -i App.ipa [--fix -o Out.ipa]` | Merge-completeness check: every bundle-relative dep must resolve; `--fix` auto-repairs from a fix-dir / deb / cache / online repos |
| `xkvm rootless -i rootful.deb -o rootless.deb [--xina] [--tweakinject] [--thin]` | rootful → rootless conversion (standard, Xina-style, or Derootifier TweakInject-style) |
| `xkvm rootful -i rootless.deb -o rootful.deb` | rootless → rootful (inverse of the above) |
| `xkvm roothide -i rootless.deb -o roothide.deb [--pkgmirror] [--mode auto\|dynamic]` | rootless → roothide (RootHidePatcher semantics) |
| `xkvm debify -i Tweak.dylib -o Tweak.deb` | Build a MobileSubstrate `.deb` from a dylib or payload dir |
| `xkvm undeb -i Tweak.deb -o dir/` | Unpack a `.deb` into its tweak artifacts (with placement manifest) |
| `xkvm cgen -o out.cyan [-f tweak ...]` | Generate a shareable `.cyan` config from flags |
| `xkvm cyan-check <file.cyan>...` | Validate `.cyan` configs without applying (exit 1 on errors) |
| `xkvm fetch`-equivalent | **Folded into inject**: `-f <repo-package-id> --fetch` resolves through Canister/MobileAPT (M4) |
| `xkvm cache [--clear]` | Show or empty the persistent fetch cache (`~/Library/Caches/xkvm/fetch`, 7-day TTL) |
| `xkvm decrypt <app-id\|app-store-url\|bundle-id>` | Download an App Store app by Apple ID (bag → auth → buy → sinfs + metadata) |
| `xkvm device <op>` | Control a connected iPhone/iPad via go-ios: list/pair/info/battery/apps/install/uninstall/launch/kill/syslog/restart/shutdown/watch/screenshot/devmode/forward/pasteboard/omega. iOS 17+ tunnel-gated ops auto-start a userspace developer tunnel (XKVM_NO_AUTO_TUNNEL=1 declines); syslog filters with --process/--contains (`--udid`, `--json`; policy + errors in docs/device-control.md) |

### Key root flags (inject)

`-i/--input`, `-o/--output`, `-f/--file` (repeatable), `-z/--cyan` (repeatable
configs), `--fetch <bundle-id>`, `-A/--apt-source` (extra repos),
`--no-recurse`, `-s/--fakesign`, `--patch` (inject bundled sideload-repair
dylib set; implies fakesign), `--ellekit` (real ElleKit runtime + framework),
`--root-dylib` (place dylib at app root with `@executable_path` instead of
Frameworks/`@rpath`), `--liquid-glass` / `--liquid-glass-compat` /
`--force-fullscreen` (compatibility patches), `-n -v -b -m` (name/version/bundle
id/minOS), `-k/--icon`, `-l` (plist merge), `-x` (entitlements), `-u -w -d`
(uisd/no-watch/documents), `-q` (thin), `-e -g` (extensions), `-c 0..9`
(compress level), `--ignore-encrypted`, `--overwrite`.

### TUI menu map

```
 apps    inject · check · decrypt
 tweaks  extract · fetch · build · cache
 convert convert (rootful/rootless/roothide/Xina)
 info    help · about
```

Two-level category browser: arrow keys move (↑/↓ or j/k), 1-9 jumps, enter
picks, q backs out. Every option explains itself in a "why this one" panel
while highlighted (what it does + a short example), and every CLI flag is
reachable — the inject flow exposes the full root-flag surface, fetch has a
per-tweak version picker, debify/cgen expose their extras, convert asks
thin/tweakinject/pkgmirror/mode. Each flow is a `flowXxx` method in
`internal/tui/tui.go` driving the same `internal/app` functions the CLI
uses. When stdin is piped (CI, tests), colors/animation/raw mode turn off
and the pickers read a line protocol: numbers or names for lists,
space-separated `N`/`!N` tokens for multi-selects, blank uses the defaults,
`q` cancels.

## 4. Architecture map (package by package)

```
cmd/xkvm/main.go            cobra root; delegates to internal/cli
internal/cli/               cobra commands + all flag binding (sorted patch
                            registration), completion, exit codes
internal/app/               pipeline orchestrator: Run (inject), ExtractArtifacts,
                            CheckAndFix, decrypt orchestration, package cmds
internal/tui/               dependency-free menu (ANSI, pipable), injectable
                            Run for tests; log capture + "what happened" panel
internal/ipa/               zip extract/repack, compression levels, hidden-entry
                            exclusion, zip-slip-safe extraction
internal/deb/               ar parser (hand-rolled ~60 lines) + data.tar.{gz,xz,
                            zst,bz2,lzma} extraction; Build/Unpack (both tars)
internal/plist/             howett wrappers: name/version/bundleid/minOS/uisd/
                            documents/merge; xml1 conversion port (plutil)
internal/macho/             the binary seam — PURE GO (M3), no embedded tools:
  bin.go                      Bin surface (RemoveSignature, Fakesign, deps, ...)
  native.go                   go-macho/pkg/codesign ops (≈ insert_dylib/ldid/
                              otool/lipo); growing-rename (ChangeDependency/
                              SetInstallName); serializeTOC rpath self-align fix
  der.go                      entitlements DER encoder for ad-hoc signing
  cstring.go                  __TEXT.__cstring dlopen-string rewrite (in-place +
                              growing via new __PATCH_ROOTLESS segment)
  sdk26.go                    LC_BUILD_VERSION.sdk → 26.0 (Liquid Glass patch)
internal/inject/            per-app injection: placement (Frameworks/root/PlugIns),
                            @rpath vs @executable_path contracts, dep fixing,
                            root-dylib support, placement-memory restore
internal/rootless/          package converters (all upstream-faithful):
  rootless.go                 rootful→rootless (rootless-patcher semantics)
  rootful.go                  rootless→rootful (inverse)
  roothide.go                 rootless→roothide (RootHidePatcher semantics,
                              incl. pkgmirror + AutoPatches .roothidepatch)
  xina.go                     Xina-style rootless (byte-level NUL seds)
  script_rootless.go          token-based DEBIAN-script path conversion
  sandy_golden_test.go        libSandy plist rewrite pinned on real fixtures
internal/patch/             compatibility-patch registry (feather-style):
                            force-fullscreen, liquid-glass, liquid-glass-compat;
                            Register() in init() + sorted flag binding
internal/fetch/             Canister v4 client + MobileAPT Packages parser +
                            dependency recursion + smart solver + persistent
                            versioned cache (7-day TTL) + default repo sweep
                            (repos.go, ~113 active repos)
internal/cyanfile/          .cyan zip config parse/generate/validate (upstream
                            shape + xkvm root_dylibs extension)
internal/decrypt/           App Store download flow (PancakeStore IPATool.swift
                            port): GUID, bag, authenticate (2FA detection,
                            pod-follow), buy, sinf+iTunesMetadata rewriting,
                            session persistence (0600), output-path prefs
internal/artifact/          shared collector + xkvm-manifest.json (placement
                            memory for extract→re-inject)
internal/appbundle/         app-bundle ops: extensions, icon (incl. CgBI PNG
                            decoder), check tiers, fakesign-all
internal/extras/            go:embed payload: hooking frameworks (ElleKit,
                            Cephei, CydiaSubstrate, Orion) + sideload-fix
                            dylibs (zxPluginsInject, Sideloadbypass*,
                            sideloadKeychainFix, ...)
internal/log/               [*]/[?]/[!]/[<] formatter, --silent, capture for TUI
internal/testutil/          fixtures + SkipUnlessNativeToolchain gate
```

## 5. Core pipeline (inject)

```
validate inputs
extract/copy app (.ipa → zip, .tipa, .app)
encryption check on main executable (--ignore-encrypted overrides)
parse .cyan config(s) → merge into args (validated first — apply-flow hook)
fetch stage: resolve -f entries that are repo bundle ids → debs (M4, recursive)
extract .debs (ar + data.tar.*) → collect dylib/framework/appex/bundle
fix common dependencies (substrate→ElleKit, orion, cephei*)
auto-inject missing hooking frameworks from extras/
inject: dylib/framework → Frameworks (@rpath), --root-dylib or manifest-root →
        app root (@executable_path), appex → PlugIns, other → app root
apply plist ops (name/version/bundleid/minOS/merge/uisd/documents)
apply compatibility patches (internal/patch registry — before fakesign)
change icon (120/152px + CFBundleIcons, CgBI decode)
mass fakesign all binaries (or thin to arm64)
repack .ipa (compression level, exclude hidden files) | emit .app
auto-warn on missing bundle-relative deps (the check tiers)
```

## 6. Fidelity & correctness conventions (important for any new work)

- **Pure-Go, native-only.** The M2 embedded toolchain was deleted. All Mach-O
  work goes through go-macho / pkg/codesign; macOS validates the signatures.
  The only `go:embed` left is the extras payload (content, not tooling).
- **Upstream-faithful converters.** rootless/roothide/xina are ports of the
  ecosystem's own tools (rootless-patcher, RootHidePatcher, Xinam1ne). Golden
  tests pin them **byte-for-byte against upstream output**; deliberate
  deviations are documented in ARCHITECTURE.md (e.g. Apple `/usr/lib` families
  blacklisted from rewriting; gzip instead of zstd for roothide output).
- **Placement memory.** `xkvm extract` writes `xkvm-manifest.json`; re-injecting
  those files honors it automatically (root dylibs get `@executable_path`
  without any flag). This is what makes extract→re-inject round-trips of
  real-world tweak sets (e.g. the Regram family) work.
- **Test style.** Every behavior change ships with a test. House style:
  golden/byte-identical tests vs upstream, hermetic httptest for network
  (Canister/MobileAPT/decrypt), `SkipUnlessNativeToolchain` for tests that
  compile Mach-Os, `-race` in CI. Full suite is green across all 18 packages.
- **Self-improve protocol.** `docs/self-improve-protocol.md` (committed-when-pushed;
  currently in the working tree) + `make qa` is the standard pre-commit gate.
  Staticcheck and govulncheck are pinned in go.mod via the `tool` directive; `make lint` runs both with zero
  extra installs.
- **Anti-slop writing.** Docs/help text are plain-language; no emoji, no
  marketing fluff (the repo references jalaalrd/anti-ai-slop-writing for style).
- **No destructive surprises.** `check --fix` writes a fixed copy, never mutates
  the input. The fetch cache prunes only the default cache dir (never a
  caller-supplied folder).

## 7. Key subsystems worth knowing before touching

### 7.1 The binary seam: `internal/macho`
`Bin` is the surface (RemoveSignature, Fakesign, ChangeDependency,
SetInstallName, AddRpath/RemoveRpath, ListDependencies, IsEncrypted, inject).
Two subtle pieces you must not break:

- **Growing-rename machinery:** changing a load-command dependency to a *longer*
  string grows the Mach-O and shifts `__LINKEDIT` — handled by re-serializing
  and re-offsetting every linkedit-referencing load command. Byte-identical
  tests pin the rpath serializer (the self-align fix against otool on a
  synthetic 32-bit fixture).
- **`__cstring` rewrite (`cstring.go`):** runtime dlopen strings compiled into
  `__TEXT` are rewritten too — in-place when they shrink/fit, otherwise packed
  into a new `__PATCH_ROOTLESS,__cstring` segment inserted before `__LINKEDIT`
  with ADRP/ADD/ADR instruction retargeting. Runtime-proofed by loading a
  converted dylib via dyld and asserting dlerror names the new path.

### 7.2 `xkvm check` — merge-completeness (two tiers)
- **Tier 1 (deterministic):** every bundle-relative load-command dep
  (`@rpath/`, `@executable_path/`, `@loader_path/`) must resolve inside the
  bundle; Swift shims and system paths exempt.
- **Tier 2 (heuristic):** bare `NAME.framework` tokens in the strings of
  **non-main** binaries — the runtime-`dlopen` signature (RyukGram's settings
  UI dlopen'ing `ffmpegkit.framework` is the canonical case). Reachability is
  scoped by tweak family (the `.bundle`/`.appex`/root-`<Name>.dylib` stem).
  Findings are marked `Suspected`.
- `check --fix` searches a fix-dir / local debs / the fetch cache / online
  repos for the missing piece, installs it in the right place, re-signs, and
  writes a fixed copy.

### 7.3 `internal/decrypt` (committed as `7b7e373`)
Port of PancakeStore's `MuffinStoreJailed/Functions/IPATool.swift` (itself
ipatool-derived). Steps, each hermetic-httptest-pinned: GUID → bag.xml →
authenticate (2FA = `ErrTwoFactor`, pod-follow "russia fix", trailing-slash
"brazil fix") → buy endpoint (session headers + cookies) → zip download with
`iTunesMetadata.plist` + `SC_Info/` sinfs. **Boundary (documented in help):**
the binary stays FairPlay-encrypted; real Mach-O decryption needs a jailbroken
device. Auth persists to `~/Library/Application Support/xkvm/auth.json` (0600,
plaintext password — treat like `~/.ssh`). The TUI shows a 1/2/3 output-path
question (ask/reuse/never) and a session menu (keep / change account / logout).

### 7.4 `internal/patch` registry
Adding a compatibility patch is one `init()` registration + `Name()`/`Apply()`;
the CLI binds `--<name>` flags automatically (sorted). Mach-O-touching patches
must follow the strip-edit-resign discipline (extract ents → remove sig → edit
→ sign with ents). Mutual exclusivity lives in `Options.validate()`.

## 8. Current state (2026-08-23) — read this before starting work
- 9f840e6 — dedup batch:- b5b3745 — stdlib twins removed- REPO SPLIT (2026-08-23, post-ride): the device subsystem now lives standalone at github.com/gwnodex-bit/idev (private; module github.com/gwnodex-bit/idev) — device/ library + cmd/idev CLI + internal/log ported verbatim with import rewrites, full test suite green, own Makefile/CI. xKVM's internal/device copy remains authoritative-for-xkvm until PHASE 2: flip xkvm to `require github.com/gwnodex-bit/idev` + delete internal/device + repoint cli/tui imports. Do NOT edit both copies for new features — land in idev first.
- be304a7+e888bb5 — auto-tunnel version gate:- 82934ad — device automation pair: `xkvm device forward <host> <dev>` (iproxy-style usbmuxd relay, every iOS version, no tunnel) and `xkvm device pasteboard get|set` (clipboard); Handler seam grew Forward/PasteboardGet/PasteboardSet.
- 6965577 — fuzz batch: FuzzParseNoPanicNoEscape smashes the .cyan parser (shared-file hostile input; zip bytes -> Parse+Validate) asserting no panic and no outDir escape. 30s / 103k execs clean — the inject/ zip-slip guard holds.
- 82934ad+docs — device automation pair: `xkvm device forward <host> <dev>` (iproxy-style usbmuxd relay, every iOS version, no tunnel) and `xkvm device pasteboard get|set` (clipboard). Handler seam grew Forward/PasteboardGet/PasteboardSet. Also restores README device rows silently dropped when an earlier multi-assert doc edit aborted mid-cell.
- ab6ecff — fuzz batch: three new parser targets (FuzzDebArMember / FuzzSafeJoin / FuzzParseIndex, 30s each, ~3.8M execs clean); building the ar target exposed and fixed a live panic — readArMember allocated from the untrusted 10-char size field (negative = makeslice panic; huge = OOM steer), now bounded by maxARMemberSize with TestArMemberRejectsHostileSizes pinning it. Any crafted .deb could crash xkvm before this.
 shouldAutoTunnel skips the spawn on iOS 16 and older (DDI territory; unknown versions still attempt), remediation text now covers both gate generations. Process note: be304a7 briefly shipped with an unformatted test file (gofmt red, tests green), fixed in e888bb5; wake-up gating is now structurally &&-chained.
 (net.JoinHostPort, strconv.Atoi). NEXT UP (found via hidden-bug checklist): auto-tunnel should skip spawn when device reports iOS < 17 (DDI territory, not CoreDevice) — pure predicate + table test, wire into GoIOS.tunnelReady.
 internal/fsutil replaces three drifted copy helpers (streaming + perms preserved; pkgmirror no longer flattens modes); slices.Contains/builtin max/cmp.Or replace local re-implementations.

**HEAD is past `5e9f4d1`; read `git log` for the live list.** Batches since
then, newest first:

- Device-control parity batch (this one): auto-managed iOS 17+ developer
  tunnels (discover -> spawn -> retry; `internal/device/tunnel.go`,
  XKVM_NO_AUTO_TUNNEL kill-switch), os_trace syslog fallback over the tunnel,
  `device watch` / `screenshot` / `devmode`, syslog `--process/--contains`
  filters, entitlements plist pre-validation in Options.validate.
- Agent-context docs (fe98d3a + f74a18a): AGENTS.md expanded, CLAUDE.md,
  docs/PRD.md, docs/TTD.md, docs/ARCHITECTURE_OVERVIEW.md added then made
  LOCAL-ONLY (untracked + gitignored). A fresh clone will not have them;
  do not re-commit them.
- Earlier batches after d3da30d:

- `7b7e373` — `xkvm decrypt` (App Store ipatool flow): M5 §5.7 contract in
  ARCHITECTURE.md, the `internal/decrypt` package (16 hermetic tests; incl. the
  nil-interface `SinfPaths` panic fix + regression test), CLI + TUI wiring,
  `docs/self-improve-protocol.md`.
- `8fc61d0` — modernization: Go 1.26.4 → 1.26.6 (clears the 6 reachable stdlib
  CVEs govulncheck reported), staticcheck + govulncheck pinned via the go.mod
  `tool` directive (`tools.go` deleted), `make lint`/`make qa`/CI updated,
  `sort.Slice` → `slices.SortFunc`, named struct types in
  `internal/macho/cstring.go`.
- `5e9f4d1` — release pipeline: `.github/workflows/release.yml` fires on
  `v*` tags (goreleaser builds the six combos, injects the tag into
  `xkvm --version` via `-X internal/app.Version`, publishes the assets the
  one-shot installers expect). `app.Version` is now a `var` for that reason;
  asset naming and the installers are one contract — change them in lockstep.
  To cut a release: `git tag v0.1.0 && git push origin v0.1.0` (see
  CONTRIBUTING.md "Cutting a release").

- **Verified 2026-08-16:** `make qa` fully green — lint (gofmt/vet/tidy/staticcheck/
  govulncheck; govulncheck: **no vulnerabilities found**) + `-race` suite across
  all 18 packages + 6-combo cross-compile. CI runs the same gate on macOS-14
  arm64 / Ubuntu x64 / Windows x64.
- **What's NOT verified (only a real login can prove it):** live Apple auth —
  no Apple ID exists on this dev machine. Apple's auth is unstable upstream
  too. If live auth fails, the likely first place to look is cookies from
  intermediate redirect hops (Go captures only the final response's cookies).

## 9. Build / test / QA

```bash
make build        # → bin/xkvm
make lint         # gofmt + go vet + go.mod/tools consistency + staticcheck
make test         # go test ./...
make qa           # full gate: lint + -race suite + 6-combo cross-compile
go test -race ./internal/decrypt/   # just the new package
```

E2E against real artifacts lives in `scripts/` (`e2e-real.sh`, `check-smoke.sh`);
they need real IPAs/debs and the macOS native toolchain — not part of `make qa`.

## 10. Documentation index

| Path | Content |
|---|---|
| `ARCHITECTURE.md` | The deep design doc: upstream parity tables, every converter's exact steps, check tiers, patch registry, decrypt contract (§5.7), milestones, risks. **The authoritative spec — read the relevant section before touching a subsystem.** |
| `README.md` | User-facing: install, quick start, commands, common flags |
| `feather-ellekit-spec.md` | Feather-style ElleKit integration spec (D10 patch contract) |
| `docs/ellekit-build.md` | Building a fat arm64+arm64e ElleKit.framework for vendoring |
| `docs/roothide-install.md` | Installing converted packages on a roothide jailbreak |
| `docs/self-improve-protocol.md` | Static + dynamic bug-hunting loop; `make qa` bundles its mechanical phases |
| `docs/HANDOFF.md` | **This file** |
| `NOTICE` | Third-party provenance (Cephei GPL-family, sideload dylibs, etc.) |
| `CONTRIBUTING.md` | Contribution flow and test conventions |
