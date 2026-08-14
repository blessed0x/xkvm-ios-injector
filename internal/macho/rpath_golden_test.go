package macho

// Byte-identical regression test for the LC_RPATH serializer fix
// (serializeTOC writing rpaths with Dylib.Write-style pad-to-own-Len
// semantics instead of go-macho's Rpath.Write absolute-position padding).
//
// The bug: Rpath.Write pads to the ABSOLUTE buffer position's 8-byte
// boundary (buf.Len()%8) while the command's declared cmdsize is
// self-aligned (LoadSize). On a 32-bit slice whose preceding load commands
// don't total a multiple of 8 — i.e. sizeofcmds % 8 == 4 — the rpath
// command physically occupies more bytes than it declares (28 written vs
// 24 declared for "/usr/lib"), desyncing every command after it. Found
// converting Shadow's PreferenceBundle: the second LC_RPATH was read as
// cmd=0, cmdsize=0x8000001c ("invalid command block size at byte 0xa28").
//
// This test builds a THIN armv7 dylib (iPhoneOS SDK — the macOS SDK
// dropped armv7) whose natural load-command region is verified to be
// 4-mod-8, adds two rpaths through the production Bin.AddRpath (the exact
// sequence that broke ShadowSettings.bundle), and pins the result
// byte-for-byte: the two rpath commands must serialize to exactly the
// self-aligned golden bytes, otool must read them back with those sizes,
// and a raw walk of the slice must end flush at sizeofcmds.

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// rpathGolden24 is the exact on-disk bytes of an LC_RPATH for "/usr/lib":
//
//	cmd=0x8000001c (LC_RPATH)  cmdsize=24  pathoffset=12  "/usr/lib\0" + 3 pad
func rpathGolden24() []byte {
	g := []byte{
		0x1c, 0x00, 0x00, 0x80, // cmd: LC_RPATH
		0x18, 0x00, 0x00, 0x00, // cmdsize: 24
		0x0c, 0x00, 0x00, 0x00, // path offset: 12
	}
	g = append(g, []byte("/usr/lib\x00")...)
	for len(g) < 24 {
		g = append(g, 0x00)
	}
	return g
}

// rpathGolden32 is the exact on-disk bytes of an LC_RPATH for
// "/var/jb/usr/lib": cmd=0x8000001c  cmdsize=32  pathoffset=12  path + pad.
func rpathGolden32() []byte {
	g := []byte{
		0x1c, 0x00, 0x00, 0x80, // cmd: LC_RPATH
		0x20, 0x00, 0x00, 0x00, // cmdsize: 32
		0x0c, 0x00, 0x00, 0x00, // path offset: 12
	}
	g = append(g, []byte("/var/jb/usr/lib\x00")...)
	for len(g) < 32 {
		g = append(g, 0x00)
	}
	return g
}

// TestRpathByteIdentical32 is the byte-identical pin. A thin armv7 fixture
// whose pre-rpath command region is 4-mod-8 goes through AddRpath twice; the
// output must contain the two rpath commands at exactly the golden bytes,
// readable back by otool with exactly those sizes, and the command walk must
// end flush at sizeofcmds.
func TestRpathByteIdentical32(t *testing.T) {
	skipNoToolchain(t)
	tmp := t.TempDir()
	path := buildSlice(t, tmp, "Rpath32Tweak", "armv7")

	// Precondition: the fixture's load-command region must total 4 mod 8 so
	// the absolute-padding bug would actually fire (a 0-mod-8 region would
	// pass even with the buggy writer). clang's armv7 output naturally lands
	// here (the LC_ID_DYLIB install name length makes sizeofcmds ≡ 4); the
	// assertion documents that this fixture genuinely exercises the bug.
	pre := sliceHeader(t, path)
	if pre.sizeofcmds%8 != 4 {
		t.Fatalf("fixture sizeofcmds = %d (mod 8 = %d); the rpath drift needs a 4-mod-8 region — pick an install name length that lands there", pre.sizeofcmds, pre.sizeofcmds%8)
	}

	b := Bin{Path: path}
	if err := b.AddRpath("/usr/lib"); err != nil {
		t.Fatalf("AddRpath(/usr/lib): %v", err)
	}
	if err := b.AddRpath("/var/jb/usr/lib"); err != nil {
		t.Fatalf("AddRpath(/var/jb/usr/lib): %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Thin armv7: header at offset 0. The fixture starts with some number of
	// commands; the two AddRpath calls must add exactly two.
	hdr := sliceHeader(t, path)
	if hdr.ncmds != pre.ncmds+2 {
		t.Fatalf("ncmds = %d, want %d (fixture's %d + 2 rpaths)", hdr.ncmds, pre.ncmds+2, pre.ncmds)
	}

	// Walk the slice; capture the two trailing LC_RPATH commands' raw bytes.
	bo := binary.LittleEndian
	p := 28
	var rpaths [][]byte
	for i := 0; i < int(hdr.ncmds); i++ {
		if p+8 > len(data) {
			t.Fatalf("walk ran off the slice at byte %#x", p)
		}
		cmd := bo.Uint32(data[p : p+4])
		sz := bo.Uint32(data[p+4 : p+8])
		if sz < 8 || p+int(sz) > len(data) {
			t.Fatalf("cmd[%d] @%#x: cmd=%#x size=%d — desynced", i, p, cmd, sz)
		}
		if cmd == 0x8000001c { // LC_RPATH
			rpaths = append(rpaths, data[p:p+int(sz)])
		}
		p += int(sz)
	}
	if p != 28+int(hdr.sizeofcmds) {
		t.Fatalf("walk ends at %#x but sizeofcmds declares %#x — not flush", p, 28+int(hdr.sizeofcmds))
	}
	if len(rpaths) != 2 {
		t.Fatalf("found %d LC_RPATH commands, want 2", len(rpaths))
	}

	// Byte-identical: each rpath's raw bytes must equal the golden layout.
	if !bytes.Equal(rpaths[0], rpathGolden24()) {
		t.Errorf("first LC_RPATH bytes:\n got %x\nwant %x", rpaths[0], rpathGolden24())
	}
	if !bytes.Equal(rpaths[1], rpathGolden32()) {
		t.Errorf("second LC_RPATH bytes:\n got %x\nwant %x", rpaths[1], rpathGolden32())
	}

	// otool is the independent oracle: it must read back exactly the two
	// rpaths with the golden sizes, in order, at their golden offsets.
	out, err := exec.Command("otool", "-arch", "armv7", "-l", path).CombinedOutput()
	if err != nil {
		t.Fatalf("otool: %v: %s", err, out)
	}
	lines := string(out)
	if !strings.Contains(lines, "path /usr/lib (offset 12)") {
		t.Errorf("otool missing /usr/lib rpath; output:\n%s", lines)
	}
	if !strings.Contains(lines, "path /var/jb/usr/lib (offset 12)") {
		t.Errorf("otool missing /var/jb/usr/lib rpath; output:\n%s", lines)
	}
	// cmdsize lines must be 24 and 32 (in that order), not the 2147483676
	// garbage the desynced layout produced before the fix. Parse each
	// LC_RPATH block (its cmdsize line + its path line) so other commands'
	// 24-byte sizes don't confuse the order check.
	var rpathSizes []string
	blocks := strings.Split(lines, "\n")
	for i, l := range blocks {
		if !strings.Contains(l, "LC_RPATH") {
			continue
		}
		// In otool -l output the cmdsize line follows the cmd line.
		for j := i + 1; j < len(blocks) && j <= i+2; j++ {
			if strings.Contains(blocks[j], "cmdsize") {
				rpathSizes = append(rpathSizes, strings.TrimSpace(blocks[j]))
				break
			}
		}
	}
	if len(rpathSizes) != 2 {
		t.Fatalf("otool sees %d LC_RPATH blocks, want 2:\n%s", len(rpathSizes), lines)
	}
	if rpathSizes[0] != "cmdsize 24" || rpathSizes[1] != "cmdsize 32" {
		t.Errorf("otool rpath cmdsizes = %v, want [cmdsize 24 cmdsize 32] (in order)", rpathSizes)
	}
	if strings.Contains(lines, "2147483676") {
		t.Errorf("otool still sees the desynced 0x8000001c garbage cmdsize:\n%s", lines)
	}
}

// sliceHdr is the parsed 32-bit header of a thin Mach-O.
type sliceHdr struct {
	ncmds      uint32
	sizeofcmds uint32
}

func sliceHeader(t *testing.T, path string) sliceHdr {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bo := binary.LittleEndian
	return sliceHdr{
		ncmds:      bo.Uint32(data[16:20]),
		sizeofcmds: bo.Uint32(data[20:24]),
	}
}
