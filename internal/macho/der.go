package macho

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"sort"
	"strconv"
)

// This file is the M3 entitlements DER encoder. The ad-hoc signature blob
// carries two copies of the entitlements: the XML plist (CSLOT_ENTITLEMENTS)
// and a DER encoding (CSLOT_ENTITLEMENTS_DER). We reproduce Apple's XNU
// der_plist format exactly — a context-specific 0x70 "Apple plist" wrapper
// carrying a version INTEGER, then a 0xb0 SET of per-key SEQUENCEs
// (SEQUENCE { UTF8String key, <typed value> }), captured byte-for-byte from
// codesign -s - (701a020101b01530130c0e…0101ff). ldid's 0x31-wrapped form is
// tolerated on-device, but macOS codesign's DR evaluation fails on any DER
// that isn't the Apple format. A non-empty DER also keeps pkg/codesign.Sign
// from dereferencing a nil SpecialSlots slice when signing binaries whose
// existing CodeDirectory carries no special slots (go-build ad-hoc
// signatures, LC_CODE_SIGNATURE stubs).

// plistKind discriminates the value kinds an entitlements plist can hold.
type plistKind uint8

const (
	pBool plistKind = iota
	pString
	pInt
	pArray
	pDict
	pData
)

type plistVal struct {
	kind plistKind
	b    bool
	s    string
	i    int64
	arr  []plistVal
	dict []plistPair
}

type plistPair struct {
	key string
	val plistVal
}

// entitlementsDER converts an entitlements XML plist into the DER blob ldid
// embeds in the signature.
func entitlementsDER(data []byte) ([]byte, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	// Skip any wrapper elements (e.g. <plist> with its DOCTYPE) until the
	// first <dict>, which is where the key/value pairs live.
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			// No <dict> at all (e.g. an empty <plist/>): emit a valid empty
			// Apple plist rather than failing the whole sign.
			return derTlv(0x70, append([]byte{0x02, 0x01, 0x01}, derTlv(0xb0, nil)...)), nil
		}
		if err != nil {
			return nil, fmt.Errorf("entitlements DER: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			pairs, err := parseDictBody(dec)
			if err != nil {
				return nil, err
			}
			// Apple's DER-encoded entitlements (XNU der_plist, the format
			// codesign embeds in CSLOT_ENTITLEMENTS_DER) wraps the key/value
			// set in a context-specific 0x70 "Apple plist" carrying a version
			// INTEGER, then a 0xb0 SET of per-key SEQUENCEs. Reproduced from a
			// codesign -s - fixture: 701a020101b01530130c0e…0101ff. ldid's
			// 0x31-wrapped form is tolerated by devices but macOS codesign's
			// DR evaluation fails on a bare SEQUENCE. Keys are sorted
			// lexicographically: DER SET OF mandates canonical ordering, and
			// codesign rejects an unsorted set (confirmed against a
			// codesign -s - fixture with keys in non-alphabetical input
			// order: zeta/alpha/mid -> alpha/mid/zeta).
			sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
			var set []byte
			for _, p := range pairs {
				val, err := derValue(p.val)
				if err != nil {
					return nil, err
				}
				set = append(set, derSeq(append(derUTF8String(p.key), val...))...)
			}
			// 0x70 { INTEGER 1, 0xb0 { pairs } }
			inner := append([]byte{0x02, 0x01, 0x01}, derTlv(0xb0, set)...)
			return derTlv(0x70, inner), nil
		}
	}
}

// parseDictBody consumes alternating <key>text</key> <value/> pairs until the
// matching </dict>, preserving document order (deterministic output).
func parseDictBody(dec *xml.Decoder) ([]plistPair, error) {
	var pairs []plistPair
	var key string
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("entitlements DER: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "key":
				ct, err := dec.Token()
				if err != nil {
					return nil, err
				}
				cd, ok := ct.(xml.CharData)
				if !ok {
					return nil, fmt.Errorf("entitlements DER: expected text after <key>")
				}
				key = string(cd)
				// Consume the matching </key> so the EndElement case below only
				// fires for the dict's own close; otherwise the first </key>
				// ends the pair loop and every DER comes out empty ("30 00").
				endTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if ee, ok := endTok.(xml.EndElement); !ok || ee.Name.Local != "key" {
					return nil, fmt.Errorf("entitlements DER: expected </key>")
				}
			case "true", "false":
				if key == "" {
					return nil, fmt.Errorf("entitlements DER: value without key")
				}
				pairs = append(pairs, plistPair{key, plistVal{kind: pBool, b: t.Name.Local == "true"}})
				key = ""
				// A self-closing <true/> still emits a matching </true> token;
				// consume (and verify) it so it can't end the pair loop — the
				// first key's close used to terminate the whole dict parse.
				closeTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				if ee, ok := closeTok.(xml.EndElement); !ok || ee.Name.Local != t.Name.Local {
					return nil, fmt.Errorf("entitlements DER: expected </%s>", t.Name.Local)
				}
			case "string", "integer", "data", "array", "dict":
				if key == "" {
					return nil, fmt.Errorf("entitlements DER: value without key")
				}
				v, err := parseValue(dec, t)
				if err != nil {
					return nil, err
				}
				pairs = append(pairs, plistPair{key, v})
				key = ""
			default:
				return nil, fmt.Errorf("entitlements DER: unexpected element <%s> in dict", t.Name.Local)
			}
		case xml.EndElement:
			return pairs, nil
		}
	}
}

// parseValue consumes a single value element (and its children) and returns
// it as a plistVal.
func parseValue(dec *xml.Decoder, se xml.StartElement) (plistVal, error) {
	text := func() (string, error) {
		ct, err := dec.Token()
		if err != nil {
			return "", err
		}
		cd, ok := ct.(xml.CharData)
		if !ok {
			return "", fmt.Errorf("expected text in <%s>", se.Name.Local)
		}
		return string(cd), nil
	}
	switch se.Name.Local {
	case "string":
		s, err := text()
		if err != nil {
			return plistVal{}, err
		}
		if _, err := dec.Token(); err != nil { // consume </string>
			return plistVal{}, err
		}
		return plistVal{kind: pString, s: s}, nil
	case "data":
		s, err := text()
		if err != nil {
			return plistVal{}, err
		}
		if _, err := dec.Token(); err != nil { // consume </data>
			return plistVal{}, err
		}
		return plistVal{kind: pData, s: s}, nil
	case "integer":
		s, err := text()
		if err != nil {
			return plistVal{}, err
		}
		n, perr := strconv.ParseInt(s, 10, 64)
		if perr != nil {
			return plistVal{}, fmt.Errorf("entitlements DER: bad integer %q: %w", s, perr)
		}
		if _, err := dec.Token(); err != nil { // consume </integer>
			return plistVal{}, err
		}
		return plistVal{kind: pInt, i: n}, nil
	case "array":
		var arr []plistVal
		for {
			tok, err := dec.Token()
			if err != nil {
				return plistVal{}, err
			}
			switch t := tok.(type) {
			case xml.StartElement:
				v, err := parseValue(dec, t)
				if err != nil {
					return plistVal{}, err
				}
				arr = append(arr, v)
			case xml.EndElement:
				return plistVal{kind: pArray, arr: arr}, nil
			}
		}
	case "dict":
		pairs, err := parseDictBody(dec)
		return plistVal{kind: pDict, dict: pairs}, err
	}
	return plistVal{}, fmt.Errorf("entitlements DER: unsupported element <%s>", se.Name.Local)
}

// derValue encodes one entitlement value. Unsupported kinds (nested dicts,
// data) fall back to a UTF8String of their raw text so the DER stays
// parseable rather than failing the whole sign.
func derValue(v plistVal) ([]byte, error) {
	switch v.kind {
	case pBool:
		if v.b {
			return derTlv(0x01, []byte{0xff}), nil // DER TRUE
		}
		return derTlv(0x01, []byte{0x00}), nil
	case pString:
		return derUTF8String(v.s), nil
	case pData:
		return derUTF8String(v.s), nil
	case pInt:
		return derInteger(v.i), nil
	case pArray:
		var content []byte
		for _, e := range v.arr {
			ev, err := derValue(e)
			if err != nil {
				return nil, err
			}
			content = append(content, ev...)
		}
		return derTlv(0x31, content), nil // SET OF
	case pDict:
		// DER SET OF demands canonical ordering at every nesting level, not
		// just the top one — sort nested pairs too.
		sort.Slice(v.dict, func(i, j int) bool { return v.dict[i].key < v.dict[j].key })
		var content []byte
		for _, p := range v.dict {
			ev, err := derValue(p.val)
			if err != nil {
				return nil, err
			}
			content = append(content, derSeq(append(derUTF8String(p.key), ev...))...)
		}
		return derSeq(content), nil
	}
	return derUTF8String(v.s), nil
}

// --- DER primitives ---------------------------------------------------------

func derLen(n int) []byte {
	if n < 0x80 {
		return []byte{byte(n)}
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte(n & 0xff)}, b...)
		n >>= 8
	}
	return append([]byte{0x80 | byte(len(b))}, b...)
}

func derTlv(tag byte, content []byte) []byte {
	out := []byte{tag}
	out = append(out, derLen(len(content))...)
	return append(out, content...)
}

func derSeq(content []byte) []byte  { return derTlv(0x30, content) }
func derUTF8String(s string) []byte { return derTlv(0x0c, []byte(s)) }

// derInteger encodes a non-negative integer in minimal two's-complement DER
// form (the entitlements values that appear as integers are small and
// positive, e.g. platform/version numbers).
func derInteger(n int64) []byte {
	var b []byte
	v := uint64(n)
	for v > 0 {
		b = append([]byte{byte(v & 0xff)}, b...)
		v >>= 8
	}
	if len(b) == 0 {
		b = []byte{0}
	}
	if b[0]&0x80 != 0 {
		b = append([]byte{0}, b...)
	}
	return derTlv(0x02, b)
}
