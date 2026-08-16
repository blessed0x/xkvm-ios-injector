package macho

// This file closes the __cstring boundary documented in ARCHITECTURE.md
// §5.4: runtime-dlopen strings compiled into __TEXT are now rewritten, not
// just load commands. The design ports rootless-patcher's
// RPMachOModifier/RPStringPatcher faithfully with VM-correct addressing:
//
//  1. scan __TEXT.__cstring for NUL-terminated strings
//  2. rewrites that fit in place (replacement length <= original) are done
//     directly in the section — no reference retargeting needed
//  3. growing replacements are packed into a new __PATCH_ROOTLESS,__cstring
//     segment inserted physically before __LINKEDIT (mapped after it in VM,
//     matching upstream), every linkedit-referencing load command's offsets
//     shift by the new segment size, and each reference — __cfstring table
//     entries, __data pointers, ADRP/ADD and ADR instructions — is
//     retargeted to the relocated string's VM address
//
// Deviations from upstream (documented): addresses are compared/patched as
// full 64-bit VM values (upstream mixes file and image-base spaces); the
// patcher runs on arm64 slices only (upstream's assembler helpers are
// arm64-specific — x86_64 LEA RIP-relative refs are not retargeted, so
// non-arm64 slices get in-place rewrites only); the chained-fixups
// starts_in_image growth is written but deliberately not repointed
// (upstream orphans the block; dyld keeps using the valid original).

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"fmt"
	"os"
	"slices"

	"github.com/blacktop/go-macho"
	"github.com/blacktop/go-macho/types"
)

// CString is one NUL-terminated string found in __TEXT.__cstring.
type CString struct {
	FileOff uint32 // file offset of the string's first byte
	Addr    uint64 // virtual address of the string
	Value   string
}

// inPlaceEdit is a __cstring replacement that fits in the original slot.
type inPlaceEdit struct {
	fileOff uint32
	origLen int
	repl    string
}

// relocation is a growing __cstring replacement packed into
// __PATCH_ROOTLESS.__cstring.
type relocation struct {
	c    CString
	repl string
}

// RewriteStats reports what a RewriteCStrings pass did per slice.
type RewriteStats struct {
	Scanned   int
	InPlace   int
	Relocated int
}

const (
	// dyld_chained_fixups_header magic (0xFADE0C02).
	dyldChainedFixupsMagic = 0xFADE0C02
	// dyld_chained_starts_in_segment fixed size (24 bytes of fields before
	// the page_start array).
	dyldStartsInSegmentSize = 24
)

// CStrings scans __TEXT.__cstring of the first architecture slice and
// returns the strings it contains.
func CStrings(path string) ([]CString, error) {
	data, err := readFileBytes(path)
	if err != nil {
		return nil, err
	}
	f, err := readFirst(path)
	if err != nil {
		return nil, err
	}
	sec := f.Section("__TEXT", "__cstring")
	if sec == nil || sec.Size == 0 {
		return nil, nil
	}
	return scanCStrings(data, sec)
}

// RewriteCStrings rewrites every __TEXT.__cstring string of every
// architecture slice via convert: the function returns the replacement for
// a string, or "" to leave it untouched. Growing replacements are relocated
// into a new __PATCH_ROOTLESS,__cstring segment (see the package comment).
// The file is rewritten atomically.
func RewriteCStrings(path string, convert func(string) string) (RewriteStats, error) {
	var total RewriteStats
	err := editMachO(path, func(f *macho.File, orig []byte) ([]byte, error) {
		stats, out, err := rewriteSliceCStrings(f, orig, convert)
		total.Scanned += stats.Scanned
		total.InPlace += stats.InPlace
		total.Relocated += stats.Relocated
		if err != nil {
			return nil, err
		}
		return out, nil
	})
	return total, err
}

func readFileBytes(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// scanCStrings splits a __cstring section into NUL-terminated strings.
func scanCStrings(data []byte, sec *types.Section) ([]CString, error) {
	if int(sec.Offset)+int(sec.Size) > len(data) {
		return nil, fmt.Errorf("__cstring section out of bounds (off=%d size=%d file=%d)", sec.Offset, sec.Size, len(data))
	}
	secEnd := int(sec.Offset) + int(sec.Size)
	section := data[sec.Offset:secEnd]
	var out []CString
	for i := 0; i < len(section); {
		j := bytes.IndexByte(section[i:], 0)
		if j < 0 {
			break // unterminated tail — not a string
		}
		if j > 0 {
			out = append(out, CString{
				FileOff: sec.Offset + uint32(i),
				Addr:    sec.Addr + uint64(i),
				Value:   string(section[i : i+j]),
			})
		}
		i += j + 1
	}
	return out, nil
}

// relocatedString is a growing replacement moved into __PATCH_ROOTLESS.
type relocatedString struct {
	oldAddr, newAddr uint64
	oldLen, newLen   int
}

// rewriteSliceCStrings implements RewriteCStrings for one architecture slice.
func rewriteSliceCStrings(f *macho.File, orig []byte, convert func(string) string) (RewriteStats, []byte, error) {
	var stats RewriteStats
	sec := f.Section("__TEXT", "__cstring")
	if sec == nil || sec.Size == 0 {
		return stats, orig, nil
	}
	strings, err := scanCStrings(orig, sec)
	if err != nil {
		return stats, nil, err
	}

	// Split into in-place edits and growing relocations.
	var inPlace []inPlaceEdit
	var reloc []relocation
	for _, c := range strings {
		stats.Scanned++
		repl := convert(c.Value)
		if repl == "" || repl == c.Value {
			continue
		}
		if len(repl) <= len(c.Value) {
			inPlace = append(inPlace, inPlaceEdit{c.FileOff, len(c.Value), repl})
			stats.InPlace++
		} else {
			reloc = append(reloc, relocation{c, repl})
			stats.Relocated++
		}
	}

	patched := bytes.Clone(orig)
	for _, e := range inPlace {
		copy(patched[e.fileOff:], []byte(e.repl))
		// Zero only the remainder of THIS string's slot — zeroing to the
		// section end would clobber the following strings.
		remainder := e.origLen - len(e.repl)
		if remainder > 0 {
			clear(patched[e.fileOff+uint32(len(e.repl)) : e.fileOff+uint32(e.origLen)])
		}
	}
	if len(reloc) == 0 {
		return stats, patched, nil
	}

	// Relocation requires an arm64 slice (the ADRP/ADD/ADR patcher is
	// arm64-only) and a __LINKEDIT segment to insert before.
	if f.CPU != types.CPUArm64 && f.CPU != types.CPUArm6432 {
		return stats, patched, nil // in-place only; relocations skipped
	}
	le := f.Segment("__LINKEDIT")
	if le == nil {
		return stats, nil, fmt.Errorf("cannot relocate %d __cstring string(s): no __LINKEDIT segment", len(reloc))
	}

	// Deterministic order: by original VM address.
	slices.SortFunc(reloc, func(a, b relocation) int { return cmp.Compare(a.c.Addr, b.c.Addr) })

	// Pack the replacements; map original -> relocated string.
	targets := make(map[uint64]relocatedString, len(reloc))
	var newBytes []byte
	for _, r := range reloc {
		targets[r.c.Addr] = relocatedString{oldAddr: r.c.Addr, newAddr: 0, oldLen: len(r.c.Value), newLen: len(r.repl)}
		newBytes = append(newBytes, []byte(r.repl)...)
		newBytes = append(newBytes, 0)
	}
	pageSize := nativePageSize(f)
	newSize := pageAlign(uint64(len(newBytes)), pageSize)

	// Segment placement: physically before __LINKEDIT, mapped after it in
	// VM (upstream layout). Capture the old linkedit geometry and the
	// chained-fixups data offset BEFORE any mutation: the append must read
	// the original fixups block at its ORIGINAL file position, while the
	// shifted commands live in the rebuilt file.
	oldLeOff := uint32(le.Offset)
	oldLeFilesz := uint32(le.Filesz)
	fixupsDataOff := uint32(0)
	for _, l := range f.Loads {
		if c, ok := l.(*macho.DyldChainedFixups); ok && c.Offset != 0 {
			fixupsDataOff = c.Offset
			break
		}
	}
	seg := &macho.Segment{SegmentHeader: macho.SegmentHeader{
		LoadCmd:   types.LC_SEGMENT_64,
		Len:       uint32(seg64CmdSize + sec64CmdSize),
		Name:      "__PATCH_ROOTLESS",
		Addr:      le.Addr,
		Memsz:     newSize,
		Offset:    uint64(oldLeOff),
		Filesz:    newSize,
		Maxprot:   vmProtRead,
		Prot:      vmProtRead,
		Nsect:     1,
		Firstsect: uint32(len(f.Sections)),
	}}
	secNew := &types.Section{SectionHeader: types.SectionHeader{
		Name:   "__cstring",
		Seg:    "__PATCH_ROOTLESS",
		Addr:   le.Addr,
		Size:   uint64(len(newBytes)),
		Offset: oldLeOff,
		Flags:  types.CstringLiterals,
	}}
	f.Sections = append(f.Sections, secNew)

	// Assign replacement addresses now that the section addr is known.
	base := secNew.Addr
	for _, r := range reloc {
		t := targets[r.c.Addr]
		t.newAddr = base
		targets[r.c.Addr] = t
		base += uint64(len(r.repl) + 1)
	}

	// Insert the segment load before __LINKEDIT and shift linkedit geometry.
	insertSegmentBeforeLinkedit(f, seg)
	le.Addr += newSize
	le.Offset += newSize
	shiftLinkeditOffsets(f, newSize)

	// Chained-fixups growth: append a starts_in_image block covering the new
	// segment (orphaned, matching upstream) and grow __LINKEDIT sizes.
	var leAppend []byte
	if fixupsDataOff != 0 {
		leAppend = chainedFixupsAppend(orig, oldLeOff, fixupsDataOff)
		if len(leAppend) > 0 {
			le.Filesz += uint64(len(leAppend))
			le.Memsz += pageAlign(uint64(len(leAppend)), pageSize)
		}
	}

	// Assemble: TOC + everything up to the old linkedit position (segments
	// and gaps, with the in-place patches) + the new segment + linkedit data
	// + the chained-fixup append.
	var toc bytes.Buffer
	if err := serializeTOC(f, &toc); err != nil {
		return stats, nil, err
	}
	if first := firstDataOffset(f); toc.Len() > first {
		return stats, nil, fmt.Errorf("not enough space for load commands (%d > %d)", toc.Len(), first)
	}
	out := bytes.Clone(toc.Bytes())
	if int(oldLeOff) > len(out) {
		out = append(out, patched[len(out):oldLeOff]...)
	}
	// The new segment data (page-aligned, zero-padded after the strings).
	segData := make([]byte, newSize)
	copy(segData, newBytes)
	out = append(out, segData...)
	leEnd := int(oldLeOff) + int(oldLeFilesz)
	if leEnd > len(patched) {
		leEnd = len(patched)
	}
	out = append(out, patched[oldLeOff:leEnd]...)
	out = append(out, leAppend...)

	// Retarget references on the assembled bytes.
	patchReferenceRefs(out, f, targets)
	return stats, out, nil
}

// patchReferenceRefs retargets every reference to a relocated cstring:
// __cfstring table entries, __data pointer slots, and ADRP/ADD / ADR
// instruction pairs in executable segments.
func patchReferenceRefs(out []byte, f *macho.File, targets map[uint64]relocatedString) {
	// CFString table: each entry is {isa, info, data, length}; data at +16,
	// length at +24 (both native-endian in the file's byte order).
	for _, segName := range []string{"__DATA", "__DATA_CONST"} {
		sec := f.Section(segName, "__cfstring")
		if sec == nil || sec.Size == 0 {
			continue
		}
		data := sectionBytes(out, sec)
		for off := 0; off+32 <= len(data); off += 32 {
			ptr := binary.LittleEndian.Uint64(data[off+16:])
			if t, ok := targets[ptr]; ok {
				binary.LittleEndian.PutUint64(data[off+16:], t.newAddr)
				binary.LittleEndian.PutUint32(data[off+24:], uint32(t.newLen))
			}
		}
	}
	// __data pointer slots.
	if sec := f.Section("__DATA", "__data"); sec != nil && sec.Size != 0 {
		data := sectionBytes(out, sec)
		for off := 0; off+8 <= len(data); off += 8 {
			ptr := binary.LittleEndian.Uint64(data[off:])
			if t, ok := targets[ptr]; ok {
				binary.LittleEndian.PutUint64(data[off:], t.newAddr)
			}
		}
	}
	patchInstructionRefs(out, f, targets)
}

// patchInstructionRefs walks executable segment code as one instruction
// stream, emulating ADRP/ADR/ADD register values, and retargets any pair
// whose computed value is a relocated string. ADRP immediately followed by
// ADD is the canonical literal-loading pattern.
func patchInstructionRefs(out []byte, f *macho.File, targets map[uint64]relocatedString) {
	type region struct {
		addr uint64
		off  uint32
		size uint32
	}
	var regions []region
	for _, seg := range f.Segments() {
		if seg.Maxprot&vmProtExec == 0 || seg.Filesz == 0 {
			continue
		}
		regions = append(regions, region{seg.Addr, uint32(seg.Offset), uint32(seg.Filesz)})
	}
	slices.SortFunc(regions, func(a, b region) int { return cmp.Compare(a.addr, b.addr) })

	var regs [32]uint64
	for _, r := range regions {
		if int(r.off)+int(r.size) > len(out) {
			continue
		}
		code := out[r.off : r.off+r.size]
		for i := uint32(0); i+insnSize <= r.size; i += insnSize {
			pc := r.addr + uint64(i)
			insn := binary.LittleEndian.Uint32(code[i:])

			switch {
			case insn&adrpMask == adrpOpcode:
				regs[insn&regMask] = adrpValue(insn, pc)

			case insn&addMask == addOpcode:
				rd, rn, imm, shift := addFields(insn)
				if shift > 1 {
					continue
				}
				if shift == 1 {
					imm <<= 12
				}
				val := regs[rn] + uint64(imm)
				regs[rd] = val
				t, ok := targets[val]
				if !ok || i < insnSize {
					continue
				}
				prev := binary.LittleEndian.Uint32(code[i-insnSize:])
				// Canonical pairing: the previous instruction is an ADRP
				// whose destination feeds this ADD's source.
				if prev&adrpMask != adrpOpcode || prev&regMask != uint32(rn) {
					continue
				}
				pcADRP := pc - insnSize
				pageDelta := (int64(t.newAddr>>12) - int64(pcADRP>>12))
				binary.LittleEndian.PutUint32(code[i-insnSize:], encodeADRP(rn, pageDelta))
				binary.LittleEndian.PutUint32(code[i:], encodeADD(rd, rn, uint32(t.newAddr&pageOffsetMask), false))

			case insn&adrMask == adrOpcode:
				rd := int(insn & regMask)
				val := adrValue(insn, pc)
				regs[insn&regMask] = val
				t, ok := targets[val]
				if !ok {
					continue
				}
				off := int64(t.newAddr) - int64(pc)
				if off >= -adrMaxRange && off <= adrMaxRange {
					binary.LittleEndian.PutUint32(code[i:], encodeADR(rd, off))
					patchSwiftLength(code, i, t)
				} else if i+insnSize < r.size {
					// Out of ADR range: rewrite as ADRP+ADD into a
					// following NOP (upstream's fallback).
					next := binary.LittleEndian.Uint32(code[i+insnSize:])
					if next == nopInsn {
						pageDelta := (int64(t.newAddr>>12) - int64(pc>>12))
						binary.LittleEndian.PutUint32(code[i:], encodeADRP(rd, pageDelta))
						binary.LittleEndian.PutUint32(code[i+insnSize:], encodeADD(rd, rd, uint32(t.newAddr&pageOffsetMask), false))
						// The Swift length MOVZ sits 16 bytes after the ADR's own
						// position (upstream passes i, not i+INSTRUCTION_SIZE, in
						// both branches).
						patchSwiftLength(code, i, t)
					}
				}
			}
		}
	}
}

// patchSwiftLength fixes a Swift-emitted hardcoded string-length MOVZ that
// sits 16 bytes after an ADR rewrite (upstream's _patchSwiftInstructionForLengthAt).
func patchSwiftLength(code []byte, i uint32, t relocatedString) {
	at := i + insnSize*4
	if int(at)+insnSize > len(code) {
		return
	}
	insn := binary.LittleEndian.Uint32(code[at:])
	if insn&movMask != movOpcode {
		return
	}
	rd, imm := movFields(insn)
	if int(imm) != t.oldLen {
		return
	}
	binary.LittleEndian.PutUint32(code[at:], encodeMOV(rd, uint16(t.newLen)))
}

// sectionBytes returns the file bytes of a section (bounded).
func sectionBytes(out []byte, sec *types.Section) []byte {
	start, end := int(sec.Offset), int(sec.Offset)+int(sec.Size)
	if start < 0 || end > len(out) {
		return nil
	}
	return out[start:end]
}

// insertSegmentBeforeLinkedit places seg in f.Loads immediately before the
// __LINKEDIT load command.
func insertSegmentBeforeLinkedit(f *macho.File, seg *macho.Segment) {
	for i, l := range f.Loads {
		if s, ok := l.(*macho.Segment); ok && s.Name == "__LINKEDIT" {
			f.Loads = append(f.Loads, nil)
			copy(f.Loads[i+1:], f.Loads[i:])
			f.Loads[i] = seg
			return
		}
	}
	f.Loads = append(f.Loads, seg) // no __LINKEDIT found — append
}

// shiftLinkeditOffsets bumps every linkedit-referencing load command's file
// offsets by delta, porting upstream's _shiftCommandsWithOffset.
func shiftLinkeditOffsets(f *macho.File, delta uint64) {
	d := uint32(delta)
	for _, l := range f.Loads {
		switch v := l.(type) {
		case *macho.DyldInfo:
			if v.RebaseSize > 0 {
				v.RebaseOff += d
			}
			if v.BindSize > 0 && v.BindOff != 0 {
				v.BindOff += d
			}
			if v.WeakBindSize > 0 && v.WeakBindOff != 0 {
				v.WeakBindOff += d
			}
			if v.LazyBindSize > 0 && v.LazyBindOff != 0 {
				v.LazyBindOff += d
			}
			if v.ExportSize > 0 && v.ExportOff != 0 {
				v.ExportOff += d
			}
		case *macho.Symtab:
			if v.Symoff != 0 {
				v.Symoff += d
			}
			if v.Stroff != 0 {
				v.Stroff += d
			}
		case *macho.Dysymtab:
			if v.Tocoffset != 0 {
				v.Tocoffset += d
			}
			if v.Modtaboff != 0 {
				v.Modtaboff += d
			}
			if v.Extrefsymoff != 0 {
				v.Extrefsymoff += d
			}
			if v.Indirectsymoff != 0 {
				v.Indirectsymoff += d
			}
			if v.Extreloff != 0 {
				v.Extreloff += d
			}
			if v.Locreloff != 0 {
				v.Locreloff += d
			}
		case *macho.CodeSignature:
			if v.Offset != 0 {
				v.Offset += d
			}
		case *macho.FunctionStarts:
			if v.Offset != 0 {
				v.Offset += d
			}
		case *macho.DataInCode:
			if v.Offset != 0 {
				v.Offset += d
			}
		case *macho.LinkerOptimizationHint:
			if v.Offset != 0 {
				v.Offset += d
			}
		case *macho.DyldExportsTrie:
			if v.Offset != 0 {
				v.Offset += d
			}
		case *macho.DyldChainedFixups:
			if v.Offset != 0 {
				v.Offset += d
			}
		}
	}
}

// chainedFixupsAppend builds the enlarged dyld_chained_starts_in_image block
// (seg_count+1, the extra entry zeroed for __PATCH_ROOTLESS) that upstream
// appends after __LINKEDIT. The block is deliberately orphaned — the
// header's starts_offset still points at the valid original — matching
// upstream's behavior. Returns nil when the binary has no readable fixups
// data. orig is the unpatched slice bytes; leOff is the ORIGINAL (pre-shift)
// __LINKEDIT file offset; dataoff is the LC_DYLD_CHAINED_FIXUPS dataoff.
func chainedFixupsAppend(orig []byte, leOff, dataoff uint32) []byte {
	if dataoff < leOff || int(dataoff) >= len(orig) {
		return nil
	}
	hdr := orig[dataoff:]
	if len(hdr) < 16 || binary.LittleEndian.Uint32(hdr) != dyldChainedFixupsMagic {
		return nil
	}
	startsOffset := binary.LittleEndian.Uint32(hdr[8:])
	if startsOffset >= uint32(len(hdr)) {
		return nil
	}
	starts := hdr[startsOffset:]
	if len(starts) < 4 {
		return nil
	}
	segCount := binary.LittleEndian.Uint32(starts)

	// New block: seg_count (4) + (seg_count+1) offsets + one
	// starts_in_segment blob per existing segment (variable size).
	newBlock := make([]byte, 0, 4+4*(segCount+1))
	binary.LittleEndian.PutUint32(newBlock[:4], segCount+1)
	offsets := make([]byte, 4*(segCount+1))
	newBlock = append(newBlock, offsets...)

	for i := uint32(0); i < segCount; i++ {
		if int(4+4*i+4) > len(starts) {
			return nil
		}
		segInfoOff := binary.LittleEndian.Uint32(starts[4+4*i:])
		if segInfoOff == 0 {
			continue
		}
		if int(segInfoOff)+dyldStartsInSegmentSize > len(starts) {
			return nil
		}
		si := starts[segInfoOff:]
		pageCount := binary.LittleEndian.Uint32(si[20:])
		size := dyldStartsInSegmentSize
		if pageCount > 0 {
			size += int(pageCount-1) * 4
		}
		if int(segInfoOff)+size > len(starts) {
			return nil
		}
		binary.LittleEndian.PutUint32(newBlock[4+4*i:], uint32(len(newBlock)))
		newBlock = append(newBlock, starts[segInfoOff:segInfoOff+uint32(size)]...)
	}
	return newBlock
}

const (
	seg64CmdSize = 72
	sec64CmdSize = 80
	vmProtRead   = types.VmProtection(0x1)
	vmProtExec   = types.VmProtection(0x4)
)
