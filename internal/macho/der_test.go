package macho

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// TestEntitlementsDERAppleFormat pins the exact bytes codesign produces for a
// get-task-allow entitlements plist. Regression: parseDictBody used to return
// on the first </key>, so every DER came out as the empty sequence "3000" and
// codesign's DR evaluation failed on the resulting signature.
func TestEntitlementsDERAppleFormat(t *testing.T) {
	ents := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>get-task-allow</key><true/></dict></plist>`)
	der, err := entitlementsDER(ents)
	if err != nil {
		t.Fatalf("entitlementsDER: %v", err)
	}
	// Captured from codesign -s - --entitlements on macOS (XNU der_plist).
	const want = "701a020101b01530130c0e6765742d7461736b2d616c6c6f770101ff"
	if hex.EncodeToString(der) != want {
		t.Fatalf("DER = %x, want %s", der, want)
	}
}

// TestEntitlementsDERMultiKey ensures every </key> in the dict is consumed
// (not just the first) so all pairs survive into the DER.
func TestEntitlementsDERMultiKey(t *testing.T) {
	// Keys chosen so none is a substring of another: the position check
	// below must not be satisfiable by accident.
	ents := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>zeta</key><true/>
<key>alpha</key><true/>
<key>get-task-allow</key><false/>
</dict></plist>`)
	der, err := entitlementsDER(ents)
	if err != nil {
		t.Fatalf("entitlementsDER: %v", err)
	}
	if len(der) < 10 {
		t.Fatalf("DER too short for 3 keys: %x", der)
	}
	// Apple-plist wrapper: 0x70 tag, version INTEGER 01 01, 0xb0 SET.
	if der[0] != 0x70 || !hasBytes(der, []byte{0x02, 0x01, 0x01}) || !hasBytes(der, []byte{0xb0}) {
		t.Fatalf("DER missing Apple-plist structure: %x", der)
	}
	// All three keys must be present.
	for _, k := range []string{"zeta", "alpha", "get-task-allow"} {
		if !hasBytes(der, []byte(k)) {
			t.Errorf("DER missing key %q: %x", k, der)
		}
	}
	// DER SET OF requires canonical order: keys sorted lexicographically
	// regardless of document order (input above is zeta/alpha/…).
	pos := func(k string) int { return bytes.Index(der, []byte(k)) }
	ordered := []string{"alpha", "get-task-allow", "zeta"}
	for i := 0; i+1 < len(ordered); i++ {
		if pos(ordered[i]) < 0 || pos(ordered[i]) > pos(ordered[i+1]) {
			t.Errorf("DER keys not in canonical sorted order: %x", der)
			break
		}
	}
}

// TestEntitlementsDEREmptyDict produces a structurally valid (if empty)
// Apple-plist DER rather than erroring.
func TestEntitlementsDEREmptyDict(t *testing.T) {
	ents := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict/></plist>`)
	der, err := entitlementsDER(ents)
	if err != nil {
		t.Fatalf("entitlementsDER: %v", err)
	}
	if der[0] != 0x70 {
		t.Fatalf("expected Apple-plist 0x70 wrapper, got %x", der)
	}
}

// TestEntitlementsDERNoDict: a plist with no <dict> at all must still
// produce a valid empty Apple-plist DER rather than erroring.
func TestEntitlementsDERNoDict(t *testing.T) {
	ents := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"/>`)
	der, err := entitlementsDER(ents)
	if err != nil {
		t.Fatalf("entitlementsDER: %v", err)
	}
	if der[0] != 0x70 {
		t.Fatalf("expected Apple-plist 0x70 wrapper, got %x", der)
	}
}

func hasBytes(hay, needle []byte) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
