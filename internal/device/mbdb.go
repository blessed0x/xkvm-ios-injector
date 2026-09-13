package device

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
)

// MBDB (multi-backup database) record encoding: the manifest format
// mobilebackup2 streams as Manifest.mbdb. Layout mirrors the sparserestore
// codec (mbdb.py) byte-for-byte: 2-byte big-endian string lengths, 8-byte
// inode, 4-byte uids/times, 8-byte size, 1-byte flags + property count.
const (
	// S_IFREG/S_IFDIR plus 0750 — the same bits sparserestore writes
	// (DEFAULT | file-mode type bit).
	mbdbModeFile = 0o100755
	mbdbModeDir  = 0o040755
	mbdbFlags    = 0x4
)

type mbdbRecord struct {
	Domain     string
	Filename   string
	Link       string
	Hash       []byte
	Key        []byte
	Mode       uint16
	Inode      uint64
	UserID     uint32
	GroupID    uint32
	MTime      uint32
	ATime      uint32
	CTime      uint32
	Size       uint64
	Flags      uint8
	Properties [][2]string
}

func (r mbdbRecord) bytes() []byte {
	var b []byte
	pad := func(s string) {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(s)))
		b = append(b, l[:]...)
		b = append(b, s...)
	}
	padRaw := func(p []byte) {
		var l [2]byte
		binary.BigEndian.PutUint16(l[:], uint16(len(p)))
		b = append(b, l[:]...)
		b = append(b, p...)
	}
	u16 := func(v uint16) { b = binary.BigEndian.AppendUint16(b, v) }
	u32 := func(v uint32) { b = binary.BigEndian.AppendUint32(b, v) }
	u64 := func(v uint64) { b = binary.BigEndian.AppendUint64(b, v) }

	pad(r.Domain)
	pad(r.Filename)
	pad(r.Link)
	padRaw(r.Hash)
	padRaw(r.Key)
	u16(r.Mode)
	u64(r.Inode)
	u32(r.UserID)
	u32(r.GroupID)
	u32(r.MTime)
	u32(r.ATime)
	u32(r.CTime)
	u64(r.Size)
	b = append(b, r.Flags)
	b = append(b, uint8(len(r.Properties)))
	for _, p := range r.Properties {
		pad(p[0])
		pad(p[1])
	}
	return b
}

func manifestDB(records []mbdbRecord) []byte {
	out := []byte("mbdb\x05\x00")
	for _, r := range records {
		out = append(out, r.bytes()...)
	}
	return out
}

// randInode draws a random inode like sparserestore does (randbytes(8)).
func randInode() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0 // crypto/rand failing is not recoverable; 0 still round-trips
	}
	return binary.BigEndian.Uint64(b[:])
}

// payloadName is the backup blob filename convention: sha1(domain + "-" +
// path) in hex.
func payloadName(domain, path string) string {
	sum := sha1.Sum([]byte(domain + "-" + path))
	return hex.EncodeToString(sum[:])
}
