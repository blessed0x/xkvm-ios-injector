package macho

// arm64 instruction helpers for the __cstring reference patcher (a faithful
// Go port of rootless-patcher's assembler.c + the decode macros from
// assembler.h, with VM-correct addressing). Only the encodings the patcher
// needs are implemented; every helper is a pure function over 32-bit
// little-endian instruction words.

const (
	insnSize       = 4
	regMask        = 0x1F
	pageOffsetMask = 0xFFF
	armPageSize    = 0x1000 // ADRP addressing granularity (always 4K)
	adrMaxRange    = 1 << 20

	adrpOpcode = 0x90000000
	adrpMask   = 0x9F000000
	addOpcode  = 0x91000000
	addMask    = 0xFF000000
	adrOpcode  = 0x10000000
	adrMask    = 0x9F000000
	movOpcode  = 0xD2800000
	movMask    = 0xD2800000
	nopInsn    = 0xD503201F
)

// signExtend21 sign-extends a 21-bit immediate to int64.
func signExtend21(v uint32) int64 {
	v &= 0x1FFFFF
	if v&(1<<20) != 0 {
		v |= ^uint32(0x1FFFFF)
	}
	return int64(int32(v))
}

// adrpValue returns the page-aligned target of an ADRP instruction at pc.
func adrpValue(insn uint32, pc uint64) uint64 {
	immlo := (insn >> 29) & 0x3
	immhi := (insn >> 5) & 0x7FFFF
	imm := signExtend21((immhi << 2) | immlo)
	return (pc &^ pageOffsetMask) + uint64(imm<<12)
}

// encodeADRP builds an ADRP that loads the page containing addr, given the
// instruction's own pc. pageDelta is the signed page offset (addr's page
// minus pc's page).
func encodeADRP(rd int, pageDelta int64) uint32 {
	imm := uint32(int32(pageDelta)) & 0x1FFFFF
	insn := uint32(adrpOpcode) | uint32(rd)
	insn |= (imm & 0x3) << 29           // immlo
	insn |= ((imm >> 2) & 0x7FFFF) << 5 // immhi
	return insn
}

// adrValue returns the target of an ADR instruction at pc (signed 21-bit
// byte offset from the instruction itself).
func adrValue(insn uint32, pc uint64) uint64 {
	immlo := (insn >> 29) & 0x3
	immhi := (insn >> 5) & 0x7FFFF
	imm := signExtend21((immhi << 2) | immlo)
	return pc + uint64(imm)
}

// encodeADR builds an ADR loading pc+offset (|offset| must fit the signed
// 21-bit field).
func encodeADR(rd int, offset int64) uint32 {
	imm := uint32(int32(offset)) & 0x1FFFFF
	insn := uint32(adrOpcode) | uint32(rd)
	insn |= (imm & 0x3) << 29
	insn |= ((imm >> 2) & 0x7FFFF) << 5
	return insn
}

// addFields decodes an ADD-immediate's rd/rn/imm/shift (shift 0 or 1 = LSL 12).
func addFields(insn uint32) (rd, rn int, imm uint32, shift uint32) {
	rd = int(insn & regMask)
	rn = int((insn >> 5) & regMask)
	imm = (insn >> 10) & pageOffsetMask
	shift = (insn >> 22) & 3
	return
}

// encodeADD builds an ADD-immediate (shift 0 or 1 = LSL 12).
func encodeADD(rd, rn int, imm uint32, shift12 bool) uint32 {
	insn := uint32(addOpcode) | uint32(rd) | uint32(rn)<<5 | (imm&pageOffsetMask)<<10
	if shift12 {
		insn |= 1 << 22
	}
	return insn
}

// movFields decodes a MOVZ (imm16) rd/imm16.
func movFields(insn uint32) (rd int, imm uint16) {
	return int(insn & regMask), uint16((insn >> 5) & 0xFFFF)
}

// encodeMOV builds a MOVZ (imm16).
func encodeMOV(rd int, imm uint16) uint32 {
	return uint32(movOpcode) | uint32(rd) | uint32(imm)<<5
}
