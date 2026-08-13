package plist

import (
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
