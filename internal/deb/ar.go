package deb

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const arMagic = "!<arch>\n"

// arMember is one member of an ar archive. The member data is fully
// materialized in memory (deb payloads are small), and any trailing padding
// byte is consumed before the next member is read.
type arMember struct {
	Name string
	Data io.Reader
}

// readArMember reads the next 60-byte ar header plus the member data.
// io.EOF is returned when the archive ends.
func readArMember(r io.Reader) (arMember, error) {
	var hdr [60]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return arMember{}, err // io.EOF at a clean end
	}
	if hdr[58] != '`' || hdr[59] != '\n' {
		return arMember{}, fmt.Errorf("invalid ar member header magic")
	}

	name := strings.TrimSpace(string(hdr[0:16]))
	size, err := strconv.ParseInt(strings.TrimSpace(string(hdr[48:58])), 10, 64)
	if err != nil {
		return arMember{}, fmt.Errorf("invalid ar member size for %q: %w", name, err)
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return arMember{}, fmt.Errorf("short ar member %q: %w", name, err)
	}

	// ar pads odd-sized members to an even boundary; the pad byte comes after
	// the data, so it is consumed only once the data has been read.
	if size%2 == 1 {
		if _, err := io.CopyN(io.Discard, r, 1); err != nil && err != io.EOF {
			return arMember{}, err
		}
	}
	return arMember{Name: name, Data: bytes.NewReader(data)}, nil
}
