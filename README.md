# xKVM

iOS app modifier & tweak injector, written in Go.

xKVM merges two lineages:
- **cyan** (pyzule-rw) — the actively maintained app modifier / tweak injector
- **Azule** — the (now archived) tool whose Canister/MobileAPT repo fetching and
  App Store decryption features cyan lacks

## Status

**Focus (per user scope):** host = macOS, target = `.ipa`. Core = inject `.dylib`/`.deb`
tweaks into an `.ipa`, and extract tweaks out of one (`xkvm extract`). Azule repo-fetch
/decrypt (M4/M5) stay wired as flags but are deprioritized.

| Milestone | Content | Status |
|---|---|---|
| M0 | Scaffold: CLI, logging, CI | ✅ |
| M1 | Containers: ipa/deb/plist, extract command | ✅ |
| M2 | Injection parity (hybrid toolchain) | ✅ (deleted — superseded by M3) |
| M3 | Pure-Go Mach-O (go-macho / codesign) | ✅ (native-only) |
| M4 | Azule fetch: Canister/MobileAPT | ⬜ |
| M5 | iOS on-device: decrypt, cross-compile | ⬜ |
| M6 | Ship: brew tap, releases, docs | ⬜ |

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full plan.
