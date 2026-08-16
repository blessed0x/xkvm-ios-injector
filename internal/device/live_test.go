//go:build live

// Live device tests run against a real, physically connected iPhone.
//
//	export XKVM_DEVICE_UDID=<udid>      (optional; skips if unset)
//	go test -tags live -v -run Live ./internal/device/
//
// These are deliberately NOT part of CI: they need hardware + trust. They
// are the user-side verification path the hermetic suite cannot replace.
package device

import (
	"context"
	"os"
	"testing"
	"time"
)

func liveHandler(t *testing.T) (*GoIOS, string) {
	t.Helper()
	udid := os.Getenv("XKVM_DEVICE_UDID")
	if udid == "" {
		t.Skip("XKVM_DEVICE_UDID not set — plug in a device and set it to run live tests")
	}
	return New(Options{Timeout: 30 * time.Second}), udid
}

func TestLiveListAndInfo(t *testing.T) {
	h, udid := liveHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	devs, err := h.List(ctx)
	if err != nil {
		t.Fatalf("List: %v (is usbmuxd running?)", err)
	}
	found := false
	for _, d := range devs {
		if d.UDID == udid {
			found = true
		}
	}
	if !found {
		t.Fatalf("device %s not in list: %+v", udid, devs)
	}
	info, err := h.Info(ctx, udid)
	if err != nil {
		t.Fatalf("Info: %v (unlock the device / re-trust this computer?)", err)
	}
	if info.Name == "" || info.ProductVersion == "" {
		t.Errorf("info looks empty: %+v", info)
	}
	t.Logf("live device: %s, iOS %s (%s), %s", info.Name, info.ProductVersion, info.BuildVersion, info.ProductType)
}

func TestLiveAppsAndBattery(t *testing.T) {
	h, udid := liveHandler(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	apps, err := h.Apps(ctx, udid, false)
	if err != nil {
		t.Fatalf("Apps: %v", err)
	}
	t.Logf("live user apps: %d", len(apps))
	batt, err := h.Battery(ctx, udid)
	if err != nil {
		t.Fatalf("Battery: %v", err)
	}
	if batt.CurrentCapacity > 100 {
		t.Errorf("capacity out of range: %d", batt.CurrentCapacity)
	}
}
