// Script conversion for the rootless direction, ported from rootless-patcher
// (Nightwind, MIT): RPScriptHandler + RPDirectoryScanner.scriptFiles.
//
// Upstream's script handling is TOKEN-based, not sed-based like patch.sh's
// roothide direction: the file is split on the separator set " \n\"={}", and
// every token whose first path component is a bootstrap root is converted
// under /var/jb via the same ConversionRuleset (ShouldConvert/ConvertString).
// This means tokens fire that a space-anchored sed would skip (e.g.
// "$(/sbin/launchctl …)" — the "/sbin/launchctl" token has no leading space)
// and vice versa (a tab-indented path never fires, because tabs are not
// separators and the leading tab makes the first component non-bootstrap).

package rootless

import (
	"bytes"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/log"
)

// controlScriptNames are the dpkg control scripts rootless-patcher treats as
// scripts even without a shebang (RPDirectoryScanner.scriptFiles).
var controlScriptNames = map[string]bool{
	"preinst": true, "postinst": true, "prerm": true, "postrm": true,
	"postrmv": true, "extrainst": true, "triggers": true,
}

// scriptMagicMatch mirrors RPDirectoryScanner._magicMatchesMachO: a DEBIAN
// control script that is actually a Mach-O (MH_MAGIC_64, MH_CIGAM_64,
// FAT_MAGIC, FAT_CIGAM — upstream's exact set, checked on both byte orders)
// is excluded from the control-script branch.
func scriptMagicMatch(data []byte) bool {
	if len(data) < 4 {
		return false
	}
	for _, v := range []uint32{
		binary.LittleEndian.Uint32(data[:4]),
		binary.BigEndian.Uint32(data[:4]),
	} {
		switch v {
		case 0xfeedfacf, 0xcffaedfe, 0xcafebabe, 0xbebafeca:
			return true
		}
	}
	return false
}

// isASCII reports whether every byte is valid 7-bit ASCII. Upstream reads
// script files with NSASCIIStringEncoding, which fails on any non-ASCII byte
// and silently skips the file; a shebang file containing UTF-8 is therefore
// NOT treated as a script.
func isASCII(data []byte) bool {
	for _, b := range data {
		if b >= 0x80 {
			return false
		}
	}
	return true
}

// splitScriptTokens splits s on the separator set " \n\"={}" exactly as
// upstream's componentsSeparatedByCharactersInSet. Note tabs and \r are NOT
// separators (a tab-indented path keeps its leading tab, so it never
// converts).
func splitScriptTokens(s string) []string {
	var toks []string
	start := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\n', '"', '=', '{', '}':
			toks = append(toks, s[start:i])
			start = i + 1
		}
	}
	return append(toks, s[start:])
}

type scriptReplacement struct {
	from string
	to   string
}

// patchScriptForRootless ports RPScriptHandler.convertStringsUsingConversionRuleset:
// tokenize, skip empty and shebang-prefixed tokens, join a token ending in
// '\' with the following token (upstream's line-continuation handling), then
// convert each token via the shared ruleset. Replacements are deduped by
// original (last wins — upstream's NSMutableDictionary), applied in insertion
// order, each guarded by upstream's double-conversion check: an original is
// only replaced when the converted string is not already present in the
// script. This last guard is load-bearing: a script that already contains
// "/var/jb/Library/…" will NOT have its "/Library/…" tokens rewritten.
func patchScriptForRootless(data []byte) []byte {
	s := string(data)
	toks := splitScriptTokens(s)

	var repls []scriptReplacement
	idx := map[string]int{}
	for i := 0; i < len(toks); i++ {
		tok := toks[i]
		if tok == "" || strings.HasPrefix(tok, "#!") {
			continue
		}
		toConvert := tok
		if strings.HasSuffix(toConvert, `\`) && i+1 < len(toks) {
			toConvert = tok + " " + toks[i+1]
		}
		if !ShouldConvert(toConvert) {
			continue
		}
		conv := ConvertString(toConvert)
		if j, ok := idx[toConvert]; ok {
			repls[j].to = conv
		} else {
			idx[toConvert] = len(repls)
			repls = append(repls, scriptReplacement{from: toConvert, to: conv})
		}
	}

	out := s
	for _, r := range repls {
		if !strings.Contains(out, r.to) {
			out = strings.ReplaceAll(out, r.from, r.to)
		}
	}
	return []byte(out)
}

// convertScripts ports RPDirectoryScanner.scriptFiles + the script rewrite
// loop in main.m: it collects every script in the unpacked package — any file
// whose contents are pure ASCII and start with "#!" anywhere in the tree
// (payload and DEBIAN), plus the DEBIAN control scripts
// (preinst/postinst/prerm/postrm/postrmv/extrainst/triggers) even without a
// shebang, skipping Mach-Os — and applies patchScriptForRootless to each,
// preserving the original file mode and mtime (upstream captures attributes
// before rewriting and restores them after).
func convertScripts(dir string) error {
	seen := map[string]bool{}
	var scripts []string

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			log.Warnf("script scan: can't read %s: %v", path, err)
			return nil
		}
		if !isASCII(data) || !bytes.HasPrefix(data, []byte("#!")) {
			return nil
		}
		seen[path] = true
		scripts = append(scripts, path)
		return nil
	})
	if err != nil {
		return err
	}

	// Control-script branch: named scripts in DEBIAN/ even without a shebang.
	debian := filepath.Join(dir, "DEBIAN")
	if entries, err := os.ReadDir(debian); err == nil {
		for _, e := range entries {
			if e.IsDir() || !controlScriptNames[e.Name()] {
				continue
			}
			p := filepath.Join(debian, e.Name())
			if seen[p] {
				continue
			}
			data, err := os.ReadFile(p)
			if err != nil {
				log.Warnf("script scan: can't read %s: %v", p, err)
				continue
			}
			if scriptMagicMatch(data) {
				continue
			}
			scripts = append(scripts, p)
		}
	}

	for _, p := range scripts {
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		out := patchScriptForRootless(data)
		if bytes.Equal(out, data) {
			continue
		}
		rel, _ := filepath.Rel(dir, p)
		log.Infof("rewrote script paths in %s", rel)
		if err := os.WriteFile(p, out, info.Mode().Perm()); err != nil {
			return err
		}
		if err := os.Chtimes(p, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
	}
	return nil
}
