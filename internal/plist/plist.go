// Package plist wraps howett.net/plist for Info.plist-style files: binary
// and XML decoding, binary encoding (the format real app Info.plists ship in,
// and the safest default when rewriting), and XML encoding for plists that
// were authored as XML.
package plist

import (
	"fmt"
	"os"
	"reflect"

	howett "howett.net/plist"
)

// Dict is a decoded plist dictionary.
type Dict = map[string]any

// Open parses a binary or XML plist file.
func Open(path string) (Dict, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Decode(data)
}

// Decode parses binary/XML plist bytes into a Dict.
func Decode(data []byte) (Dict, error) {
	var d Dict
	if _, err := howett.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("couldn't parse plist: %w", err)
	}
	return d, nil
}

// Encode serializes d as a binary plist.
func Encode(d Dict) ([]byte, error) {
	return howett.Marshal(d, howett.BinaryFormat)
}

// Write serializes d as a binary plist.
func Write(path string, d Dict) error {
	data, err := Encode(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// WriteXML serializes d as an XML plist (for .strings-style files and plist
// merge outputs that should stay human-readable).
func WriteXML(path string, d Dict) error {
	data, err := EncodeXML(d)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// EncodeXML serializes d as an XML plist. Used by the roothide converter to
// re-encode merged entitlements before ad-hoc signing.
func EncodeXML(d Dict) ([]byte, error) {
	return howett.Marshal(d, howett.XMLFormat)
}

// ConvertToXML1 ports `plutil -convert xml1`: decodes a binary or XML plist
// (any root type) and re-serializes it as XML1. This is how text-level plist
// surgery (e.g. roothide's >-root path rewrites) can match binary plists.
// Like plutil under `set -e`, a file that is not a valid plist is an error.
func ConvertToXML1(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v any
	if _, err := howett.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("plutil -convert xml1 equivalent: not a valid plist: %w", err)
	}
	out, err := howett.Marshal(v, howett.XMLFormat)
	if err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o644)
}

// Equal deep-compares two dicts (test + merge semantics).
func Equal(a, b Dict) bool {
	return reflect.DeepEqual(a, b)
}
