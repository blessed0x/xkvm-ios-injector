package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xscope0/xkvm-ios-injector/internal/device"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// deviceOrHint resolves the target phone and surfaces any policy error
// (ambiguity, nothing connected) with its remediation. Returns false when
// the flow must stop.
func (u *UI) deviceOrHint() (device.Dev, bool) {
	if u.Device == nil {
		u.say(cRed, "device control isn't wired into this build — use the CLI:")
		u.say(cCyan, "  xkvm device <pair|info|apps|install|launch|kill|syslog>")
		return device.Dev{}, false
	}
	target, err := device.Resolve(context.Background(), u.Device, "")
	if err != nil {
		u.say(cRed, err.Error())
		return device.Dev{}, false
	}
	return target, true
}

// dvPair is the device menu's trust flow.
func (u *UI) dvPair() {
	u.flowIntro("pair the device", "trusts this computer on the phone. The first run makes the phone show a Trust dialog — tap it, then run pair once more (only needed once).", "plug in → device → pair")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	out, err := u.runSpinning("pairing "+target.UDID+"…", func() error {
		return u.Device.Pair(context.Background(), target.UDID, nil)
	})
	if err != nil {
		u.showResult(out, err, "paired "+target.UDID+" — install, launch and logs are unlocked")
		return
	}
	u.say(cGreen, "[ok] paired "+target.UDID+" — install, launch and logs are unlocked")
}

// dvInfo prints the lockdown card for the phone.
func (u *UI) dvInfo() {
	u.flowIntro("device info", "the phone's name, iOS version, model and architecture — one screen, everything the inject flow needs to know.", "device → info")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	out, err := u.runSpinning("asking the device…", func() error {
		info, err := u.Device.Info(context.Background(), target.UDID)
		if err != nil {
			return err
		}
		log.Infof("device   %s", info.Name)
		log.Infof("ios      %s (%s)", info.ProductVersion, info.BuildVersion)
		log.Infof("model    %s · %s · %s", info.ProductType, info.HardwareModel, info.DeviceColor)
		log.Infof("arch     %s", info.CPUArchitecture)
		log.Infof("serial   %s", info.SerialNumber)
		return nil
	})
	u.showResult(out, err, "device info")
}

// dvBattery prints the battery card.
func (u *UI) dvBattery() {
	u.flowIntro("battery", "charge level and charging state from the device's diagnostics.", "device → battery")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	out, err := u.runSpinning("reading battery…", func() error {
		b, err := u.Device.Battery(context.Background(), target.UDID)
		if err != nil {
			return err
		}
		state := "not charging"
		if b.IsCharging {
			state = "charging"
		}
		log.Infof("battery  %d%% (%s)", b.CurrentCapacity, state)
		return nil
	})
	u.showResult(out, err, "battery")
}

// dvApps browses installed apps and lets the user launch or remove the
// picked one.
func (u *UI) dvApps() {
	u.flowIntro("installed apps", "every user app on the phone with its bundle id. Pick one to launch it, or remove it (the phone asks you to confirm).", "device → apps → pick an app")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	var apps []device.App
	_, err := u.runSpinning("browsing apps…", func() error {
		var derr error
		apps, derr = u.Device.Apps(context.Background(), target.UDID, false)
		return derr
	})
	if err != nil {
		u.showResult("", err, "browsing apps")
		return
	}
	if len(apps) == 0 {
		u.say(cYellow, "no user apps found")
		return
	}
	choices := make([]Choice, 0, len(apps))
	for _, a := range apps {
		choices = append(choices, Choice{Name: a.BundleID, Desc: a.Name, Ex: a.Path})
	}
	picked := u.choose("pick an app:", choices, false)
	if len(picked) == 0 {
		return
	}
	app := apps[picked[0]]
	actions := u.choose("what to do with "+app.Name+"?", []Choice{
		{"launch it", "start the app and report its pid", "launch " + app.BundleID, false},
		{"uninstall it", "remove the app and its data (the phone confirms)", "uninstall " + app.BundleID, false},
	}, false)
	if len(actions) == 0 {
		return
	}
	switch actions[0] {
	case 0:
		out, lerr := u.runSpinning("launching "+app.Name+"…", func() error {
			pid, err := u.Device.Launch(context.Background(), target.UDID, app.BundleID, nil, nil)
			if err == nil {
				log.Infof("launched %s — pid %d (stop it with device → kill %d)", app.Name, pid, pid)
			}
			return err
		})
		u.showResult(out, lerr, "launching "+app.Name)
	case 1:
		if !u.askYesNo("really uninstall "+app.Name+"? this deletes the app and its data", false) {
			return
		}
		_, derr := u.runSpinning("removing "+app.Name+"…", func() error {
			return u.Device.Uninstall(context.Background(), target.UDID, app.BundleID)
		})
		if derr != nil {
			u.say(cRed, derr.Error())
			return
		}
		u.say(cGreen, "[ok] uninstalled "+app.BundleID)
	}
}

// dvInstall streams a local .ipa onto the device.
func (u *UI) dvInstall() {
	u.flowIntro("install an .ipa", "streams a built .ipa straight onto the phone over the Xcode zip-conduit — the other half of inject. Install this way right after you build.", "inject App.ipa → device → install")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	in := u.requirePath("which .ipa should I install?", "")
	if in == "" {
		return
	}
	out, err := u.runSpinning("streaming "+in+"… (the whole app rides the wire)", func() error {
		return u.Device.Install(context.Background(), target.UDID, in)
	})
	if err != nil {
		u.showResult(out, err, "installing")
		return
	}
	u.say(cGreen, "[ok] installed — launch it from device → apps, or watch it with device → syslog")
}

// dvLaunch starts an app by bundle id.
func (u *UI) dvLaunch() {
	u.flowIntro("launch an app", "start any installed app by its bundle id; the pid is printed so kill can stop it.", "device → launch com.example.app")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	id := u.readLine("bundle id: ")
	if id == "" {
		return
	}
	out, err := u.runSpinning("launching "+id+"…", func() error {
		pid, lerr := u.Device.Launch(context.Background(), target.UDID, id, nil, nil)
		if lerr == nil {
			log.Infof("launched %s — pid %d", id, pid)
		}
		return lerr
	})
	u.showResult(out, err, "launching "+id)
}

// dvKill stops a process by pid.
func (u *UI) dvKill() {
	u.flowIntro("kill a process", "stop a running app by the pid launch reported (or pick one from apps → launch).", "device → kill 1234")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	raw := u.readLine("pid: ")
	if raw == "" {
		return
	}
	pid, perr := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if perr != nil {
		u.say(cRed, raw+" is not a pid")
		return
	}
	out, err := u.runSpinning("killing "+strconv.FormatUint(pid, 10)+"…", func() error {
		return u.Device.Kill(context.Background(), target.UDID, pid)
	})
	if err != nil {
		u.showResult(out, err, "killing")
		return
	}
	u.say(cGreen, "[ok] killed pid "+strconv.FormatUint(pid, 10))
}

// chanWriter funnels handler-side writes into a channel so only the flow's
// own goroutine ever touches the output stream (single-writer guarantee —
// concurrent handler output and menu rendering would interleave on a real
// terminal and race the test buffers).
type chanWriter struct {
	ch  chan string
	ctx context.Context
}

func (w *chanWriter) Write(p []byte) (int, error) {
	// Deliver a line the handler already produced: the buffered channel has
	// room in practice, so a line emitted concurrently with cancel() must not
	// be swallowed by the stop path (user hits Enter as a log line arrives).
	select {
	case w.ch <- string(p):
		return len(p), nil
	default:
	}
	// Channel full: only then yield to cancellation instead of blocking.
	select {
	case w.ch <- string(p):
		return len(p), nil
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

// dvSyslog streams parsed device logs until the user presses Enter.
func (u *UI) dvSyslog() {
	u.flowIntro("live syslog", "the phone's log stream, parsed into timestamp / process / message lines. Press Enter to stop.", "device → syslog")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lines := make(chan string, 128)
	done := make(chan error, 1)
	go func() {
		done <- u.Device.Syslog(ctx, target.UDID, &chanWriter{ch: lines, ctx: ctx})
	}()
	u.say(cCyan, "streaming "+target.UDID+" — lines appear as the device emits them; press Enter to stop")
	stop := make(chan struct{})
	go func() {
		u.readLine("")
		close(stop)
	}()
	for {
		select {
		case ln, more := <-lines:
			if !more {
				return
			}
			fmt.Fprintln(u.Out, ln)
		case <-stop:
			cancel()
			// Drain what the handler already produced before we decide
			// it's finished. A buffered line and the handler's exit race
			// each other; printing the line first guarantees no log line
			// is swallowed by the "stopped" banner.
			timeout := time.After(2 * time.Second)
			for {
				// Emit every line already buffered without blocking.
				select {
				case ln, more := <-lines:
					if !more {
						u.say(cYellow, "log stream stopped")
						return
					}
					fmt.Fprintln(u.Out, ln)
					continue
				default:
				}
				// Nothing buffered: wait for the handler to exit, one more
				// line, or give up.
				select {
				case <-done:
					u.say(cYellow, "log stream stopped")
					return
				case ln, more := <-lines:
					if !more {
						u.say(cYellow, "log stream stopped")
						return
					}
					fmt.Fprintln(u.Out, ln)
				case <-timeout:
					u.say(cYellow, "log stream stopped")
					return
				}
			}
		case err := <-done:
			if err != nil {
				u.say(cRed, err.Error())
			}
			return
		case <-ctx.Done():
			return
		}
	}
}

// dvOmega is the blacklist remover — jailbreak.party Omega reimplemented on
// the native stack: a partial backup restore that replaces the revoke +
// certificate databases with directories the system can no longer write to.
// Version policy: 16-18 and 26 supported (26.1 live-verified), <16 /
// 19-25 / >=27 hard-blocked, 28+ untested (future, unreleased). Apple
// never released iOS 19-25; iOS 27 is blocked because its restores can
// reset data.
func (u *UI) dvOmega() {
	u.flowIntro("blacklist remover (Omega)", "clears the databases that remember which of your sideloaded apps are revoked or which signing certificates are banned — the jailbreak.party Omega restore, rebuilt in Go. The phone reboots by itself when it finishes; turn Find My OFF and back up first.", "device → omega")
	target, ok := u.deviceOrHint()
	if !ok {
		return
	}
	var version string
	if info, err := u.Device.Info(context.Background(), target.UDID); err == nil {
		version = info.ProductVersion
	}
	if version == "" {
		version = u.readLine("couldn't read the OS version from the device — enter it (X.Y): ")
		if version == "" {
			return
		}
	}
	verdict, why, err := device.OmegaVerdict(version)
	if err != nil {
		u.say(cRed, err.Error())
		return
	}
	switch verdict {
	case device.Unsupported:
		u.say(cRed, "[hard block] omega on iOS "+version+" is NOT supported — xkvm will not run it")
		u.say(cWhite, "  why: "+why)
		u.say(cYellow, "  this is for your own protection: the restore could reset your data. nothing was changed.")
		return
	case device.Untested:
		u.say(cYellow, "[caution] iOS "+version+" is a future/release nobody has verified Omega on yet")
		u.say(cWhite, "  why: "+why)
		u.say(cYellow, "  make a full backup before continuing — you are the first line of defense here")
	case device.Supported:
		u.say(cGreen, "[supported] iOS "+version+" — "+why)
		u.say(cYellow, "  reminder: turn Find My OFF and make a backup before continuing")
	}
	u.say(cWhite, "  the restore replaces the revoke + certificate databases; your apps stay installed")
	if u.readLine("type CONTINUE to run the restore (anything else backs out): ") != "CONTINUE" {
		u.say(cYellow, "cancelled — nothing was changed")
		return
	}
	last := float64(-25)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, rerr := u.runSpinning("restoring… the phone reboots by itself when done", func() error {
		return u.Device.OmegaRestore(ctx, target.UDID, func(pct float64) {
			if pct-last >= 25 {
				log.Infof("restore progress: %.0f%%", pct)
				last = pct
			}
		})
	})
	u.showResult(out, rerr, "omega restore")
	if rerr == nil {
		u.say(cWhite, "  the device reboots now — revoked apps and banned certificates are forgotten for good")
	}
}
