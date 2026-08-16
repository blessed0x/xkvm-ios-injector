package decrypt

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// withTempConfig redirects the config dir to a temp dir for the duration of
// the test, so persistence tests never touch the real user config.
func withTempConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := userConfigDir
	userConfigDir = func() (string, error) { return dir, nil }
	t.Cleanup(func() { userConfigDir = old })
}

func TestAuthRoundTrip(t *testing.T) {
	withTempConfig(t)

	c := New("test@example.com", "hunter2")
	c.Pod = "42"
	c.AccountName = "Jane Doe"
	c.headers = map[string]string{"X-Dsid": "1", "X-Token": "tok"}
	c.jar.cookies = []http.Cookie{{Name: "n", Value: "v"}}

	if err := c.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	path, _ := authPath()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("auth file: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("auth file mode = %o, want 600", fi.Mode().Perm())
	}

	restored := New("", "")
	if err := restored.Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !restored.HasAuth() {
		t.Fatal("HasAuth() false after Load")
	}
	if restored.AppleID != "test@example.com" || restored.Pod != "42" || restored.AccountName != "Jane Doe" {
		t.Errorf("restored identity = %q/%q/%q", restored.AppleID, restored.Pod, restored.AccountName)
	}
	if restored.headers["X-Token"] != "tok" {
		t.Errorf("restored headers = %v", restored.headers)
	}
	if len(restored.jar.cookies) != 1 || restored.jar.cookies[0].Name != "n" {
		t.Errorf("restored cookies = %v", restored.jar.cookies)
	}
}

func TestLogout(t *testing.T) {
	withTempConfig(t)

	c := New("a@b.c", "pw")
	c.headers = map[string]string{"X-Dsid": "1"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if !HasSavedAuth() {
		t.Fatal("HasSavedAuth() false after save")
	}
	if err := Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if HasSavedAuth() {
		t.Fatal("HasSavedAuth() true after logout")
	}
	// Logging out when nothing is saved is a no-op, not an error.
	if err := Logout(); err != nil {
		t.Fatalf("second Logout: %v", err)
	}
}

func TestPrefsRoundTrip(t *testing.T) {
	withTempConfig(t)

	// Defaults when nothing is saved.
	p, err := LoadPrefs()
	if err != nil {
		t.Fatalf("LoadPrefs empty: %v", err)
	}
	if p.AskMode != AskModeAsk || p.OutputDir != "" {
		t.Errorf("empty prefs = %+v, want ask/none", p)
	}

	p = Prefs{OutputDir: filepath.Join(t.TempDir(), "out"), AskMode: AskModeNever}
	if err := SavePrefs(p); err != nil {
		t.Fatalf("SavePrefs: %v", err)
	}
	got, err := LoadPrefs()
	if err != nil {
		t.Fatalf("LoadPrefs: %v", err)
	}
	if got.AskMode != AskModeNever || got.OutputDir != p.OutputDir {
		t.Errorf("round-tripped prefs = %+v, want %+v", got, p)
	}
}
