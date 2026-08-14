<div align="center">

```
 ___    ___ ___  __    ___      ___ _____ ______
|\  \  /  /|\  \|\  \ |\  \    /  /|\   _ \  _   \
\ \  \/  / | \  \/  /|\ \  \  /  / | \  \\\__\ \  \
 \ \    / / \ \   ___  \ \  \/  / / \ \  \\|__| \  \
  /     \/   \ \  \\ \ \  \    / /   \ \  \    \ \  \
 /  /\   \    \ \__\\ \__\ \__/ /     \ \__\    \ \__\
/__/ /\ __\    \|__| \|__|\|__|/       \|__|     \|__|
|__|/ \|__|
```

**The iOS tweak toolbox, in your terminal.**

Inject tweaks into apps · extract them back out · convert jailbreak packages between formats.

> 🎨 **New to xkvm? Run `xkvm tui`** — a colorful menu that asks questions in plain
> words, animates work with a block spinner, and shows you what happened. No flags
> to memorize: it drives the exact same engine as the command line.

[![CI](https://github.com/xscope0/xkvm-ios-injector/actions/workflows/ci.yml/badge.svg)](https://github.com/xscope0/xkvm-ios-injector/actions)
[![Go](https://img.shields.io/badge/Go-1.26-blue)]()
[![License: MIT](https://img.shields.io/github/license/xscope0/xkvm-ios-injector)](LICENSE)

</div>

---

## What is xkvm?

xkvm is a command-line tool for people who sideload iOS apps. Give it an app (`.ipa`, `.tipa`, or `.app`) and a tweak, and it does the fiddly parts for you — no GUI, no clicking through wizards.

With xkvm you can:

- **Inject tweaks into an app.** Add a tweak (`.dylib`, `.deb`, framework, or bundle) to an app, wire it up so it actually loads, and re-sign everything so the app still installs.
- **Extract tweaks from an app.** Take a modified app you already have and pull the injected tweaks back out — dylibs, frameworks, bundles, app extensions.
- **Convert jailbreak packages.** Translate a tweak package between the three major jailbreak styles — *rootful* (classic), *rootless* (modern `/var/jb`), and *roothide* (jbroot) — and between `.deb` and `.dylib` forms.
- **Fetch tweaks from Cydia repos.** Resolve a tweak by its bundle id through Canister / MobileAPT, including its dependencies.
- **Fix sideloading problems.** Inject a bundled set of App Store / keychain repair dylibs and apply compatibility patches to apps that misbehave when sideloaded.

Everything is written in **Go**, with no external tools required for the heavy lifting. macOS is the primary platform; the core also builds and is tested on Linux.

## Features

| | |
|---|---|
| 🔧 **Tweak injection** | Add dylibs, debs, frameworks, and bundles to an app — with correct load commands (`@rpath`, `@executable_path`) and re-signing. |
| 🧩 **Tweak extraction** | `xkvm extract` pulls every injected artifact back out of an app, recording where each one lived so re-injection is automatic. |
| 🔁 **Package conversion** | Faithful ports of the ecosystem's own converters: rootful → rootless → roothide, and `.deb` ↔ `.dylib`. |
| 📦 **Repo fetching** | `--fetch` resolves tweaks by bundle id through Canister / MobileAPT with dependency recursion. |
| 📝 **Shareable configs** | `.cyan` files capture every option — generate them with `cgen`, validate them with `cyan-check`, apply them with `-z`. |
| 🩹 **Sideload fixes** | `--patch` injects the bundled sideload-repair dylib set; `--ellekit` swaps in the real ElleKit hooking runtime. |
| ✅ **Completeness checks** | `xkvm check` verifies every bundle-relative dependency resolves — so merged tweaks don't crash at launch. |
| 🧼 **Deterministic builds** | Same input, same output — with zip-slip-safe extraction and pure-Go Apple-format code signatures. |

## Try it in 30 seconds

```bash
xkvm tui          # the friendly menu — pick "inject", answer the questions
```

Or go straight to the command line:

```bash
xkvm -i App.ipa -f MyTweak.dylib -o App-Tweaked.ipa   # inject a tweak
xkvm extract -i App-Tweaked.ipa -o tweaks/            # pull tweaks back out
xkvm rootless -i tweak.deb -o tweak-rootless.deb      # convert a package
xkvm check -i App-Tweaked.ipa                         # find missing files before you install
```

Every menu screen ends with the equivalent command, so the TUI doubles as a teacher.

## Installation

**Requirements:** macOS (primary), [Go 1.26+](https://go.dev/dl/).

```bash
git clone https://github.com/xscope0/xkvm-ios-injector.git
cd xkvm-ios-injector
make build          # produces ./bin/xkvm
```

Or install directly with Go:

```bash
go install github.com/xscope0/xkvm-ios-injector/cmd/xkvm@main
```

Check that it works:

```bash
xkvm --help
```

## Quick start

**Inject a tweak into an app:**

```bash
xkvm -i App.ipa -o Patched.ipa -f MyTweak.dylib
```

**Inject several tweaks and fakesign (AppSync / TrollStore):**

```bash
xkvm -i App.ipa -o Patched.ipa -f TweakA.dylib -f TweakB.deb -s
```

**Extract the tweaks from a modified app:**

```bash
xkvm extract -i Patched.ipa -o extracted-tweaks/
```

**Convert a tweak package for a different jailbreak:**

```bash
xkvm rootless -i classic.deb -o modern.deb     # rootful → rootless
xkvm roothide  -i modern.deb -o jbroot.deb     # rootless → roothide
xkvm undeb     -i tweak.deb  -o artifacts/     # unpack a .deb
xkvm debify    -i MyTweak.dylib -o MyTweak.deb # .dylib → .deb
```

**Fetch a tweak from a Cydia repo:**

```bash
xkvm -i App.ipa -o Patched.ipa --fetch com.example.tweak
```

## Commands

| Command | What it does |
|---|---|
| `xkvm -i <app> ...` | Inject tweaks, modify the app, and re-sign it |
| `extract` | Pull tweaks (dylibs, frameworks, bundles, app extensions) out of an app |
| `rootless` | Convert a rootful package to the modern rootless layout |
| `roothide` | Convert a rootless package to a roothide-jailbreak package |
| `debify` | Build a MobileSubstrate `.deb` from a dylib or payload directory |
| `undeb` | Unpack a `.deb` into its tweak artifacts |
| `check` | Verify bundle-relative dependencies resolve (merge completeness) |
| `cyan-check` | Validate a `.cyan` config file before applying it |
| `cgen` | Turn your flags into a shareable `.cyan` config file |

## Common options

| Flag | What it does |
|---|---|
| `-i, --input` | The app to modify (`.ipa`, `.tipa`, or `.app`) |
| `-o, --output` | Where to write the result (defaults to overwriting the input) |
| `-f, --file` | A tweak to inject — repeatable (dylibs, debs, frameworks, bundles) |
| `-z, --cyan` | A `.cyan` config file to apply — repeatable |
| `--fetch` | Fetch a tweak by bundle id via Canister / MobileAPT |
| `--ellekit` | Use the real ElleKit hooking runtime |
| `--patch` | Inject the bundled sideload-repair dylib set (implies `--fakesign`) |
| `-s, --fakesign` | Fakesign all binaries (AppSync / TrollStore) |
| `-b / -n / -v / -m` | Change bundle id, name, app version, or minimum OS version |
| `-k, --icon` | Change the app icon |

Run `xkvm --help` for the complete list.

## How it works

- **Pure-Go Mach-O editing and code signing** — built on [`blacktop/go-macho`](https://github.com/blacktop/go-macho) and its `pkg/codesign`, producing Apple-format signatures macOS itself validates. No `ldid` or `install_name_tool` needed.
- **Faithful package conversion** — the converters are ports of the ecosystem's own tools ([rootless-patcher](https://github.com/NightwindDev/rootless-patcher), [RootHidePatcher](https://github.com/roothide/RootHidePatcher)), pinned byte-for-byte against upstream output by golden tests.
- **Safety by default** — zip-slip-safe container handling, deterministic builds, and a post-conversion audit that flags surviving rootful paths.

## Documentation

- [ARCHITECTURE.md](ARCHITECTURE.md) — design, milestones, and fidelity notes
- [docs/ellekit-build.md](docs/ellekit-build.md) — building ElleKit for injection
- [docs/roothide-install.md](docs/roothide-install.md) — installing converted packages on a roothide jailbreak
- [feather-ellekit-spec.md](feather-ellekit-spec.md) — Feather-style ElleKit integration spec

## Project status

| Milestone | Content | Status |
|---|---|---|
| M0 | Scaffold: CLI, logging, CI | ✅ |
| M1 | Containers: ipa/deb/plist, extract command | ✅ |
| M2 | Injection parity (hybrid toolchain) | ✅ (superseded by M3) |
| M3 | Pure-Go Mach-O (go-macho / codesign) | ✅ |
| M4 | Azule fetch: Canister / MobileAPT | ✅ |
| M5 | iOS on-device: decrypt, cross-compile | ⬜ planned |
| M6 | Ship: brew tap, releases, docs | ⬜ planned |

## Contributing

Contributions are welcome — bug reports, feature ideas, and pull requests. See [CONTRIBUTING.md](CONTRIBUTING.md) to get started. Every behavior change ships with a test; the house style is golden tests that pin converters byte-for-byte against upstream output.

## Acknowledgments

xkvm descends from two lineages and borrows conventions from several more:

- **[cyan / pyzule-rw](https://github.com/asdfzxcvbn/pyzule-rw)** (Unlicense) — the app-modifier / tweak-injector lineage xkvm descends from
- **[Azule](https://github.com/mpelteshki/Azule)** (archived) — repo fetching and App Store decryption ideas
- **[Feather](https://github.com/claration/Feather)** (GPL-3.0) — sideloading and ElleKit conventions
- **[rootless-patcher](https://github.com/NightwindDev/rootless-patcher)** (MIT) — rootless conversion semantics
- **[RootHidePatcher](https://github.com/roothide/RootHidePatcher)** (GPL) — roothide conversion semantics (reference only)
- **[Derootifier](https://github.com/haxi0/Derootifier)** (GPL-3.0) — format reference for `--tweakinject`
- **[ElleKit](https://ellekit.space/)** — the hooking runtime
- **[blacktop/go-macho](https://github.com/blacktop/go-macho)** (MIT) — Mach-O parsing and code signing

## License

The xkvm source code is licensed under the [MIT License](LICENSE).

xkvm also bundles third-party components (the ElleKit runtime, sideload-repair dylibs, and a Cephei framework) under their own licenses. See [NOTICE](NOTICE) for the full provenance.
