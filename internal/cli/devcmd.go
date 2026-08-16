package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xscope0/xkvm-ios-injector/internal/device"
	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// deviceHandler builds the device-control handler. A variable so tests can
// inject a hermetic fake; production wires device.New.
var deviceHandler = func(o device.Options) device.Handler {
	return device.New(o)
}

// newDeviceCmd is the device-control front door: pair, inspect, install,
// launch, observe — the loop that turns xkvm into a complete sideloading
// pipeline (pymobiledevice3's control surface, backed by go-ios).
func newDeviceCmd() *cobra.Command {
	var (
		udid, supervised, supervisedPW string
		timeout                        time.Duration
		showJSON, systemApps, forceYes bool
		envs, appArgs                  []string
	)
	cmd := &cobra.Command{
		Use:   "device",
		Short: "control a connected iPhone/iPad: pair, install, launch, logs",
		Long: `device talks to an iPhone or iPad directly over USB (or WiFi once
paired) — the same protocols pymobiledevice3 uses: lockdownd pairing,
zip-conduit installs, process control, syslog. Together with the inject
command it closes the loop: build the tweaked .ipa, install it, launch it,
watch its logs — no SideStore or Xcode needed.

Subcommands:

  xkvm device list                show connected devices (UDID + transport)
  xkvm device pair                trust a device (tap "Trust" when asked)
  xkvm device info                name, iOS version, model, color, build
  xkvm device battery             battery capacity and charging state
  xkvm device apps                installed user apps (bundle ids)
  xkvm device install App.ipa     install an .ipa via the Xcode zip-conduit
  xkvm device uninstall <bundle>  remove an installed app
  xkvm device launch <bundle>     launch an app, print its pid
  xkvm device kill <pid>          terminate a running process
  xkvm device syslog              stream parsed device logs (Ctrl-C to stop)
  xkvm device restart             reboot the device
  xkvm device shutdown            power the device off

When more than one device is connected, pick one with --udid.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	f := cmd.PersistentFlags()
	f.StringVar(&udid, "udid", "", "target device by UDID (required when more than one is connected; see `list`)")
	f.DurationVar(&timeout, "timeout", 15*time.Second, "per-operation timeout")

	h := func() device.Handler { return deviceHandler(device.Options{Timeout: timeout}) }

	list := &cobra.Command{Use: "list", Short: "show connected devices",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			devs, err := dh.List(cmd.Context())
			if err != nil {
				return err
			}
			devs = device.Distinct(devs)
			return printJSONOr(cmd, showJSON, devs, func() error {
				if len(devs) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "no devices connected")
					return nil
				}
				for _, d := range devs {
					trans := make([]string, 0, len(d.Transports))
					for _, tr := range d.Transports {
						trans = append(trans, string(tr))
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s  %s\n", d.UDID, strings.Join(trans, "+"))
				}
				return nil
			})
		}}
	list.Flags().BoolVar(&showJSON, "json", false, "machine-readable output")

	pair := &cobra.Command{Use: "pair", Short: "trust the device (tap Trust on its screen)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			var sup *device.Supervised
			if supervised != "" {
				p12, err := os.ReadFile(supervised)
				if err != nil {
					return fmt.Errorf("reading --supervised identity: %w", err)
				}
				sup = &device.Supervised{P12: p12, Password: supervisedPW}
				log.Infof("supervised pairing (no Trust tap needed)")
			} else {
				log.Infof("pairing %s — if the device shows a Trust dialog, tap Trust, then run this again", target.UDID)
			}
			if err := dh.Pair(cmd.Context(), target.UDID, sup); err != nil {
				return err
			}
			log.Infof("paired %s", target.UDID)
			return nil
		}}
	pair.Flags().StringVar(&supervised, "supervised", "", "Apple Configurator .p12 identity for tap-free pairing (supervised devices)")
	pair.Flags().StringVar(&supervisedPW, "supervised-password", "", "password for the --supervised .p12")

	info := &cobra.Command{Use: "info", Short: "device name, iOS version, model",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			out, err := dh.Info(cmd.Context(), target.UDID)
			if err != nil {
				return err
			}
			return printJSONOr(cmd, showJSON, out, func() error {
				fmt.Fprintf(cmd.OutOrStdout(), "device:   %s\n", out.Name)
				fmt.Fprintf(cmd.OutOrStdout(), "ios:      %s (%s)\n", out.ProductVersion, out.BuildVersion)
				fmt.Fprintf(cmd.OutOrStdout(), "model:    %s · %s · %s\n", out.ProductType, out.HardwareModel, out.DeviceColor)
				fmt.Fprintf(cmd.OutOrStdout(), "arch:     %s\n", out.CPUArchitecture)
				fmt.Fprintf(cmd.OutOrStdout(), "udid:     %s\n", out.SerialNumber)
				return nil
			})
		}}
	info.Flags().BoolVar(&showJSON, "json", false, "machine-readable output")

	battery := &cobra.Command{Use: "battery", Short: "battery capacity and charging state",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			out, err := dh.Battery(cmd.Context(), target.UDID)
			if err != nil {
				return err
			}
			return printJSONOr(cmd, showJSON, out, func() error {
				state := "not charging"
				if out.IsCharging {
					state = "charging"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "battery: %d%% (%s)\n", out.CurrentCapacity, state)
				return nil
			})
		}}
	battery.Flags().BoolVar(&showJSON, "json", false, "machine-readable output")

	apps := &cobra.Command{Use: "apps", Short: "installed apps and their bundle ids",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			out, err := dh.Apps(cmd.Context(), target.UDID, systemApps)
			if err != nil {
				return err
			}
			return printJSONOr(cmd, showJSON, out, func() error {
				for _, a := range out {
					log.Infof("%s\t%s", a.BundleID, a.Name)
				}
				return nil
			})
		}}
	apps.Flags().BoolVar(&systemApps, "system", false, "list system apps instead of user apps")
	apps.Flags().BoolVar(&showJSON, "json", false, "machine-readable output")

	install := &cobra.Command{Use: "install <app.ipa>", Short: "install an .ipa straight onto the device",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			log.Infof("installing %s on %s (this streams the whole app — sit tight)", args[0], target.UDID)
			if err := dh.Install(cmd.Context(), target.UDID, args[0]); err != nil {
				return err
			}
			log.Infof("installed %s", args[0])
			return nil
		}}

	uninstall := &cobra.Command{Use: "uninstall <bundle-id>", Short: "remove an installed app",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			if err := dh.Uninstall(cmd.Context(), target.UDID, args[0]); err != nil {
				return err
			}
			log.Infof("uninstalled %s", args[0])
			return nil
		}}

	launch := &cobra.Command{Use: "launch <bundle-id>", Short: "launch an app and print its pid",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			env, serr := parseEnv(envs)
			if serr != nil {
				return serr
			}
			pid, err := dh.Launch(cmd.Context(), target.UDID, args[0], env, appArgs)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "launched %s (pid %d)\n", args[0], pid)
			return nil
		}}
	launch.Flags().StringArrayVar(&envs, "env", nil, "launch environment entry K=V (repeatable)")
	launch.Flags().StringArrayVar(&appArgs, "arg", nil, "launch argument (repeatable)")

	kill := &cobra.Command{Use: "kill <pid>", Short: "terminate a running process",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			pid, perr := strconv.ParseUint(args[0], 10, 64)
			if perr != nil {
				return &device.Error{Kind: device.KindUsage, Op: "kill", Err: fmt.Errorf("%q is not a pid", args[0])}
			}
			if err := dh.Kill(cmd.Context(), target.UDID, pid); err != nil {
				return err
			}
			log.Infof("killed pid %d", pid)
			return nil
		}}

	syslogCmd := &cobra.Command{Use: "syslog", Short: "stream parsed device logs (Ctrl-C stops)",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			return dh.Syslog(cmd.Context(), target.UDID, cmd.OutOrStdout())
		}}

	restart := &cobra.Command{Use: "restart", Short: "reboot the device",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			if err := confirmDestructive(cmd, forceYes, "reboot "+target.UDID+"?"); err != nil {
				return err
			}
			if err := dh.Restart(cmd.Context(), target.UDID); err != nil {
				return err
			}
			log.Infof("restarting %s", target.UDID)
			return nil
		}}

	shutdown := &cobra.Command{Use: "shutdown", Short: "power the device off",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dh := h()
			defer dh.Close()
			target, err := resolveUDID(cmd, dh, udid)
			if err != nil {
				return err
			}
			if err := confirmDestructive(cmd, forceYes, "shut down "+target.UDID+"?"); err != nil {
				return err
			}
			if err := dh.Shutdown(cmd.Context(), target.UDID); err != nil {
				return err
			}
			log.Infof("shutting down %s", target.UDID)
			return nil
		}}
	restart.Flags().BoolVar(&forceYes, "yes", false, "skip the confirmation prompt (scripts)")
	shutdown.Flags().BoolVar(&forceYes, "yes", false, "skip the confirmation prompt (scripts)")

	var (
		iosVer     string
		omegaForce bool
	)
	omega := &cobra.Command{
		Use:   "omega [--ios X.Y] [--yes]",
		Short: "clear the app-revoke + certificate blacklists (jailbreak.party Omega)",
		Long: `omega runs the jailbreak.party Omega restore: it replaces the device's
app-revoke and certificate-validity databases with empty directories, so
the system can never write a new revoke into them — revoked or
certificate-banned sideloaded apps start working again, permanently
(until a data wipe).

The iOS version is detected from the connected device; --ios X.Y
overrides. Policy: iOS 16-18 is the proven window (supported), 19-26 gets
a caution (untested — nobody verified Omega there yet), and <16 or >=27 is
a hard block: on iOS 27 the backup system changed and this restore can
reset your data or settings, so xkvm refuses to run it.

Before running: turn OFF Find My (Settings → your name → Find My) and
make a backup. The phone reboots by itself when the restore finishes.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Version policy first: an explicit --ios must hard-block (or
			// gate) without any device attached — it is pure math, and the
			// block exists to protect the user before anything runs.
			verdict := device.Supported
			why := ""
			var verr error
			if iosVer != "" {
				verdict, why, verr = device.OmegaVerdict(iosVer)
				if verr != nil {
					return verr
				}
			}
			dh := h()
			defer dh.Close()
			var (
				target  device.Dev
				version string
				err     error
			)
			if iosVer == "" {
				target, err = resolveUDID(cmd, dh, udid)
				if err != nil {
					return err
				}
				info, ierr := dh.Info(cmd.Context(), target.UDID)
				if ierr != nil {
					return fmt.Errorf("reading the device's iOS version (or set --ios): %w", ierr)
				}
				version = info.ProductVersion
				verdict, why, verr = device.OmegaVerdict(version)
				if verr != nil {
					return verr
				}
			} else {
				version = iosVer
			}
			fmt.Fprintf(cmd.OutOrStdout(), "device ios: %s — omega on this version is %s\n", version, verdict)
			switch verdict {
			case device.Unsupported:
				return &device.Error{Kind: device.KindUnsupported, Op: "omega", Err: fmt.Errorf("%s: %s", version, why),
					Remediation: "hard block — xkvm will not run the Omega restore on this iOS version.\n  This is to protect your data: " + why}
			case device.Untested:
				fmt.Fprintln(cmd.OutOrStdout(), "[caution] this iOS version is outside the range Omega was proven on.")
				fmt.Fprintln(cmd.OutOrStdout(), "  "+why)
				fmt.Fprintln(cmd.OutOrStdout(), "  back up first. the restore replaces those databases and reboots the phone.")
			case device.Supported:
				fmt.Fprintln(cmd.OutOrStdout(), "[reminder] turn Find My OFF and make a backup before continuing.")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "the restore replaces the revoke + certificate databases — your sideloaded apps stay, the blacklists go")
			if !omegaForce && !gateContinue(cmd) {
				return &device.Error{Kind: device.KindUsage, Op: "omega",
					Err:         fmt.Errorf("not confirmed"),
					Remediation: "type CONTINUE to proceed (or --yes for scripts)"}
			}
			fmt.Fprintln(cmd.OutOrStdout(), "restoring… the phone reboots by itself when done (takes a few minutes)")
			last := float64(-25)
			if err := dh.OmegaRestore(cmd.Context(), target.UDID, func(pct float64) {
				if pct-last >= 25 {
					fmt.Fprintf(cmd.OutOrStdout(), "restore progress: %.0f%%\n", pct)
					last = pct
				}
			}); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "[ok] omega restore complete — the device reboots now: revoked apps and banned certificates are forgotten for good")
			return nil
		},
	}
	omega.Flags().StringVar(&iosVer, "ios", "", "iOS version override (X.Y) — normally detected from the device")
	omega.Flags().BoolVar(&omegaForce, "yes", false, "skip the CONTINUE confirmation (scripts)")

	cmd.AddCommand(list, pair, info, battery, apps, install, uninstall, launch, kill, syslogCmd, restart, shutdown, omega)
	return cmd
}

// resolveUDID runs the no-guess device policy (explicit UDID wins, else
// exactly one device) and returns the chosen one.
func resolveUDID(cmd *cobra.Command, dh device.Handler, udid string) (device.Dev, error) {
	d, err := device.Resolve(cmd.Context(), dh, udid)
	if err != nil {
		return device.Dev{}, err
	}
	return d, nil
}

// printJSONOr writes v as indented JSON when jsonMode, otherwise fn().
func printJSONOr(cmd *cobra.Command, jsonMode bool, v any, fn func() error) error {
	if !jsonMode {
		return fn()
	}
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// parseEnv turns "K=V" strings into a map, rejecting malformed entries.
func parseEnv(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("--env value %q is not K=V", e)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("--env key %q given twice", k)
		}
		out[k] = v
	}
	return out, nil
}

// confirmDestructive gates reboot/shutdown: --yes for scripts, an explicit
// y confirm on a terminal, and a hard refusal otherwise — a destructive
// action never runs by default.
func confirmDestructive(cmd *cobra.Command, yes bool, prompt string) error {
	if yes {
		return nil
	}
	reader := bufio.NewReader(os.Stdin)
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", prompt)
	line, err := reader.ReadString('\n')
	if err != nil {
		return &device.Error{Kind: device.KindUsage, Op: "confirm",
			Err: errors.New("no answer on stdin"), Remediation: "pass --yes to run non-interactively"}
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return nil
	default:
		return &device.Error{Kind: device.KindUsage, Op: "confirm", Err: errors.New("not confirmed — nothing was changed")}
	}
}
