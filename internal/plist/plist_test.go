package plist

import (
	"strings"

	howett "howett.net/plist"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenXMLFixture(t *testing.T) {
	d, err := Open(filepath.Join("..", "..", "testdata", "fixtures", "app", "Info.plist"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if d["CFBundleExecutable"] != "Test" {
		t.Errorf("CFBundleExecutable = %v, want Test", d["CFBundleExecutable"])
	}
	if d["CFBundleIdentifier"] != "com.example.test" {
		t.Errorf("CFBundleIdentifier = %v, want com.example.test", d["CFBundleIdentifier"])
	}
}

func TestBinaryRoundTrip(t *testing.T) {
	// Includes real-world types M2 will encounter when rewriting Info.plists:
	// integers (decoded by howett as uint64), dates, and data blobs.
	d := Dict{
		"CFBundleIdentifier":               "com.example.test",
		"CFBundleDisplayName":              "TestApp",
		"UISupportedInterfaceOrientations": []any{"UIInterfaceOrientationPortrait"},
		"Nested":                           Dict{"key": "value"},
		"HasData":                          []byte{0x01, 0x02, 0x03},
		"Enabled":                          true,
		"BuildNumber":                      uint64(42),
		"BuildDate":                        time.Date(2020, 6, 1, 12, 0, 0, 0, time.UTC),
	}
	data, err := Encode(d)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	if !Equal(d, got) {
		t.Errorf("round trip mismatch:\ngot  %#v\nwant %#v", got, d)
	}
}

func TestWriteAndOpen(t *testing.T) {
	d := Dict{"CFBundleIdentifier": "com.example.test", "Enabled": true}
	p := filepath.Join(t.TempDir(), "Info.plist")
	if err := Write(p, d); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := Open(p)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if !Equal(d, got) {
		t.Errorf("write/open mismatch:\ngot  %#v\nwant %#v", got, d)
	}
}

func TestWriteXML(t *testing.T) {
	d := Dict{"CFBundleIdentifier": "com.example.test"}
	p := filepath.Join(t.TempDir(), "Info.plist")
	if err := WriteXML(p, d); err != nil {
		t.Fatalf("WriteXML() error = %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(data)
	if err != nil {
		t.Fatalf("Decode() of XML output error = %v", err)
	}
	if !Equal(d, got) {
		t.Errorf("xml round trip mismatch:\ngot  %#v\nwant %#v", got, d)
	}
}

func TestConvertToXML1BinaryRoundTrip(t *testing.T) {
	// A real binary plist (marshaled, not hand-rolled bytes): dict with a
	// string plus an inner array.
	src, err := howett.Marshal(Dict{"key1": "value", "arr": []any{"a", "b"}}, howett.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "Info.plist")
	if err := os.WriteFile(path, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ConvertToXML1(path); err != nil {
		t.Fatalf("binary plist must convert: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "<?xml") {
		t.Fatalf("output is not xml1: %q", data[:40])
	}
	d, err := Decode(data)
	if err != nil {
		t.Fatalf("converted output must re-parse: %v", err)
	}
	if d["key1"] != "value" {
		t.Fatalf("string value lost: %#v", d)
	}
	arr, ok := d["arr"].([]any)
	if !ok || len(arr) != 2 || arr[0] != "a" {
		t.Fatalf("array value lost: %#v", d["arr"])
	}
}

func TestConvertToXML1RejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.plist")
	if err := os.WriteFile(path, []byte("\x00\x01this is not a plist"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ConvertToXML1(path)
	if err == nil || !strings.Contains(err.Error(), "not a valid plist") {
		t.Fatalf("garbage must fail with the plutil-style message, got %v", err)
	}
}

func TestConvertToXML1PreservesTypes(t *testing.T) {
	src := filepath.Join(t.TempDir(), "a.plist")
	// howett decodes plist integers as uint64; write the same shape we
	// expect to read back so DeepEqual is exact.
	d := Dict{"name": "Tweak", "count": uint64(3), "enabled": true, "tags": []any{"a", "b"}}
	if err := Write(src, d); err != nil {
		t.Fatal(err)
	}
	if err := ConvertToXML1(src); err != nil {
		t.Fatal(err)
	}
	got, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if !Equal(got, d) {
		t.Fatalf("round-trip drifted:\n got  %#v\n want %#v", got, d)
	}
}
