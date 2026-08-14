package macho

import (
	"testing"

	"github.com/xscope0/xkvm-ios-injector/internal/testutil"
)

// TestChangeDependencyGrowingRename is a regression test for a latent
// serializeTOC bug: an edit that GROWS a load command (e.g. renaming a dylib
// dependency to a longer path) used to leave the header's sizeofcmds stale,
// which broke re-parsing with "invalid command block size". All earlier
// renames shrank the command, so the bug never surfaced.
func TestChangeDependencyGrowingRename(t *testing.T) {
	testutil.SkipUnlessNativeToolchain(t)
	tmp := t.TempDir()
	tweak := testutil.MakeTweak(t, tmp, "HookerTweak")
	b := Bin{Path: tweak}
	if err := b.RemoveSignature(); err != nil {
		t.Fatalf("RemoveSignature: %v", err)
	}
	if err := b.InjectWeak("/usr/lib/libhooker.dylib"); err != nil {
		t.Fatalf("InjectWeak: %v", err)
	}
	// "/usr/lib/libhooker.dylib" (25 chars) -> "@rpath/ElleKit.framework/ElleKit"
	// (29 chars): the Dylib command grows 56 -> 64 bytes.
	if err := b.ChangeDependency("/usr/lib/libhooker.dylib", "@rpath/ElleKit.framework/ElleKit"); err != nil {
		t.Fatalf("ChangeDependency (growing): %v", err)
	}
	deps, err := b.Dependencies()
	if err != nil {
		t.Fatalf("re-parse after growing rename: %v", err)
	}
	want := "@rpath/ElleKit.framework/ElleKit"
	found := false
	for _, d := range deps {
		if d == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("dependency %q not found after rename; got %v", want, deps)
	}
}
