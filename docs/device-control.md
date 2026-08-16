# Device Control — implementation-ready blueprint

Generated before code (project-workflow-analysis-blueprint-generator), then
frozen after build. References cross-checked against Context7
(`/danielpaulus/go-ios`, 600 snippets, benchmark 64.3) **and** the module
source of `github.com/danielpaulus/go-ios v1.3.2` (the authoritative oracle);
where they disagreed, the source won (example: v1.3.2 syslog has
`ReadLogMessage()/Parser()`, not `StartCapture(ch)`).

## 1. Purpose

Turn xkvm from "produces an .ipa, hands it to SideStore" into a complete
loop: **pair → inspect → install → launch → observe**. Same transport family
(pure-Go Apple protocols) as the vendored go-macho/ipsw components.

## 2. Entry point

`xkvm device <op> [flags]` — cobra subcommands:

| op | action | go-ios call |
|---|---|---|
| `list` | USB + network devices | `ios.ListDevices()` |
| `pair` | standard trust-dialog pairing; `--supervised p12:pw` | `ios.Pair` / `ios.PairSupervised` |
| `info` | lockdown values (name/version/type/color…) | `lockdown GetValues` |
| `battery` | battery diagnostics | `ios.GetBatteryDiagnostics` |
| `apps` | user/system app inventory | `installationproxy` Browse |
| `install <ipa>` | zip-conduit install (Xcode path) | `zipconduit.SendFile` |
| `uninstall <id>` | instance proxy uninstall | `installationproxy.Uninstall` |
| `launch <id>` / `kill <pid>` | process control | `instruments.ProcessControl` |
| `syslog` | stream parsed logs | `syslog.New` + `ReadLogMessage` |
| `restart` / `shutdown` | diagnostics service | `diagnostics.Reboot/Shutdown` |

Flags: `--udid` (resolve ambiguity), `--network` (prefer network transports),
`--timeout` (per-service dial default 15s), `--json` (machine output for
`info`/`apps`/`battery`).

## 3. Workflows (data flow + error policy)

**W0 field note (verified live, iOS 26.1 iPhone 11)** — one phone on
USB + WiFi appears twice from usbmuxd with the same UDID; `Distinct` counts
UDIDs, prefers USB, and every transport rides along for display
(`USB+Network`). launch/kill/install on stock iOS 17+ hit the developer
tunnel gate (upstream error classes mapped to a precise remediation
command); syslog relay is unavailable on stock iOS 17+; pair/info/battery/
apps need no tunnel.

**W1 list/resolve** — usbmuxd → `DeviceList{DeviceList []DeviceEntry}`;
`DeviceEntry.Properties.SerialNumber` is the UDID, `ConnectionType` is
"USB"/"Network". UDID absent + exactly one device = fast path; zero =
exit 65 (no device + remediation text: cable, trust, usbmuxd); >1 and no
`--udid` = exit 66 listing the choices (never guess).

**W2 pair** — standard pairing returns an error whose text contains
"PairingDialog" on first attempt; xkvm maps that to a dedicated exit code 67
with "accept on the device, then run `xkvm device pair` again". Supervised
pairing (Apple Configurator p12) surfaces
`ErrDeviceLockedPairingDeferred` as exit 67 too (unlock once, re-pair).
Pair records are persisted **by usbmuxd** (/var/db/lockdown) — xkvm owns no
record files, so no cleanup/rotation logic to get wrong.

**W3/W4 install/uninstall** — `zipconduit.New(device)→SendFile(path)` handles
`.ipa` and app directories, streams STORE-mode zip frames, then
`waitForInstallation()` polls progress; `installationproxy.Uninstall` for
removal. Install errors that mention dev images (iOS 17+) hint at RSD path —
reported verbatim, never masked.

**W5 process control** — `NewProcessControl(device)`, `LaunchApp(bundleID,
map[string]any{})` (or `LaunchAppWithArgs` for env/args), `KillProcess(pid)`.

**W6 syslog** — `syslog.New(device)`, loop `ReadLogMessage()`, parse with
`syslog.Parser()`, print `ts process message`, `Close()` — always deferred
(uv-style: no goroutine leaks).

**W7 diagnostics** — `Reboot()`/`Shutdown()` confirm before acting (typed
`y` gate); `GetBatteryDiagnostics` returns IORegistry battery block.

## 4. Error taxonomy (all errors are typed, none are swallowed)

`internal/device` returns `*device.Error{Kind, Op, Remediation}`; cobra maps
Kind → exit codes 65–69. Wrapped with `%w` from go-ios so upstream strings
survive. Timeouts via `context.WithTimeout` (dial-tolerant retry ×2 on
"connection refused" only — pairing and install are NOT retried silently).

## 4b. Omega (blacklist remover) — ported feature

`device omega` reimplements jailbreak.party Omega in Go over go-ios: build
the 17-record MBDB in memory (`internal/device/mbdb.go`, golden-pinned),
speak the mobilebackup2 device-link protocol (`internal/device/restore.go`:
4-byte length + plist frames, DLMessageVersionExchange/ProcessMessage/
DownloadFiles/StatusResponse, chunk codes 0xC/0x0, zero terminator),
replace DatabaseDomain MobileIdentityData + ProtectedDomain trustd
databases with directories, inject the two skip-setup plists, reboot via
diagnostics. Version policy: 16-18 supported / 19-26 untested (caution) /
<16 and >=27 hard block (KindUnsupported, exit 66) — Omega's own README
warns iOS 27 restores can reset data. Protocol core fully driven by a
scripted peer over net.Pipe in tests (handshake, options, payload
byte-equality, Find My refusal, crash_on_purpose, missing-file path).

## 5. Architecture

cmd/xkvm (cobra) → internal/device (orchestration, retries, typed errors)
→ go-ios ios/{installationproxy,zipconduit,instruments,syslog,diagnostics}
→ usbmuxd. No shared state; each op opens/closes its own connections.

```
CLI flags ──► device.Resolve(udid) ──► DeviceEntry
                    │
        ┌───────────┼────────────────────────────┐
     pair        install          launch/observe
        │           │                     │
     lockdown    zipconduit        instruments / syslog
        └───────────┴─────────┬──────────────┘
                              ▼
                       usbmuxd socket
```

## 6. Testing approach

- **Hermetic (default, CI):** `device.Client` seam interface; fake client
  satisfies every op; tests for resolve ambiguity, pairing-dialog mapping,
  error taxonomy, syslog loop close, exit codes. No hardware, no network.
- **Live (`//go:build live`):** real-device tests behind `XKVM_DEVICE_UDID`;
  run manually with a plugged phone (`go test -tags live -run Live ./internal/device/`).
- **CLI:** exit-code table tests via the existing piped-run harness.
