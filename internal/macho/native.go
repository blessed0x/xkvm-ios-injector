package macho

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/pkg/codesign"
	ctypes "github.com/blacktop/go-macho/pkg/codesign/types"
	"github.com/blacktop/go-macho/types"
)

// This file is the M3 milestone: pure-Go replacements for every vendored
// toolchain call, using blacktop/go-macho (parse/modify/rebuild) and its
// pkg/codesign (ad-hoc SuperBlob / CodeDirectory / entitlements). The same
// code runs on any GOOS/GOARCH — no embedded binaries, no external tools.

// nativeDepStarters mirrors cyan's get_dependencies filter (the same path
// prefixes, minus the leading tab that otool -L output carries).
var nativeDepStarters = []string{"/Library/", "/usr/lib/", "@"}

func isFatData(data []byte) bool {
	return len(data) >= 4 && binary.BigEndian.Uint32(data[:4]) == uint32(types.MagicFat)
}

// readFirst opens the first architecture slice (thin input, or the first arm
// of a fat input) for read-only inspection — parity with otool, which reads
// the first slice of fat binaries.
func readFirst(path string) (*macho.File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if isFatData(data) {
		ff, err := macho.NewFatFile(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer ff.Close()
		if len(ff.Arches) == 0 {
			return nil, fmt.Errorf("%s: fat binary has no architectures", path)
		}
		return ff.Arches[0].File, nil
	}
	return macho.NewFile(bytes.NewReader(data))
}

// editMachO applies fn to every architecture slice of path and atomically
// rewrites the file (temp + rename). fn mutates the File in memory and
// returns the rebuilt bytes for that slice (see nativeWrite — go-macho's own
// SaveBuffer is unusable here: it concatenates segment data without updating
// the load-command offsets, and it cannot read back __DWARF segments that
// carry Memsz=0 as go-built binaries emit).
func editMachO(path string, fn func(f *macho.File, orig []byte) ([]byte, error)) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var out []byte
	if isFatData(data) {
		ff, err := macho.NewFatFile(bytes.NewReader(data))
		if err != nil {
			return err
		}
		defer ff.Close()
		slices := make([][]byte, len(ff.Arches))
		for i, arch := range ff.Arches {
			orig := data[arch.Offset : arch.Offset+arch.Size]
			slices[i], err = fn(arch.File, orig)
			if err != nil {
				return err
			}
		}
		out, err = buildFat(ff, slices)
		if err != nil {
			return err
		}
	} else {
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return err
		}
		out, err = fn(f, data)
		if err != nil {
			return err
		}
	}
	return atomicWrite(path, out)
}

// serializeTOC writes the file header plus every load command (including
// per-segment section commands) to buf, exactly as go-macho's unexported
// writeLoadCommands does. This is the first piece of nativeWrite.
//
// The load commands are serialized into a scratch buffer first so the header
// can carry the true NCommands/SizeCommands: go-macho's FileHeader.Write
// emits the stored fields verbatim, and an edit that grows a command (e.g. a
// longer dylib install name) would otherwise leave a stale sizeofcmds that
// breaks re-parsing with "invalid command block size".
func serializeTOC(f *macho.File, buf *bytes.Buffer) error {
	var cmds bytes.Buffer
	for _, l := range f.Loads {
		switch l.Command() {
		case types.LC_SEGMENT, types.LC_SEGMENT_64:
			seg := l.(*macho.Segment)
			if err := seg.Write(&cmds, f.ByteOrder); err != nil {
				return fmt.Errorf("failed to write segment %s: %v", seg.Name, err)
			}
			for i := uint32(0); i < seg.Nsect; i++ {
				if err := f.Sections[i+seg.Firstsect].Write(&cmds, f.ByteOrder); err != nil {
					return fmt.Errorf("failed to write section: %v", err)
				}
			}
		default:
			// Rpath.Write pads to the ABSOLUTE buffer position's 8-byte
			// boundary (buf.Len()%8), but the command's declared cmdsize is
			// self-aligned (LoadSize). On a 32-bit slice whose preceding
			// commands don't total a multiple of 8 the command physically
			// occupies more bytes than it declares, desyncing every command
			// after it (the second LC_RPATH read as cmd=0, cmdsize=garbage).
			// Dylib.Write pads to its own d.Len; replicate that here for
			// rpaths (identical self-aligned semantics).
			if r, ok := l.(*macho.Rpath); ok {
				start := cmds.Len()
				if err := binary.Write(&cmds, f.ByteOrder, r.RpathCmd); err != nil {
					return fmt.Errorf("failed to write rpath: %v", err)
				}
				if _, err := cmds.WriteString(r.Path + "\x00"); err != nil {
					return fmt.Errorf("failed to write rpath path: %v", err)
				}
				written := cmds.Len() - start
				if int(r.Len) < written {
					return fmt.Errorf("rpath cmdsize %d is smaller than the %d bytes written; recalc via LoadSize after changing Path", r.Len, written)
				}
				if pad := int(r.Len) - written; pad > 0 {
					cmds.Write(make([]byte, pad))
				}
				break
			}
			if err := l.Write(&cmds, f.ByteOrder); err != nil {
				return fmt.Errorf("failed to write load command %s: %v", l.Command(), err)
			}
		}
	}
	f.NCommands = uint32(len(f.Loads))
	f.SizeCommands = uint32(cmds.Len())
	var hdr bytes.Buffer
	if err := f.FileHeader.Write(&hdr, f.ByteOrder); err != nil {
		return fmt.Errorf("failed to write file header: %v", err)
	}
	h := hdr.Bytes()
	// go-macho's FileHeader.Write always emits the 8-field struct (32 bytes),
	// but a 32-bit Mach-O header is 7 fields (28 bytes, no Reserved). The
	// 4-byte overrun shifts every load command and breaks re-parsing with
	// "invalid command block size in record at byte 0x1c" (the stale Reserved
	// field is read as cmd=1, cmdsize=1). FileHeader.Put knows the rule
	// (returns 28 for Magic32); mirror it here.
	if f.Magic == types.Magic32 {
		h = h[:28]
	}
	buf.Write(h)
	buf.Write(cmds.Bytes())
	return nil
}

// firstDataOffset returns the file offset of the first byte that is real
// segment/section content (as opposed to the header+load-command region).
// New load commands may consume slack only up to this offset.
func firstDataOffset(f *macho.File) int {
	minOff := 1 << 30
	for _, s := range f.Sections {
		if s.Offset != 0 && int(s.Offset) < minOff {
			minOff = int(s.Offset)
		}
	}
	for _, seg := range f.Segments() {
		if seg.Offset > 0 && int(seg.Offset) < minOff && seg.Filesz > 0 {
			minOff = int(seg.Offset)
		}
	}
	return minOff
}

// nativeWrite rebuilds the Mach-O. The new TOC (header + load commands)
// replaces the original load-command region; everything after it is copied
// verbatim from the original bytes, preserving every segment at its original
// file offset (gaps and alignment included). When ledata is non-nil (a fresh
// signature was computed), the __LINKEDIT region is taken from it instead.
func nativeWrite(f *macho.File, orig []byte, ledata []byte) ([]byte, error) {
	var toc bytes.Buffer
	if err := serializeTOC(f, &toc); err != nil {
		return nil, err
	}
	if first := firstDataOffset(f); toc.Len() > first {
		return nil, fmt.Errorf("not enough space for load commands (%d > %d)", toc.Len(), first)
	}

	le := f.Segment("__LINKEDIT")
	out := make([]byte, 0, len(orig))
	out = append(out, toc.Bytes()...)
	if le == nil {
		out = append(out, orig[toc.Len():]...)
		return out, nil
	}
	leStart := int(le.Offset)
	if leStart > toc.Len() {
		out = append(out, orig[toc.Len():leStart]...)
	}
	if ledata != nil {
		out = append(out, ledata...)
	} else {
		end := leStart + int(le.Filesz)
		if end > len(orig) {
			end = len(orig)
		}
		out = append(out, orig[leStart:end]...)
	}
	return out, nil
}

// nativePageSize mirrors go-macho's unexported File.pageSize: 16K pages for
// arm64/arm64_32 binaries, 4K otherwise. Used for __LINKEDIT growth while
// signing (matching what f.CodeSign does).
func nativePageSize(f *macho.File) uint64 {
	switch f.CPU {
	case types.CPUArm64, types.CPUArm6432:
		return 0x4000
	default:
		return 0x1000
	}
}

// pointerAlign rounds sz up to an 8-byte boundary (mirrors go-macho's
// internal pointerAlign used for fresh code-signature placement).
func pointerAlign(sz uint32) uint32 {
	if sz%8 != 0 {
		sz += 8 - (sz % 8)
	}
	return sz
}

// pageAlign rounds off up to the next multiple of align (mirrors go-macho's
// internal pageAlign used to grow __LINKEDIT while signing).
func pageAlign(off, align uint64) uint64 {
	if off%align != 0 {
		off += align - (off % align)
	}
	return off
}

// guardLoadCommands rejects edits whose header+load-command region would
// overlap section data — SaveBuffer cannot move section content, and a grown
// command region must fit in the space before the first section (the same
// constraint insert_dylib/install_name_tool enforce with "not enough space").
func guardLoadCommands(f *macho.File) error {
	toc := f.TOCSize()
	for _, s := range f.Sections {
		if s.Offset != 0 && s.Offset < toc {
			return fmt.Errorf("not enough space for load commands (%d bytes needed, first section data at %#x)", toc, s.Offset)
		}
	}
	if text := f.Segment("__TEXT"); text != nil && text.Offset == 0 && uint64(toc) > text.Filesz {
		return fmt.Errorf("not enough space for load commands in __TEXT (%d > %d)", toc, text.Filesz)
	}
	return nil
}

// buildFat reassembles a universal binary from its edited per-arch slices,
// keeping each arch's original alignment. Always written big-endian.
func buildFat(ff *macho.FatFile, slices [][]byte) ([]byte, error) {
	n := len(slices)
	const archHdrSize = 20 // 5 * uint32
	offset := uint32(8 + n*archHdrSize)
	offsets := make([]uint32, n)
	for i, arch := range ff.Arches {
		align := arch.Align
		if align == 0 {
			align = 14 // 16 KB, lipo's default
		}
		aligned := uint32(1) << align
		offset = (offset + aligned - 1) &^ (aligned - 1)
		offsets[i] = offset
		offset += uint32(len(slices[i]))
	}
	var out bytes.Buffer
	writeU32 := func(v uint32) { _ = binary.Write(&out, binary.BigEndian, v) }
	writeU32(uint32(types.MagicFat))
	writeU32(uint32(n))
	for i, arch := range ff.Arches {
		h := arch.FatArchHeader
		writeU32(uint32(h.CPU))
		writeU32(uint32(h.SubCPU))
		writeU32(offsets[i])
		writeU32(uint32(len(slices[i])))
		writeU32(h.Align)
	}
	for i, s := range slices {
		if pad := int(offsets[i]) - out.Len(); pad > 0 {
			out.Write(make([]byte, pad))
		}
		out.Write(s)
	}
	return out.Bytes(), nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".xkvm-native-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func cpuArchName(cpu types.CPU, sub types.CPUSubtype) string {
	switch cpu {
	case types.CPUAmd64:
		return "x86_64"
	case types.CPUArm64:
		if sub != 0 && sub&0x0f000000 != 0 {
			return "arm64e"
		}
		return "arm64"
	case types.CPUArm6432:
		return "arm64_32"
	case types.CPUArm:
		return "armv7"
	case types.CPUI386:
		return "i386"
	default:
		return fmt.Sprintf("cpu_%d", cpu)
	}
}

// --- read ops ---------------------------------------------------------------

func nativeIsEncrypted(path string) (bool, error) {
	f, err := readFirst(path)
	if err != nil {
		// otool -l exits 0 with "is not an object file" on non-Mach-O
		// input, so the toolchain backend reports these as not-encrypted
		// rather than erroring. Match that so a placeholder main binary
		// (e.g. tests, stripped IPA shims) doesn't abort the pipeline.
		return false, nil
	}
	for _, l := range f.Loads {
		switch v := l.(type) {
		case *macho.EncryptionInfo:
			if v.CryptID != 0 {
				return true, nil
			}
		case *macho.EncryptionInfo64:
			if v.CryptID != 0 {
				return true, nil
			}
		}
	}
	return false, nil
}

// nativeDependencies mirrors Bin.Dependencies: every imported dylib (the ID
// is not an import, so ImportedLibraries already excludes it) filtered by
// cyan's path-starter rule.
func nativeDependencies(path string) ([]string, error) {
	f, err := readFirst(path)
	if err != nil {
		return nil, err
	}
	var deps []string
	for _, d := range f.ImportedLibraries() {
		for _, s := range nativeDepStarters {
			if strings.HasPrefix(d, s) {
				deps = append(deps, d)
				break
			}
		}
	}
	return deps, nil
}

// nativeAllDependencies returns every imported dylib name (ImportedLibraries
// covers Load/Weak/ReExport/Upward/Lazy), unfiltered — unlike
// nativeDependencies, which applies cyan's path-starter rule.
func nativeAllDependencies(path string) ([]string, error) {
	f, err := readFirst(path)
	if err != nil {
		return nil, err
	}
	return f.ImportedLibraries(), nil
}

func nativeArchitectures(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if isFatData(data) {
		ff, err := macho.NewFatFile(bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		defer ff.Close()
		var archs []string
		for _, a := range ff.Arches {
			archs = append(archs, cpuArchName(a.CPU, a.SubCPU))
		}
		return archs, nil
	}
	f, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	return []string{cpuArchName(f.CPU, f.SubCPU)}, nil
}

// nativeThinToArch slices a fat binary down to the named architecture (or
// confirms a thin binary already is one). Architecture names come from
// cpuArchName, matching Bin.Architectures() ("arm64", "arm64e", "x86_64",
// "armv7", ...).
func nativeThinToArch(path, arch string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !isFatData(data) {
		f, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if cpuArchName(f.CPU, f.SubCPU) == arch {
			return nil // already thin the requested arch
		}
		return fmt.Errorf("%s has no %s slice (thin %s)", path, arch, cpuArchName(f.CPU, f.SubCPU))
	}
	ff, err := macho.NewFatFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer ff.Close()
	for _, a := range ff.Arches {
		if cpuArchName(a.CPU, a.SubCPU) == arch {
			return atomicWrite(path, data[a.Offset:a.Offset+a.Size])
		}
	}
	return fmt.Errorf("%s has no %s slice", path, arch)
}

// --- signature ops ----------------------------------------------------------

func nativeExtractEntitlements(path string) ([]byte, error) {
	f, err := readFirst(path)
	if err != nil {
		return nil, err
	}
	cs := f.CodeSignature()
	if cs == nil || cs.Offset == 0 || cs.Size == 0 || len(cs.CodeDirectories) == 0 {
		return nil, fmt.Errorf("no readable code signature")
	}
	return []byte(cs.Entitlements), nil
}

func nativeIsSigned(path string) bool {
	f, err := readFirst(path)
	if err != nil {
		return false
	}
	cs := f.CodeSignature()
	return cs != nil && cs.Offset > 0 && cs.Size > 0 && len(cs.CodeDirectories) > 0
}

func nativeRemoveSignature(path string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		cs := f.CodeSignature()
		if cs == nil {
			return nativeWrite(f, orig, nil)
		}
		linkedit := f.Segment("__LINKEDIT")
		if linkedit == nil {
			return nil, fmt.Errorf("no __LINKEDIT segment")
		}
		if uint64(cs.Offset) < linkedit.Offset {
			return nil, fmt.Errorf("code signature offset %#x precedes __LINKEDIT %#x", cs.Offset, linkedit.Offset)
		}
		// Truncate __LINKEDIT back to the pre-signature data; nativeWrite then
		// copies only that much, dropping the signature blob.
		linkedit.Filesz = uint64(cs.Offset) - linkedit.Offset
		linkedit.Memsz = linkedit.Filesz
		if err := f.FileTOC.RemoveLoad(cs); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}

// nativeSign ad-hoc signs path. If ents is nil the existing entitlements are
// preserved (or none created), matching ldid -S -M; non-nil ents are written
// as the new entitlements, matching ldid -S<file> -M -Cadhoc.
func nativeSign(path string, ents []byte) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		cfg := &codesign.Config{
			Flags:        ctypes.ADHOC,
			Entitlements: ents,
		}
		linkedit := f.Segment("__LINKEDIT")
		cs := f.CodeSignature()
		if cs == nil || cs.Offset == 0 || linkedit == nil || uint64(cs.Offset) < linkedit.Offset || len(cs.CodeDirectories) == 0 {
			// Fresh signature: no LC_CODE_SIGNATURE, the stub go toolchain
			// emits, or one pointing somewhere unusable. Drop any stale
			// command so nativeCodeSign builds a fresh one.
			if cs != nil {
				_ = f.FileTOC.RemoveLoad(cs)
			}
		}
		// ldid always derives the identifier from the file basename; importing
		// it from a previous CodeDirectory can resurrect a stale value (e.g.
		// go-build binaries carry "a.out") and makes re-signs diverge.
		cfg.ID = filepath.Base(path)
		// ldid writes no runtime version for ad-hoc signs. Importing one from
		// the binary's LC_BUILD_VERSION (as File.CodeSign does) puts a
		// non-zero runtime in the CD without the RUNTIME flag, which Apple's
		// verifier rejects ("code or signature have been modified").
		cfg.RuntimeVersion = 0
		if len(ents) > 0 {
			// Setting or replacing entitlements: pin an empty SpecialSlots so
			// CodeSign cannot import the previous CodeDirectory's hashes (its
			// previous-hash verification would reject legitimately-changed
			// entitlements), and supply the DER blob that codesign.Sign
			// dereferences — without it, an empty SpecialSlots slice panics on
			// `config.SpecialSlots[0]`.
			cfg.SpecialSlots = []ctypes.SpecialSlot{}
			der, err := entitlementsDER(ents)
			if err != nil {
				return nil, err
			}
			cfg.EntitlementsDER = der
		}
		ledata, err := nativeCodeSign(f, orig, cfg)
		if err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, ledata)
	})
}

// nativeCodeSign replicates go-macho's File.CodeSign (export.go:302) but
// returns the rebuilt __LINKEDIT data instead of stashing it in the
// unexported f.ledata, and reads segment data from the original file bytes
// instead of f.cr.ReadAtAddr (which mis-resolves __LINKEDIT for binaries
// whose __DWARF segment carries Memsz=0, as go-build emits).
func nativeCodeSign(f *macho.File, orig []byte, config *codesign.Config) ([]byte, error) {
	config.InitSlotHashes() // initialize slot hashes with default empty slot hashes

	config.IsMain = f.Type == types.MH_EXECUTE

	text := f.Segment("__TEXT")
	if text == nil {
		return nil, fmt.Errorf("failed to find __TEXT segment")
	}
	config.TextOffset = uint64(text.Offset)
	config.TextSize = uint64(text.Filesz)

	// check if there is an embedded Info.plist
	if infoPlist, err := f.GetEmbeddedInfoPlist(); err == nil {
		config.InfoPlist = infoPlist
	}

	linkedit := f.Segment("__LINKEDIT")
	if linkedit == nil {
		return nil, fmt.Errorf("failed to find __LINKEDIT segment")
	}

	if config.ResourceDirSlotHash != nil {
		config.SlotHashes.ResourceDir = config.ResourceDirSlotHash
	}

	var cs *macho.CodeSignature
	if cs = f.CodeSignature(); cs != nil { // existing code signature
		// import settings from existing code signature
		if len(cs.CodeDirectories) > 0 {
			if config.ID == "" {
				config.ID = cs.CodeDirectories[0].ID
			}
			if config.TeamID == "" {
				config.TeamID = cs.CodeDirectories[0].TeamID
			}
			if config.Flags == ctypes.ADHOC {
				config.Flags &^= ctypes.LINKER_SIGNED
			}
			if config.Entitlements == nil {
				config.Entitlements = []byte(cs.Entitlements)
			}
			if config.EntitlementsDER == nil {
				config.EntitlementsDER = []byte(cs.EntitlementsDER)
			}
			if config.SpecialSlots == nil {
				config.SpecialSlots = cs.CodeDirectories[0].SpecialSlots
			}
			// ldid never writes a runtime version for ad-hoc signatures, and
			// importing one (from the existing CD or LC_BUILD_VERSION) puts a
			// non-zero runtime in the CD without the RUNTIME flag, which
			// Apple's verifier rejects ("code or signature have been
			// modified"). Callers that want a runtime set it explicitly.
			if config.RuntimeVersion != 0 {
				if cs.CodeDirectories[0].Header.Runtime != 0 {
					config.RuntimeVersion = cs.CodeDirectories[0].Header.Runtime
				} else if bvs := f.BuildVersions(); len(bvs) > 0 {
					config.RuntimeVersion = bvs[0].Sdk
				} else if vm := f.VersionMin(); vm != nil {
					config.RuntimeVersion = vm.Sdk
				}
			}
		}
	} else { // create NEW code signature
		if config.ID == "" {
			return nil, fmt.Errorf("you must supply an ID")
		}
		if config.Flags&ctypes.RUNTIME != 0 {
			if config.RuntimeVersion == 0 {
				if bvs := f.BuildVersions(); len(bvs) > 0 {
					config.RuntimeVersion = bvs[0].Sdk
				} else if vm := f.VersionMin(); vm != nil {
					config.RuntimeVersion = vm.Sdk
				}
			}
		} else {
			config.RuntimeVersion = 0
		}
		cs = &macho.CodeSignature{
			CodeSignatureCmd: types.CodeSignatureCmd{
				LoadCmd: types.LC_CODE_SIGNATURE,
				Len:     uint32(binary.Size(types.CodeSignatureCmd{})),
			},
		}
		cs.Offset = pointerAlign(uint32(linkedit.Offset + linkedit.Filesz))
		// add NEW codesignature load command
		f.AddLoad(cs)
		// refresh
		cs = f.CodeSignature()
	}

	config.CodeSize = uint64(cs.Offset)

	// cache __LINKEDIT data (up to but not including any existing code
	// signature). If the actual data doesn't go up to the signature offset
	// (fresh signature on a not-aligned file), pad with zeroes.
	ledata := make([]byte, uint64(cs.Offset)-linkedit.Offset)
	size := uint64(len(ledata))
	if size > linkedit.Filesz {
		size = linkedit.Filesz
	}
	copy(ledata[:size], orig[linkedit.Offset:linkedit.Offset+size])

	// update __LINKEDIT segment sizes
	linkedit.Filesz = pageAlign(uint64(len(ledata))+codesign.EstimateCodeSignatureSize(config), nativePageSize(f))
	linkedit.Memsz = pageAlign(linkedit.Filesz, nativePageSize(f))
	// update LC_CODE_SIGNATURE size
	cs.Size = uint32((linkedit.Offset + linkedit.Filesz) - uint64(cs.Offset))

	// read data to be signed; pad to beginning of code signature if necessary
	// (in case we added a signature to a not-page-aligned-size file)
	data := make([]byte, cs.Offset)
	copy(data, orig[:linkedit.Offset+size])

	// write modified file header and load commands (including __LINKEDIT and
	// CodeSignature), since they are covered by hashes. serializeTOC already
	// emits the file header as its first element — writing it again here
	// would double it and shift every page hash off by 32 bytes.
	var buf bytes.Buffer
	if err := serializeTOC(f, &buf); err != nil {
		return nil, fmt.Errorf("failed to write updated load commands: %v", err)
	}
	copy(data, buf.Bytes())

	// sign data and add it to the new LINKEDIT segment
	csdata, err := codesign.Sign(bytes.NewReader(data), config)
	if err != nil {
		return nil, fmt.Errorf("failed to create codesignature data: %v", err)
	}
	ledata = append(ledata, csdata...)

	if linkedit.Filesz < uint64(len(ledata)) {
		return nil, fmt.Errorf("new linkedit data is larger than expected")
	} else if linkedit.Filesz > uint64(len(ledata)) { // pad with zeros
		ledata = append(ledata, make([]byte, linkedit.Filesz-uint64(len(ledata)))...)
	}

	return ledata, nil
}

// --- load-command edit ops --------------------------------------------------

// newDylib builds a dylib load command (LC_LOAD_DYLIB-family) with an inlined
// name, the classic 24-byte name offset, and insert_dylib's timestamp.
func newDylib(cmd types.LoadCmd, name string) macho.Dylib {
	d := macho.Dylib{
		DylibCmd: types.DylibCmd{
			LoadCmd:    cmd,
			NameOffset: uint32(binary.Size(types.DylibCmd{})),
			Timestamp:  2,
		},
		Name: name,
	}
	d.Len = d.LoadSize()
	return d
}

// dylibOf unwraps the shared Dylib from any dylib-flavored load command.
func dylibOf(l macho.Load) (*macho.Dylib, bool) {
	switch v := l.(type) {
	case *macho.LoadDylib:
		return &v.Dylib, true
	case *macho.WeakDylib:
		return &v.Dylib, true
	case *macho.ReExportDylib:
		return &v.Dylib, true
	case *macho.UpwardDylib:
		return &v.Dylib, true
	case *macho.LazyLoadDylib:
		return &v.Dylib, true
	}
	return nil, false
}

func nativeInjectWeak(path, dylibPath string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		f.AddLoad(&macho.WeakDylib{Dylib: newDylib(types.LC_LOAD_WEAK_DYLIB, dylibPath)})
		if err := guardLoadCommands(f); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}

// nativeIsMachO reports whether the file at path starts with a Mach-O or
// fat-Mach-O magic (either byte order). Reads only the first 4 bytes, so it
// is safe on arbitrarily large files.
func nativeIsMachO(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		if err == io.EOF {
			return false, nil
		}
		return false, err
	}
	v := binary.BigEndian.Uint32(magic[:])
	// Big-endian reads make the little-endian magics appear byte-swapped.
	// Values: 64/32-bit thin, fat, fat64, and their little-endian twins.
	switch v {
	case 0xfeedfacf, 0xfeedface, 0xcafebabe, 0xcafebabf,
		0xcffaedfe, 0xcefaedfe, 0xbebafeca, 0xbfbafeca:
		return true, nil
	}
	return false, nil
}

// nativeInstallName returns the LC_ID_DYLIB install name of the first
// architecture slice, or "" when the binary has none.
func nativeInstallName(path string) (string, error) {
	f, err := readFirst(path)
	if err != nil {
		return "", err
	}
	if d := f.DylibID(); d != nil {
		return d.Name, nil
	}
	return "", nil
}

func nativeChangeDependency(path, old, new string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		changed := false
		for _, l := range f.Loads {
			d, ok := dylibOf(l)
			if !ok || d.Name != old {
				continue
			}
			d.Name = new
			d.Len = d.LoadSize() // Write errors if Len is too small
			changed = true
		}
		if !changed {
			return nativeWrite(f, orig, nil)
		}
		if err := guardLoadCommands(f); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}

func nativeSetInstallName(path, name string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		if d := f.DylibID(); d != nil {
			d.Name = name
			d.Len = d.LoadSize()
			if err := guardLoadCommands(f); err != nil {
				return nil, err
			}
			return nativeWrite(f, orig, nil)
		}
		d := newDylib(types.LC_ID_DYLIB, name)
		f.AddLoad(&macho.IDDylib{Dylib: d})
		if err := guardLoadCommands(f); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}

func nativeAddRpath(path, rpath string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		for _, l := range f.Loads {
			if r, ok := l.(*macho.Rpath); ok && r.Path == rpath {
				return nativeWrite(f, orig, nil) // already present; install_name_tool -add_rpath errors here
			}
		}
		r := &macho.Rpath{Path: rpath}
		// Path is the lc_str offset of the string within the command; leaving
		// it zero makes a re-parse read the path from the command bytes
		// (garbage), which then desyncs every subsequent command on rewrite.
		r.RpathCmd = types.RpathCmd{
			LoadCmd:    types.LC_RPATH,
			Len:        r.LoadSize(),
			PathOffset: uint32(binary.Size(types.RpathCmd{})),
		}
		f.AddLoad(r)
		if err := guardLoadCommands(f); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}

// nativeRpaths returns the LC_RPATH paths of the first architecture slice.
func nativeRpaths(path string) ([]string, error) {
	f, err := readFirst(path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, l := range f.Loads {
		if r, ok := l.(*macho.Rpath); ok {
			out = append(out, r.Path)
		}
	}
	return out, nil
}

// nativeReplaceRpath rewrites an existing LC_RPATH entry's path (e.g.
// /var/jb/usr/lib -> @loader_path/.jbroot/usr/lib for the roothide
// layout). Growing paths ride the same guard + resize machinery as
// ChangeDependency; the command size is recomputed via LoadSize().
func nativeReplaceRpath(path, old, new string) error {
	return editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		changed := false
		for _, l := range f.Loads {
			r, ok := l.(*macho.Rpath)
			if !ok || r.Path != old {
				continue
			}
			r.Path = new
			r.Len = r.LoadSize()
			changed = true
		}
		if !changed {
			return nativeWrite(f, orig, nil)
		}
		if err := guardLoadCommands(f); err != nil {
			return nil, err
		}
		return nativeWrite(f, orig, nil)
	})
}
