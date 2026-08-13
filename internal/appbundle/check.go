package appbundle

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// MissingRef is one unresolved bundle-relative dependency. Suspected marks a
// tier-2 (dlopen-string heuristic) finding — a bare NAME.framework token in a
// binary that the app doesn't ship — which is a likely runtime crash but not
// as certain as a tier-1 load command.
type MissingRef struct {
	Dep       string // e.g. @rpath/ffmpegkit.framework/ffmpegkit, or suspected dlopen of X
	From      string // the binary referencing it, relative to the bundle root
	Suspected bool   // tier-2 dlopen heuristic (vs a deterministic load command)
}

// swiftRuntime is the set of OS-provided Swift shims. On iOS 12.2+ dyld
// resolves @rpath/libswift*.dylib via the OS's /usr/lib/swift, so apps never
// bundle them — flagging them would be a false positive on every Swift app.
// systemFrameworksLower is systemFrameworks lowercased, built once so tier-2
// lookups can be case-insensitive (a dlopen string may spell a system
// framework in any case) without a per-finding linear scan.
var systemFrameworksLower = func() map[string]bool {
	m := make(map[string]bool, len(systemFrameworks))
	for k := range systemFrameworks {
		m[strings.ToLower(k)] = true
	}
	return m
}()

var swiftRuntime = []string{
	"@rpath/libswiftCore.dylib",
	"@rpath/libswiftCoreFoundation.dylib",
	"@rpath/libswiftDarwin.dylib",
	"@rpath/libswiftDispatch.dylib",
	"@rpath/libswiftFoundation.dylib",
	"@rpath/libswiftObjectiveC.dylib",
	"@rpath/libswiftos.dylib",
	"@rpath/libswiftCoreGraphics.dylib",
	"@rpath/libswiftCoreImage.dylib",
	"@rpath/libswiftQuartzCore.dylib",
	"@rpath/libswiftUIKit.dylib",
	"@rpath/libswiftMetal.dylib",
	"@rpath/libswiftModelIO.dylib",
	"@rpath/libswiftCoreData.dylib",
	"@rpath/libswiftAVFoundation.dylib",
	"@rpath/libswiftNetwork.dylib",
	"@rpath/libswiftCombine.dylib",
}

func isSwiftRuntime(dep string) bool {
	for _, s := range swiftRuntime {
		if dep == s {
			return true
		}
	}
	return strings.HasPrefix(dep, "@rpath/libswift") && strings.HasSuffix(dep, ".dylib")
}

// systemFrameworks are Apple/OS frameworks that resolve via the OS, so a bare
// NAME.framework token for one of them is not a missing-bundle dependency.
// The list is curated (the common dlopen'd set); tier-2 findings are heuristic
// anyway, so an unlisted system framework only produces a warning.
var systemFrameworks = map[string]bool{
	"UIKit.framework": true, "Foundation.framework": true, "CoreFoundation.framework": true,
	"CoreGraphics.framework": true, "QuartzCore.framework": true, "CoreText.framework": true,
	"CoreImage.framework": true, "CoreAnimation.framework": true, "CoreData.framework": true,
	"CoreServices.framework": true, "CoreSpotlight.framework": true, "CoreTelephony.framework": true,
	"CoreLocation.framework": true, "CoreMotion.framework": true, "CoreBluetooth.framework": true,
	"CoreHaptics.framework": true, "CoreML.framework": true, "CoreMedia.framework": true,
	"CoreVideo.framework": true, "CoreAudio.framework": true, "CoreAudioKit.framework": true,
	"AVFoundation.framework": true, "AVFAudio.framework": true, "AVKit.framework": true,
	"MediaPlayer.framework": true, "MediaToolbox.framework": true, "VideoToolbox.framework": true,
	"AudioToolbox.framework": true, "Security.framework": true, "CFNetwork.framework": true,
	"SystemConfiguration.framework": true, "Network.framework": true, "NetworkExtension.framework": true,
	"Combine.framework": true, "SwiftUI.framework": true, "WebKit.framework": true,
	"JavaScriptCore.framework": true, "StoreKit.framework": true, "Photos.framework": true,
	"PhotosUI.framework": true, "Contacts.framework": true, "ContactsUI.framework": true,
	"MapKit.framework": true, "AddressBook.framework": true, "AddressBookUI.framework": true,
	"EventKit.framework": true, "EventKitUI.framework": true, "UserNotifications.framework": true,
	"UserNotificationsUI.framework": true, "NotificationCenter.framework": true, "LocalAuthentication.framework": true,
	"AuthenticationServices.framework": true, "CryptoKit.framework": true, "Accelerate.framework": true,
	"Metal.framework": true, "MetalKit.framework": true, "MetalPerformanceShaders.framework": true,
	"ImageIO.framework": true, "GLKit.framework": true, "OpenGLES.framework": true,
	"SpriteKit.framework": true, "SceneKit.framework": true, "GameKit.framework": true,
	"GameController.framework": true, "ExternalAccessory.framework": true, "HomeKit.framework": true,
	"HealthKit.framework": true, "AdSupport.framework": true, "PassKit.framework": true,
	"ReplayKit.framework": true, "Social.framework": true, "Accounts.framework": true,
	"WidgetKit.framework": true, "ActivityKit.framework": true, "SafariServices.framework": true,
	"PDFKit.framework": true, "QuickLook.framework": true, "QuickLookThumbnailing.framework": true,
	"UniformTypeIdentifiers.framework": true, "Vision.framework": true, "VisionKit.framework": true,
	"NaturalLanguage.framework": true, "Speech.framework": true, "SoundAnalysis.framework": true,
	"ScreenTime.framework": true, "FamilyControls.framework": true, "ManagedSettings.framework": true,
	"DeviceActivity.framework": true, "Intents.framework": true, "IntentsUI.framework": true,
	"ClassKit.framework": true, "CarPlay.framework": true, "CarKit.framework": true,
	"AppTrackingTransparency.framework": true, "BackgroundTasks.framework": true, "PencilKit.framework": true,
	"FileProvider.framework": true, "FileProviderUI.framework": true, "LinkPresentation.framework": true,
	"MetricKit.framework": true, "OSLog.framework": true, "PushKit.framework": true,
	"CallKit.framework": true, "Messages.framework": true, "MessageUI.framework": true,
	"WatchConnectivity.framework": true, "WatchKit.framework": true, "iAd.framework": true,
	"GSS.framework": true, "IOKit.framework": true, "MobileCoreServices.framework": true,
	"ObjectiveC.framework": true, "libobjc.framework": true, "libcrypto.framework": true,
	"libssl.framework": true, "libz.framework": true, "libxml2.framework": true,
	"libsqlite3.framework": true, "libcurl.framework": true, "libiconv.framework": true,
}

// CheckReferences reports every bundle-relative dependency that does not
// resolve inside the bundle. Two tiers:
//
// Tier 1 (deterministic): load-command dependencies under @rpath/,
// @executable_path/, @loader_path/ whose target is absent. OS-provided Swift
// runtime shims (@rpath/libswift*.dylib) are exempt — dyld resolves those via
// the OS. This is the classic "tweak references a framework the merge didn't
// ship" detector.
//
// Tier 2 (heuristic, Suspected=true): bare NAME.framework tokens in the
// strings of non-main binaries — the runtime-dlopen signature (RyukGram's
// settings UI dlopens "ffmpegkit.framework" by string, with no load command).
// Tokens inside paths (@rpath/, /System/...), and system frameworks the OS
// provides, are exempt.
func (b *Bundle) CheckReferences() ([]MissingRef, error) {
	var refs []MissingRef
	for _, bin := range b.executables() {
		deps, err := bin.Dependencies()
		if err != nil {
			continue // not a parseable Mach-O (placeholder fixtures, scripts)
		}
		contextDir := b.Path
		if i := strings.Index(bin.Path, ".appex"); i >= 0 {
			contextDir = filepath.Dir(bin.Path[:i+len(".appex")])
		}
		for _, dep := range deps {
			if isSwiftRuntime(dep) {
				continue
			}
			target, dir, ok := resolveBundleRef(dep, bin.Path, b.Path, contextDir)
			if !ok {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, target)); err != nil {
				refs = append(refs, MissingRef{Dep: dep, From: relPath(b.Path, bin.Path)})
			}
		}
	}

	// Tier 2: bare .framework tokens (runtime dlopen) in non-main binaries.
	reach := buildReachability(b.Path)
	for _, f := range machoFiles(b.Path) {
		if f == b.Main.Path {
			continue // the app's own binary legitimately names many frameworks
		}
		reachable := reach.reachable(tweakFamily(b.Path, f))
		for _, tok := range bareFrameworkTokens(f) {
			name := tok + ".framework"
			// Case-insensitive: a dlopen string may spell a shipped/system
			// framework in any case (FFmpegKit vs ffmpegkit); the on-disk
			// inventory is lowercased, so normalize the token too.
			if reachable[strings.ToLower(name)] || systemFrameworksLower[strings.ToLower(name)] {
				continue
			}
			refs = append(refs, MissingRef{
				Dep:       "suspected runtime dlopen of " + name,
				From:      relPath(b.Path, f),
				Suspected: true,
			})
		}
	}
	return refs, nil
}

// reachability is the precomputed dlopen-reachability model for one bundle:
// frameworks at Frameworks/ and the app root are reachable from any binary,
// while frameworks nested inside a .bundle/.appex are reachable only from
// binaries of the SAME tweak family (the bundle's stem). Built once with a
// single tree walk and queried per binary — a 364MB app with dozens of
// binaries must not be walked once per binary.
type reachability struct {
	always map[string]bool            // lowercase NAME.framework, reachable from any binary
	byFam  map[string]map[string]bool // lowercase NAME.framework, by tweak family
}

func buildReachability(bundlePath string) *reachability {
	r := &reachability{always: map[string]bool{}, byFam: map[string]map[string]bool{}}
	_ = filepath.WalkDir(bundlePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || !strings.HasSuffix(d.Name(), ".framework") {
			return nil
		}
		rel, err := filepath.Rel(bundlePath, path)
		if err != nil {
			return nil
		}
		first, _, _ := strings.Cut(rel, string(filepath.Separator))
		name := strings.ToLower(d.Name())
		if first == "Frameworks" || !strings.Contains(rel, string(filepath.Separator)) {
			r.always[name] = true // Frameworks/ or app root: reachable from anywhere
			return nil
		}
		if strings.HasSuffix(first, ".bundle") || strings.HasSuffix(first, ".appex") {
			fam := strings.TrimSuffix(first, filepath.Ext(first))
			if r.byFam[fam] == nil {
				r.byFam[fam] = map[string]bool{}
			}
			r.byFam[fam][name] = true // same-family bundle: reachable
		}
		return nil
	})
	return r
}

// reachable returns the lowercase framework-name set a dlopen by a binary of
// the given tweak family can reach.
func (r *reachability) reachable(fam string) map[string]bool {
	out := make(map[string]bool, len(r.always)+len(r.byFam[fam]))
	for k := range r.always {
		out[k] = true
	}
	for k := range r.byFam[fam] {
		out[k] = true
	}
	return out
}

// tweakFamily returns the logical tweak family of a binary: the stem of the
// top-level .bundle/.appex it lives in (Regram.bundle/FLEX.framework/FLEX →
// "Regram"), or the stem of a root-level <Name>.dylib (Regram.dylib →
// "Regram"). Binaries with no family marker get "".
func tweakFamily(bundlePath, binPath string) string {
	rel, err := filepath.Rel(bundlePath, binPath)
	if err != nil {
		return ""
	}
	for _, p := range strings.Split(rel, string(filepath.Separator)) {
		if strings.HasSuffix(p, ".bundle") || strings.HasSuffix(p, ".appex") {
			return strings.TrimSuffix(p, filepath.Ext(p))
		}
	}
	if !strings.Contains(rel, string(filepath.Separator)) {
		return strings.TrimSuffix(filepath.Base(binPath), filepath.Ext(binPath))
	}
	return ""
}

// machoFiles returns every regular file under bundlePath that begins with a
// Mach-O magic (fat, thin 32/64, either endianness).
func machoFiles(bundlePath string) []string {
	var out []string
	_ = filepath.WalkDir(bundlePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		magic := make([]byte, 4)
		_, _ = f.Read(magic)
		f.Close()
		if isMachOMagic(magic) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func isMachOMagic(m []byte) bool {
	switch {
	case m[0] == 0xfe && m[1] == 0xed && m[2] == 0xfa && (m[3] == 0xce || m[3] == 0xcf):
		return true
	case m[3] == 0xfe && m[2] == 0xed && m[1] == 0xfa && (m[0] == 0xce || m[0] == 0xcf):
		return true
	case m[0] == 0xca && m[1] == 0xfe && m[2] == 0xba && m[3] == 0xbe:
		return true
	case m[3] == 0xca && m[2] == 0xfe && m[1] == 0xba && m[0] == 0xbe:
		return true
	case m[0] == 0xcf && m[1] == 0xfa && m[2] == 0xed && m[3] == 0xfe:
		return true
	case m[0] == 0xce && m[1] == 0xfa && m[2] == 0xed && m[3] == 0xfe:
		return true
	}
	return false
}

var frameworkToken = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_]*\.framework`)

// bareFrameworkTokens returns the deduped, case-sensitive bare NAME.framework
// tokens found in the file's printable strings — tokens that are not part of
// a path (@rpath/, /System/...), i.e. the runtime-dlopen signature.
func bareFrameworkTokens(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, s := range printableStrings(data) {
		for _, loc := range frameworkToken.FindAllStringIndex(s, -1) {
			i := loc[0]
			if i > 0 {
				switch s[i-1] {
				case '/', '@', '.', '-', '_', ':', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
					continue // part of a path or a longer token
				case 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h', 'i', 'j', 'k', 'l', 'm', 'n', 'o', 'p', 'q', 'r', 's', 't', 'u', 'v', 'w', 'x', 'y', 'z',
					'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'J', 'K', 'L', 'M', 'N', 'O', 'P', 'Q', 'R', 'S', 'T', 'U', 'V', 'W', 'X', 'Y', 'Z':
					continue
				}
			}
			// Strip the .framework suffix: callers re-append it, so a token must
			// be the bare name (ffmpegkit, not ffmpegkit.framework) or the
			// shipped/system lookups would never match.
			tok := strings.TrimSuffix(s[i:loc[1]], ".framework")
			if !seen[tok] {
				seen[tok] = true
				out = append(out, tok)
			}
		}
	}
	return out
}

// printableStrings extracts printable ASCII runs of length >= 4, like the
// `strings` utility (the -n 4 equivalent).
func printableStrings(data []byte) []string {
	var out []string
	start := -1
	for i, c := range data {
		if c >= 0x20 && c <= 0x7e {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= 4 {
			out = append(out, string(data[start:i]))
		}
		start = -1
	}
	if start >= 0 && len(data)-start >= 4 {
		out = append(out, string(data[start:]))
	}
	return out
}

// resolveBundleRef classifies a dependency into (target basename, candidate
// directory, true) when it is bundle-relative, or ("", "", false) for system
// paths and anything else. A leading Frameworks/ component under
// @executable_path maps to the bundle's Frameworks dir.
func resolveBundleRef(dep, binPath, bundlePath, appexContext string) (string, string, bool) {
	switch {
	case strings.HasPrefix(dep, "@rpath/"):
		return firstComponent(strings.TrimPrefix(dep, "@rpath/")), filepath.Join(bundlePath, "Frameworks"), true
	case strings.HasPrefix(dep, "@executable_path/"):
		rest := strings.TrimPrefix(dep, "@executable_path/")
		if first, _, _ := strings.Cut(rest, "/"); first == "Frameworks" {
			return secondComponent(rest), filepath.Join(bundlePath, "Frameworks"), true
		}
		return firstComponent(rest), appexContext, true
	case strings.HasPrefix(dep, "@loader_path/"):
		return firstComponent(strings.TrimPrefix(dep, "@loader_path/")), filepath.Dir(binPath), true
	default:
		return "", "", false
	}
}

func firstComponent(p string) string {
	head, _, _ := strings.Cut(p, "/")
	return head
}

func secondComponent(p string) string {
	_, rest, _ := strings.Cut(p, "/")
	head, _, _ := strings.Cut(rest, "/")
	return head
}

func relPath(base, path string) string {
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return path
	}
	return rel
}
