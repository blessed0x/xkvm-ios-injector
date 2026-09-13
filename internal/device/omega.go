package device

import (
	"crypto/sha1"
	_ "embed"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"howett.net/plist"
)

// Omega: the jailbreak.party "blacklist remover" — clears the app-revoke
// and certificate-validity databases via a partial mobilebackup2 restore.
// Ported from https://github.com/jailbreakdotparty/Omega (MIT, 2026
// jailbreak.party) and its vendored sparserestore: the same domains, paths,
// record modes, and skip-setup plists, rebuilt in Go over go-ios.

//go:embed omega_files/CloudConfigurationDetails.plist
var cloudConfigPl []byte

//go:embed omega_files/com.apple.purplebuddy.plist
var purpleBuddyPl []byte

//go:embed omega_files/BackupKeyBag.txt
var backupKeyBagB64 string

// omegaFiles is the restore set, in Omega's exact order. Directory records
// replace the revoke + cert databases with empty directories (writes to
// them then fail, so the device can never "remember" a revoke again); the
// two concrete files skip setup.
//   - DatabaseDomain: MobileIdentityData Rejections/AuthListBanned*
//   - ProtectedDomain: trustd valid.sqlite3 (+shm/-wal)
var omegaFiles = []struct {
	domain, path string
	contents     []byte // nil => Directory record
}{
	{"DatabaseDomain", "", nil},
	{"DatabaseDomain", "MobileIdentityData", nil},
	{"DatabaseDomain", "MobileIdentityData/Rejections.plist", nil},
	{"DatabaseDomain", "MobileIdentityData/AuthListBannedUpps.plist", nil},
	{"DatabaseDomain", "MobileIdentityData/AuthListBannedCdHashes.plist", nil},
	{"ProtectedDomain", "", nil},
	{"ProtectedDomain", "trustd", nil},
	{"ProtectedDomain", "trustd/valid.sqlite3", nil},
	{"ProtectedDomain", "trustd/valid.sqlite3-shm", nil},
	{"ProtectedDomain", "trustd/valid.sqlite3-wal", nil},
	{"SysSharedContainerDomain-systemgroup.com.apple.configurationprofiles", "", nil},
	{"SysSharedContainerDomain-systemgroup.com.apple.configurationprofiles", "Library", nil},
	{"SysSharedContainerDomain-systemgroup.com.apple.configurationprofiles", "Library/ConfigurationProfiles", nil},
	{"SysSharedContainerDomain-systemgroup.com.apple.configurationprofiles", "Library/ConfigurationProfiles/CloudConfigurationDetails.plist", cloudConfigPl},
	{"ManagedPreferencesDomain", "", nil},
	{"ManagedPreferencesDomain", "mobile", nil},
	{"ManagedPreferencesDomain", "mobile/com.apple.purplebuddy.plist", purpleBuddyPl},
}

// omegaBackup bundles the four manifest files + payload blobs of the
// partial backup, in memory (the restore is small: a few plists).
type omegaBackup struct {
	mbdb     []byte // Manifest.mbdb
	status   []byte // Status.plist
	manifest []byte // Manifest.plist
	info     []byte // Info.plist
	payloads map[string][]byte
}

func buildOmegaBackup() (omegaBackup, error) {
	now := uint32(time.Now().Unix())
	// The keybag literal is line-wrapped; strip whitespace first (python's
	// b64decode is whitespace-tolerant, Go's is strict).
	compact := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, backupKeyBagB64)
	keybag, err := base64.StdEncoding.DecodeString(compact)
	if err != nil {
		return omegaBackup{}, fmt.Errorf("decoding backup keybag: %w", err)
	}
	var records []mbdbRecord
	payloads := make(map[string][]byte)
	for _, f := range omegaFiles {
		rec := mbdbRecord{
			Domain:   f.domain,
			Filename: f.path,
			Mode:     mbdbModeDir,
			UserID:   0,
			GroupID:  0,
			MTime:    now,
			ATime:    now,
			CTime:    now,
			Flags:    mbdbFlags,
		}
		if f.contents == nil {
			rec.Mode = mbdbModeDir
		} else {
			rec.Mode = mbdbModeFile
			rec.Inode = randInode()
			rec.Size = uint64(len(f.contents))
			rec.Hash = sha1Sum(f.contents)
			payloads[payloadName(f.domain, f.path)] = f.contents
		}
		records = append(records, rec)
	}
	status, err := xmlPlist(map[string]any{
		"BackupState":   "new",
		"Date":          time.Unix(0, 0).UTC(),
		"IsFullBackup":  false,
		"SnapshotState": "finished",
		"UUID":          "00000000-0000-0000-0000-000000000000",
		"Version":       "2.4",
	})
	if err != nil {
		return omegaBackup{}, err
	}
	manifest, err := xmlPlist(map[string]any{
		"BackupKeyBag":         keybag,
		"Lockdown":             map[string]any{},
		"SystemDomainsVersion": "20.0",
		"Version":              "9.1",
	})
	if err != nil {
		return omegaBackup{}, err
	}
	info, err := xmlPlist(map[string]any{})
	if err != nil {
		return omegaBackup{}, err
	}
	return omegaBackup{
		mbdb:     manifestDB(records),
		status:   status,
		manifest: manifest,
		info:     info,
		payloads: payloads,
	}, nil
}

func xmlPlist(v any) ([]byte, error) {
	return plist.Marshal(v, plist.XMLFormat)
}

func sha1Sum(b []byte) []byte {
	h := sha1.New()
	h.Write(b)
	return h.Sum(nil)
}
