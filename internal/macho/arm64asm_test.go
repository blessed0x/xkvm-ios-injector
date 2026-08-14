package macho

import (
	"encoding/binary"
	"testing"
)

// TestAdrpRoundTrip: encodeADRP then adrpValue must recover the target page.
func TestAdrpRoundTrip(t *testing.T) {
	cases := []struct {
		pc     uint64
		target uint64
	}{
		{0x100000400, 0x100001234}, // same-page-ish delta
		{0x100000400, 0x10005234},  // +0x5000 (5 pages)
		{0x100000400, 0x1C0000400}, // +0xC0000 pages (~3GB, in 21-bit range)
		{0x100000400, 0x40000400},  // -0xC0000 pages (negative, in range)
		{0x100000000, 0x100000000}, // identity
	}
	for _, c := range cases {
		delta := int64(c.target>>12) - int64(c.pc>>12)
		insn := encodeADRP(5, delta)
		if insn&adrpMask != adrpOpcode {
			t.Fatalf("encodeADRP produced non-ADRP %#x", insn)
		}
		got := adrpValue(insn, c.pc)
		if want := c.target &^ pageOffsetMask; got != want {
			t.Errorf("adrpValue(pc=%#x target=%#x) = %#x, want %#x", c.pc, c.target, got, want)
		}
		// The register field must survive.
		if int(insn&regMask) != 5 {
			t.Errorf("ADRP register lost: %#x", insn)
		}
	}
}

// TestAdrRoundTrip: encodeADR then adrValue must recover the target.
func TestAdrRoundTrip(t *testing.T) {
	for _, off := range []int64{0, 4, -4, 0x7FFFC, -0x80000, 12345, -99999} {
		pc := uint64(0x100000400)
		insn := encodeADR(9, off)
		if insn&adrMask != adrOpcode {
			t.Fatalf("encodeADR produced non-ADR %#x", insn)
		}
		got := adrValue(insn, pc)
		if want := int64(pc) + off; got != uint64(want) {
			t.Errorf("adrValue(off=%d) = %#x, want %#x", off, got, want)
		}
	}
}

// TestAddFieldsRoundTrip: encodeADD then addFields must recover rd/rn/imm/shift.
func TestAddFieldsRoundTrip(t *testing.T) {
	for _, c := range []struct {
		rd, rn  int
		imm     uint32
		shift12 bool
	}{
		{0, 0, 0x123, false},
		{3, 5, 0xFFF, true},
		{21, 7, 0, false},
		{1, 1, 0xABC, true},
	} {
		insn := encodeADD(c.rd, c.rn, c.imm, c.shift12)
		if insn&addMask != addOpcode {
			t.Fatalf("encodeADD produced non-ADD %#x", insn)
		}
		rd, rn, imm, shift := addFields(insn)
		if rd != c.rd || rn != c.rn || imm != c.imm {
			t.Errorf("addFields(%#x) = %d/%d/%#x, want %d/%d/%#x", insn, rd, rn, imm, c.rd, c.rn, c.imm)
		}
		wantShift := uint32(0)
		if c.shift12 {
			wantShift = 1
		}
		if shift != wantShift {
			t.Errorf("addFields(%#x) shift=%d, want %d", insn, shift, wantShift)
		}
	}
}

// TestMovRoundTrip: encodeMOV/movFields.
func TestMovRoundTrip(t *testing.T) {
	insn := encodeMOV(7, 0x2A)
	if insn&movMask != movOpcode {
		t.Fatalf("encodeMOV produced non-MOV %#x", insn)
	}
	rd, imm := movFields(insn)
	if rd != 7 || imm != 0x2A {
		t.Errorf("movFields = %d/%#x, want 7/0x2a", rd, imm)
	}
}

// TestInstructionsAreLittleEndian ensures the helpers match the on-disk byte
// order used by the patcher (binary.LittleEndian.Uint32 over the code).
func TestInstructionsAreLittleEndian(t *testing.T) {
	insn := encodeADRP(3, 5)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, insn)
	if got := binary.LittleEndian.Uint32(b); got != insn {
		t.Fatalf("byte order mismatch")
	}
}

// TestPatchSwiftLengthOffset pins the Swift-length MOVZ offset: it sits 16
// bytes (4 instructions) after the ADR's own position — NOT after the
// fallback's ADD. Upstream passes i in both branches; passing i+1 would
// patch the wrong instruction. This is the regression test for that bug.
func TestPatchSwiftLengthOffset(t *testing.T) {
	// A synthetic instruction stream: at i=0 an ADR, then a NOP at +4 (the
	// fallback writes ADD there), and the Swift MOVZ at +16.
	code := make([]byte, 32)
	// ADR at 0 (target irrelevant for this test).
	binary.LittleEndian.PutUint32(code[0:], encodeADR(0, 0x100))
	// NOP at +4 — the fallback's ADD slot.
	binary.LittleEndian.PutUint32(code[4:], nopInsn)
	// The Swift hardcoded length MOVZ at +16, holding the OLD length.
	binary.LittleEndian.PutUint32(code[16:], encodeMOV(9, 5))

	rel := relocatedString{oldLen: 5, newLen: 12}

	// Correct call (as the ADR branch and the ADRP+NOP fallback both do):
	// position i=0, so the MOVZ at 0+16 is the patch target.
	patchSwiftLength(code, 0, rel)
	rd, imm := movFields(binary.LittleEndian.Uint32(code[16:]))
	if rd != 9 || imm != 12 {
		t.Errorf("patchSwiftLength(0) -> rd=%d imm=%d, want rd=9 imm=12", rd, imm)
	}

	// The ADD slot must be untouched (it is an ADD/NOP, not a MOVZ).
	if got := binary.LittleEndian.Uint32(code[4:]); got != nopInsn {
		t.Errorf("ADD slot corrupted by patchSwiftLength: %#x", got)
	}

	// The old buggy call (position i+1 = 4) must NOT have hit the MOVZ.
	// Re-arm with the old length and prove the wrong offset misses.
	binary.LittleEndian.PutUint32(code[16:], encodeMOV(9, 5))
	patchSwiftLength(code, 4, rel)
	_, imm = movFields(binary.LittleEndian.Uint32(code[16:]))
	if imm != 5 {
		t.Errorf("patchSwiftLength(4) unexpectedly patched MOVZ to imm=%d (offset bug)", imm)
	}

	// Old length mismatch must be left alone.
	binary.LittleEndian.PutUint32(code[16:], encodeMOV(9, 99))
	patchSwiftLength(code, 0, rel)
	rd, imm = movFields(binary.LittleEndian.Uint32(code[16:]))
	if rd != 9 || imm != 99 {
		t.Errorf("length-mismatch MOVZ was patched: rd=%d imm=%d", rd, imm)
	}
}
