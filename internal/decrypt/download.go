package decrypt

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/xscope0/xkvm-ios-injector/internal/plist"
)

// Download fetches the app's IPA from the CDN and rewrites it the way
// PancakeStore does: iTunesMetadata.plist at the zip root (metadata plus the
// signing-in Apple ID) and the SC_Info/ .sinf files from the buy response.
// The output is written to outDir and its path returned.
//
// versionID is an external version id from VersionIDs; empty means the
// latest available version.
func (c *Client) Download(ctx context.Context, appID int64, versionID, outDir string) (string, error) {
	if !c.HasAuth() {
		return "", errors.New("not signed in: authenticate before downloading")
	}
	info, err := c.Purchase(ctx, appID, versionID)
	if err != nil {
		return "", err
	}

	tmp, err := os.MkdirTemp("", "xkvm-decrypt-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	zipPath := filepath.Join(tmp, "app.zip")
	if err := c.DownloadToPath(ctx, info.URL, zipPath); err != nil {
		return "", err
	}

	sinfPaths, appMeta, err := inspectZip(zipPath)
	if err != nil {
		return "", err
	}

	// iTunesMetadata.plist is the buy response's metadata (PancakeStore
	// writes that, not the app's Info.plist) plus the Apple ID that owns the
	// download. The app's Info.plist drives the output NAME and the sinf
	// fallback path instead.
	meta := info.Metadata
	if meta == nil {
		meta = appMeta
	}
	if meta == nil {
		meta = map[string]any{}
	}
	meta["apple-id"] = c.AppleID
	meta["userName"] = c.AppleID
	metaXML, err := plist.EncodeXML(meta)
	if err != nil {
		return "", fmt.Errorf("encode iTunesMetadata.plist: %w", err)
	}

	// Pair each Manifest sinf path with the blob from the buy response. When
	// the buy response carries no sinfs, the ones already in the zip (old
	// apps ship them) are left untouched.
	var targets []sinfTarget
	for i, p := range sinfPaths {
		if i < len(info.Sinfs) {
			targets = append(targets, sinfTarget{path: p, blob: info.Sinfs[i]})
		}
	}

	nameMeta := appMeta
	if nameMeta == nil {
		nameMeta = info.Metadata
	}
	out := filepath.Join(outDir, ipaName(nameMeta))
	if err := writeRewrittenZip(zipPath, out, metaXML, targets); err != nil {
		return "", err
	}
	return out, nil
}

// sinfTarget is a sinf file to write: its zip entry path and raw blob.
type sinfTarget struct {
	path string
	blob []byte
}

// inspectZip reads the app's Info.plist + SC_Info/Manifest.plist from the
// downloaded zip and returns the sinf entry paths (zip-relative), the
// Info.plist metadata, and an error for malformed archives.
func inspectZip(zipPath string) ([]string, map[string]any, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not a zipfile: %w", zipPath, err)
	}
	defer zr.Close()

	var appDir string
	meta := map[string]any{}
	for _, f := range zr.File {
		name := f.Name
		if strings.HasSuffix(name, ".app/Info.plist") && !strings.HasSuffix(name, ".appex/Info.plist") {
			// Payload/X.app/Info.plist
			idx := strings.Index(name, ".app/Info.plist")
			appDir = name[:idx+len(".app")]
			rc, err := f.Open()
			if err != nil {
				return nil, nil, err
			}
			data, err := io.ReadAll(io.LimitReader(rc, 4<<20))
			rc.Close()
			if err != nil {
				return nil, nil, err
			}
			if d, err := plist.Decode(data); err == nil {
				meta = d
			}
			break
		}
	}
	if appDir == "" {
		return nil, nil, errors.New("downloaded zip has no app: not a valid IPA")
	}

	// SinfPaths from the manifest, when present.
	manifestName := appDir + "/SC_Info/Manifest.plist"
	for _, f := range zr.File {
		if f.Name != manifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, nil, err
		}
		data, err := io.ReadAll(io.LimitReader(rc, 1<<20))
		rc.Close()
		if err != nil {
			return nil, nil, err
		}
		m, err := plist.Decode(data)
		if err != nil {
			break
		}
		// A manifest without SinfPaths (or with it as a non-array) falls
		// through to the executable-named fallback; the assertion must not
		// panic on a missing key.
		paths, ok := m["SinfPaths"].([]any)
		if !ok {
			break
		}
		var out []string
		for _, p := range paths {
			if s, ok := p.(string); ok {
				out = append(out, appDir+"/"+s)
			}
		}
		return out, meta, nil
	}

	// Old app without a manifest: SC_Info/<executable>.sinf
	exec, _ := meta["CFBundleExecutable"].(string)
	if exec == "" {
		return nil, nil, errors.New("app Info.plist has no CFBundleExecutable")
	}
	return []string{appDir + "/SC_Info/" + exec + ".sinf"}, meta, nil
}

// writeRewrittenZip copies the downloaded zip into out, replacing
// iTunesMetadata.plist and adding the sinf files.
func writeRewrittenZip(src, out string, metaXML []byte, targets []sinfTarget) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()

	zf, err := os.Create(out)
	if err != nil {
		return err
	}
	defer zf.Close()
	zw := zip.NewWriter(zf)

	for _, f := range zr.File {
		if strings.HasPrefix(f.Name, "__MACOSX/") {
			continue
		}
		if f.Name == "iTunesMetadata.plist" {
			continue // replaced below
		}
		if isSinfTarget(f.Name, targets) {
			continue // replaced below
		}
		if err := copyZipEntry(zw, f); err != nil {
			return err
		}
	}

	// iTunesMetadata.plist at the zip root.
	w, err := zw.Create("iTunesMetadata.plist")
	if err != nil {
		return err
	}
	if _, err := w.Write(metaXML); err != nil {
		return err
	}

	// The sinf blobs, in Manifest order.
	for _, t := range targets {
		w, err := zw.Create(cleanZipName(t.path))
		if err != nil {
			return err
		}
		if _, err := w.Write(t.blob); err != nil {
			return err
		}
	}
	return zw.Close()
}

func isSinfTarget(name string, targets []sinfTarget) bool {
	for _, t := range targets {
		if name == t.path {
			return true
		}
	}
	return false
}

// cleanZipName rejects path traversal so a hostile Manifest can't smuggle an
// entry outside the archive.
func cleanZipName(name string) string {
	name = filepath.ToSlash(name)
	if strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
		return strings.TrimLeft(strings.ReplaceAll(name, "..", "_"), "/")
	}
	return name
}

// copyZipEntry streams one source entry into the new archive.
func copyZipEntry(zw *zip.Writer, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := zw.Create(f.Name)
	if err != nil {
		return err
	}
	_, err = io.Copy(w, rc)
	return err
}

// ipaName derives an output filename from the app's metadata:
// <bundle id>_<version>.ipa, falling back to the adam id.
func ipaName(meta map[string]any) string {
	bundle, _ := meta["CFBundleIdentifier"].(string)
	ver, _ := meta["CFBundleShortVersionString"].(string)
	if bundle == "" || ver == "" {
		bundle, _ = meta["bundleId"].(string)
		ver, _ = meta["bundleVersion"].(string)
	}
	if bundle != "" && ver != "" {
		return sanitizeName(bundle+"_"+ver) + ".ipa"
	}
	if bundle != "" {
		return sanitizeName(bundle) + ".ipa"
	}
	return "app.ipa"
}

func sanitizeName(s string) string {
	repl := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	return repl.Replace(s)
}
