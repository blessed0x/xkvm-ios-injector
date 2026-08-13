package log

import (
	"bytes"
	"strings"
	"testing"
)

func TestFormatsAndSilence(t *testing.T) {
	var info, errw bytes.Buffer
	SetWriters(&info, &errw)

	Infof("hello %s", "world")
	Warnf("careful")
	Promptf("input?")
	Errorf("boom")

	for _, want := range []string{"[*] hello world", "[?] careful", "[<] input?", "[!] boom"} {
		if !strings.Contains(info.String()+errw.String(), want) {
			t.Errorf("output does not contain %q\ninfo=%q\nerr=%q", want, info.String(), errw.String())
		}
	}
	if !strings.Contains(errw.String(), "[!] boom") {
		t.Errorf("errors should go to the error writer, got %q", errw.String())
	}

	info.Reset()
	errw.Reset()
	SetSilent(true)
	Infof("hidden")
	Warnf("hidden too")
	Errorf("still shown")
	SetSilent(false)

	if info.Len() != 0 {
		t.Errorf("silent mode leaked info output: %q", info.String())
	}
	if !strings.Contains(errw.String(), "still shown") {
		t.Errorf("errors must not be silenced, got %q", errw.String())
	}
}
