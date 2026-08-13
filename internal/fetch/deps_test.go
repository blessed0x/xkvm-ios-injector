package fetch

import (
	"reflect"
	"testing"
)

func TestParseDepends(t *testing.T) {
	cases := []struct {
		in   string
		want [][]string
	}{
		{"", nil},
		{"a", [][]string{{"a"}}},
		{"a (>= 1.0)", [][]string{{"a"}}},
		{"a (>= 1.0), b | c (<< 2), d", [][]string{{"a"}, {"b", "c"}, {"d"}}},
		{"mobilesubstrate (>= 0.9.5000), preferenceloader", [][]string{{"mobilesubstrate"}, {"preferenceloader"}}},
		{" a , b  ", [][]string{{"a"}, {"b"}}},
		{"com.example.x | com.example.y (= 2.0)", [][]string{{"com.example.x", "com.example.y"}}},
		{"firmware, com.example.tweak (>= 2.0)", [][]string{{"firmware"}, {"com.example.tweak"}}},
	}
	for _, c := range cases {
		got := parseDepends(c.in)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("parseDepends(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestStripConstraint(t *testing.T) {
	cases := map[string]string{
		"a":            "a",
		"a (>= 1.0)":   "a",
		"a(<= 2)":      "a",
		"  b (<< 0.1)": "b",
		"c (= 1.2.3) ": "c",
	}
	for in, want := range cases {
		if got := stripConstraint(in); got != want {
			t.Errorf("stripConstraint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsCoreDep(t *testing.T) {
	for _, id := range []string{
		"mobilesubstrate", "firmware", "coreutils",
		"com.ex.substitute", "org.coolstar.libhooker",
		"gsc.ipad", "cy+cpu.arm64",
	} {
		if !isCoreDep(id) {
			t.Errorf("%s should be filtered as a core dep", id)
		}
	}
	if isCoreDep("com.example.tweak") {
		t.Error("a regular tweak id must not be treated as core")
	}
}
