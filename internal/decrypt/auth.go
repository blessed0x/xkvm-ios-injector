package decrypt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
)

// persisted is the on-disk shape of a signed-in session. The password is
// stored at 0600 on purpose: re-authenticating for every download would make
// the flow unusable, and ipatool / PancakeStore persist the same data. It is
// plaintext — treat the auth file as a secret, same as ~/.ssh.
type persisted struct {
	AppleID     string            `json:"appleId"`
	Password    string            `json:"password"`
	GUID        string            `json:"guid"`
	Pod         string            `json:"pod"`
	AccountName string            `json:"accountName"`
	Headers     map[string]string `json:"authHeaders"`
	Cookies     []http.Cookie     `json:"authCookies"`
}

// userConfigDir is the config-directory seam; tests redirect it to a temp
// dir so auth + prefs tests never touch the real user config.
var userConfigDir = os.UserConfigDir

// authPath returns the per-user auth file: os.UserConfigDir()/xkvm/auth.json.
func authPath() (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "xkvm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "auth.json"), nil
}

// Save persists the current session.
func (c *Client) Save() error {
	if !c.HasAuth() {
		return fmt.Errorf("no authenticated session to save")
	}
	p := persisted{
		AppleID:     c.AppleID,
		Password:    c.Password,
		GUID:        c.GUID,
		Pod:         c.Pod,
		AccountName: c.AccountName,
		Headers:     c.headers,
		Cookies:     c.jar.cookies,
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path, err := authPath()
	if err != nil {
		return err
	}
	return writeFile(path, data, 0o600)
}

// Load restores a previously saved session onto the client. A missing file
// is not an error; HasAuth() reports whether anything was loaded.
func (c *Client) Load() error {
	path, err := authPath()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("auth file %s is corrupt: %w", path, err)
	}
	c.AppleID = p.AppleID
	c.Password = p.Password
	c.GUID = p.GUID
	c.Pod = p.Pod
	c.AccountName = p.AccountName
	c.headers = p.Headers
	c.jar = &jar{cookies: p.Cookies}
	return nil
}

// SavedAppleID reports the Apple ID of the saved session, if any. Used by
// the TUI to name the saved session in the keep/change/logout menu.
func SavedAppleID() (string, bool) {
	path, err := authPath()
	if err != nil {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return "", false
	}
	return p.AppleID, p.AppleID != ""
}

// HasSavedAuth reports whether a signed-in session exists on disk.
func HasSavedAuth() bool {
	path, err := authPath()
	if err != nil {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// Logout removes the saved session.
func Logout() error {
	path, err := authPath()
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// AskMode controls what the TUI asks about the output directory.
const (
	AskModeAsk   = "ask"   // ask every time (default)
	AskModeReuse = "reuse" // reuse the last directory
	AskModeNever = "never" // never ask again; always use the saved directory
)

// Prefs are the decrypt-flow preferences: where output goes and whether the
// output-directory question is asked.
type Prefs struct {
	OutputDir string `json:"outputDir"`
	AskMode   string `json:"askMode"`
}

func prefsPath() (string, error) {
	base, err := userConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "xkvm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, "decrypt-prefs.json"), nil
}

// LoadPrefs reads the saved decrypt prefs; a missing file yields the default
// (ask, no directory).
func LoadPrefs() (Prefs, error) {
	path, err := prefsPath()
	if err != nil {
		return Prefs{AskMode: AskModeAsk}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Prefs{AskMode: AskModeAsk}, nil
	}
	if err != nil {
		return Prefs{AskMode: AskModeAsk}, err
	}
	var p Prefs
	if err := json.Unmarshal(data, &p); err != nil {
		return Prefs{AskMode: AskModeAsk}, err
	}
	if p.AskMode == "" {
		p.AskMode = AskModeAsk
	}
	return p, nil
}

// SavePrefs persists the decrypt prefs.
func SavePrefs(p Prefs) error {
	if p.AskMode == "" {
		p.AskMode = AskModeAsk
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path, err := prefsPath()
	if err != nil {
		return err
	}
	return writeFile(path, data, 0o600)
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
