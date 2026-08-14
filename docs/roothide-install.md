# Installing a converted roothide deb on a real device

How to take an `xkvm roothide` output and install it on a roothide jailbreak
(iOS 15–17, A12+ arm64e devices). Covers both consumption paths the
converter produces: the **installable .deb** (what dpkg/the package manager
installs) and the optional **pkgmirror** snapshot (what roothide's package
manager reads as the installed footprint).

**Feature spec:** ARCHITECTURE.md §5.4 (`xkvm roothide`).
**Upstream reference:** [roothide/RootHidePatcher](https://github.com/roothide/RootHidePatcher)
(GPL-3.0 — semantics ported, no code copied; see NOTICE).
**Honest status:** the conversion is machine-verified (otool/dpkg on real
debs, golden tests in CI) but the final proof — a tweak actually loading on a
roothide jailbreak — is the on-device step below. If anything here differs
from what the device does, the device wins; file it as an issue.

---

## 1. What the conversion produces

```bash
xkvm roothide -i rootless.deb -o roothide.deb [--pkgmirror] [--mode dynamic]
```

One input (a rootless `.deb`, payload under `var/jb/`) becomes one output:

| Piece | What it is | Who consumes it |
|---|---|---|
| The `.deb` itself | hoisted payload (`var/jb/*` → package root), system files under `rootfs/`, every `/var/jb/...` load command and LC_RPATH rewritten to `@loader_path/.jbroot/...`, control re-arched to `iphoneos-arm64e`, scripts/plists path-translated, Mach-Os re-signed (executables: roothide platform entitlements merged with existing; others: plain ad-hoc) | dpkg / Sileo / Zebra |
| `var/mobile/Library/pkgmirror/` (only with `--pkgmirror`) | a **snapshot of the hoisted-but-unmodified package**: control dir renamed `DEBIAN.<pkg>`, payload with its *original* `/var/jb` load commands, every entry `0755` | roothide's package-manager integration (sees it as an installed footprint; the Bootstrap's mobile-dir ownership fix skips it so it keeps its ownership) |

Key point: the mirror is *not* a second copy of the patched package — it's
the reference snapshot the reference tool creates before patching (see
ARCHITECTURE.md §5.4 for the exact upstream ordering). The **installable
artifact is the .deb**.

---

## 2. Prerequisites

- A **roothide jailbreak** on the device: [Dopamine-roothide](https://github.com/roothide/Dopamine-roothide)
  or [Bootstrap](https://github.com/roothide/Bootstrap) (iOS 15–17, A12+ —
  roothide is arm64e-only).
- A package manager: **Sileo** or **Zebra** (both ship with the Bootstrap).
- Something to get the `.deb` onto the device: AirDrop + Files, Filza, or
  `scp` over SSH (the Bootstrap installs OpenSSH).
- `xkvm` built for your machine: `go build -o bin/xkvm ./cmd/xkvm` in the repo.

---

## 3. Install the converted .deb

### 3a. Via the package manager (recommended)

1. AirDrop / copy `roothide.deb` to the device into Files.
2. Open it with **Filza** (install Filza from the roothide repo if needed) →
   "Open with Sileo" / "Open with Zebra".
3. Sileo/Zebra shows the package — the control it reads carries
   `Architecture: iphoneos-arm64e` plus the mode-dependent edits:
   - **`--mode auto`** → `Pre-Depends: rootless-compat(>= 0.9)` and a
     `.roothidepatch` symlink next to every payload Mach-O (the
     AutoPatches mechanism — the roothide runtime auto-patches binaries
     with a `.roothidepatch` sibling).
   - **`--mode dynamic`** → `Version: <ver>~roothide` and
     `Pre-Depends: patches-<pkg>(= <ver>~roothide)`. The `patches-<pkg>`
     dependency comes from the **roothide patches repo**; make sure that
     repo is added (the Bootstrap adds it by default).
   Install → respring.

### 3b. Via dpkg from a terminal

With SSH or the Bootstrap's terminal (root shell on the roothide jbroot):

```bash
# copy the deb to the device, then:
dpkg -i /path/to/roothide.deb
# or, resolving the patches-<pkg> dependency:
apt install /path/to/roothide.deb
```

`dpkg -i` will complain about the unresolved `patches-<pkg>` Pre-Depends if
the patches repo isn't reachable — that is *expected*: the Pre-Depends is
how dynamic mode wires the roothide patches mechanism. Use `apt install`
(which resolves from the repo) or `--force-depends` only if you know the
patches package is unnecessary for your tweak.

### 3c. Verify the install

```bash
dpkg -s <pkg>          # Status: install ok installed, Version <ver>~roothide
dpkg -L <pkg>          # payload landed at the package root (hoisted), not var/jb
```

Then check the actual load commands on the installed dylib — every jailbreak
path must be `@loader_path/.jbroot/...`:

```bash
# from the device (otool is in the bootstrap):
otool -L <installed-dylib-path>
# every line should read @loader_path/.jbroot/..., never /var/jb/...
```

Launch the tweak's target app. The roothide bootstrap injects at
`@loader_path/.jbroot` — if the app's sandbox blocks the container jbroot,
that's a roothide runtime issue, not the conversion.

---

## 4. The pkgmirror path (when to use `--pkgmirror`)

The `pkgmirror` (`var/mobile/Library/pkgmirror`, control dir `DEBIAN.<pkg>`)
is the roothide-side record that a patched package exists. The Bootstrap
(`Bootstrap/bootstrap.m`, `fixMobileDirectories`) explicitly skips
`/var/mobile/Library/pkgmirror` when it re-owns mobile directories — the
mirror keeps the ownership the patch tool set (mobile, 501:501), which is
why the converter emits it with world-readable `0755` modes. It is a
**pre-patch snapshot**: it never contains payload `.roothidepatch` symlinks
(the AutoPatches siblings ship only in the installable payload), and it
copies any `DEBIAN/*.roothidepatch` files the input package itself shipped
(upstream's best-effort `cp`).

Practical guidance:

- **Use `--pkgmirror` when the package manager on the device needs the
  mirror to see the package as installed** (the reference tool's default
  integration point — its commented-out TODO even shows appending
  `Status: install ok installed` to the mirror's control).
- **Skip it for a plain dpkg install.** The mirror adds payload size and is
  not needed to install the .deb itself.
- The mirror's payload keeps **original** `/var/jb` load commands by design
  (it's a pre-patch snapshot). Do not "fix" those — the patched copies are
  the ones the .deb installs; the mirror is metadata + reference content for
  the roothide package manager, not the runtime payload.

---

## 5. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| conversion refuses: "not a rootless package (Architecture: …)" | you fed a rootful (`iphoneos-arm`) or already-roothide deb — upstream exits 1 on the same input | convert rootful debs with `xkvm rootless` first, then feed the rootless output back in |
| Sileo refuses to install: unresolved `patches-<pkg>` | dynamic mode adds the Pre-Depends; the patches repo isn't added | Add the roothide patches repo, or rebuild with `--mode auto` (rootless-compat dep) or default (no dep) |
| tweak loads but is never auto-patched | you built with `--mode dynamic` or default, which ship no `.roothidepatch` symlinks | rebuild with `--mode auto` (adds the symlinks + `rootless-compat` dep) if you want the AutoPatches treatment |
| `dpkg -i` succeeds but the tweak never loads | app not finding the dylib at `@loader_path/.jbroot`, or the app is sandboxed away from the jbroot | verify `otool -L` paths; confirm the app was fully relaunched (not just backgrounded) |
| fixed-paths warning at convert time | surviving `/var/jb` strings anywhere in the walked payload: Mach-O `__cstring` (string tables aren't rewritten by the roothide pass), a missed load-command dep/rpath rewrite, or printable strings in other payload files (`.png`/`.strings` excluded) | informational; if a jailbreak check breaks the tweak, that's the runtime behavior to watch — and a load-command hit means a rewrite *miss* worth fixing |
| "Killed: 9" / signature error at launch | a signature the device rejects — xkvm re-signs with ad-hoc Apple-format signatures, which jailbroken installs normally accept | re-sign with ldid on-device (`ldid -S <binary>`) as a fallback; report the exact error if this ever happens |
| tweak works in some apps, not others | roothide's per-app injection list | enable the app in the roothide manager's injection settings |

---

## 6. Security note

A converted roothide deb **replaces the original code signature** on every
Mach-O it touches (load-command rewrites invalidate it; the converter then
re-signs with its own ad-hoc signature and, for executables, the roothide
platform entitlements). That is expected and documented (ARCHITECTURE.md
§5.4) — but it means the installed binary is no longer covered by the
developer's original signature, so:

- Only convert and install debs **you trust** (your own, or from sources you
  already run on the device).
- The output deb is a new artifact: `dpkg -L`/`dpkg -s` and a first-launch
  check after install are the minimum verification before relying on it.

---

## 7. What's still unverified (only a device can prove)

- That a converted tweak actually loads and hooks on a real roothide
  jailbreak (the load-command surgery is verified with otool; the runtime
  merge is not).
- That the `patches-<pkg>` dependency resolution works against the live
  roothide patches repo.
- That the pkgmirror snapshot is consumed the way the package manager reads
  it (the mirror's on-device role is inferred from the Bootstrap source,
  not observed on a device).

If you install a converted deb and hit a mismatch with this guide, the
guide is wrong — report it so the semantics can be corrected.
