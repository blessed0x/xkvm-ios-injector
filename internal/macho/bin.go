package macho

// Bin wraps a Mach-O binary path with pure-Go operations. The M3 native
// backend (go-macho + pkg/codesign, see native.go) is the only backend: it
// replaced the M2 embedded toolchain (insert_dylib, ldid, lipo, otool,
// install_name_tool), which has been removed — xkvm ships no embedded
// binaries and runs on any GOOS/GOARCH.
type Bin struct {
	Path string
}

// IsEncrypted reports whether the binary has an encrypted (cryptid 1) segment.
func (b Bin) IsEncrypted() (bool, error) {
	return nativeIsEncrypted(b.Path)
}

// Dependencies returns the load-command dylib dependencies. The dylib's own
// LC_ID_DYLIB install name is not an import, so it is naturally excluded.
func (b Bin) Dependencies() ([]string, error) {
	return nativeDependencies(b.Path)
}

// ChangeDependency rewrites a dylib dependency name in every architecture
// slice.
func (b Bin) ChangeDependency(old, new string) error {
	return nativeChangeDependency(b.Path, old, new)
}

// SetInstallName sets the LC_ID_DYLIB install name. Used by tests to build
// fixtures with real dylib IDs.
func (b Bin) SetInstallName(name string) error {
	return nativeSetInstallName(b.Path, name)
}

// AddRpath adds an @rpath entry (a no-op when it is already present).
func (b Bin) AddRpath(rpath string) error {
	return nativeAddRpath(b.Path, rpath)
}

// RemoveSignature removes the code signature so the binary can be edited.
func (b Bin) RemoveSignature() error {
	return nativeRemoveSignature(b.Path)
}

// Fakesign ad-hoc signs the binary with no entitlements, for
// AppSync/TrollStore.
func (b Bin) Fakesign() error {
	return nativeSign(b.Path, nil)
}

// IsSigned reports whether the binary carries a readable code signature.
// go-build binaries carry an empty LC_CODE_SIGNATURE command but no
// CodeDirectory, so this reports false for them.
func (b Bin) IsSigned() bool {
	return nativeIsSigned(b.Path)
}

// ExtractEntitlements returns the binary's current entitlements.
func (b Bin) ExtractEntitlements() ([]byte, error) {
	return nativeExtractEntitlements(b.Path)
}

// SignWithEntitlements ad-hoc signs b with the given entitlements XML.
// pkg/codesign generates ad-hoc requirements itself, so no requirements blob
// is needed (the M2 ldid -Cadhoc -Q path is gone).
func (b Bin) SignWithEntitlements(ents []byte) error {
	return nativeSign(b.Path, ents)
}

// InjectWeak inserts a weak LC_LOAD_DYLIB for dylibPath.
func (b Bin) InjectWeak(dylibPath string) error {
	return nativeInjectWeak(b.Path, dylibPath)
}

// Architectures returns the architectures of the binary.
func (b Bin) Architectures() ([]string, error) {
	return nativeArchitectures(b.Path)
}

// InstallName returns the LC_ID_DYLIB install name of the first architecture
// slice, or "" when the binary has none. Used by the rootless converter to
// rewrite an absolute dylib id under /var/jb.
func (b Bin) InstallName() (string, error) {
	return nativeInstallName(b.Path)
}

// AllDependencies returns every imported dylib name with no path filtering
// (Dependencies applies cyan's /Library/, /usr/lib/, @ starter rule, which
// hides converted /var/jb/... paths). Used by tests to assert the full load
// command set after rootless conversion.
func (b Bin) AllDependencies() ([]string, error) {
	return nativeAllDependencies(b.Path)
}

// IsMachO reports whether the file at path is a Mach-O binary (thin or fat,
// either byte order). Used by the rootless converter to find binaries inside
// a deb payload without relying on extensions.
func IsMachO(path string) (bool, error) {
	return nativeIsMachO(path)
}

// ThintoArm64 thins a fat binary to arm64 in place. Thin arm64 binaries are a
// no-op; fat binaries without arm64 return an error.
func (b Bin) ThintoArm64() error {
	return b.ThinToArch("arm64")
}

// ThinToArch thins a fat binary to the named architecture (e.g. "arm64",
// "arm64e", "x86_64") in place. Thin binaries already of that architecture
// are a no-op; thin binaries of another architecture (or fat binaries lacking
// the requested slice) return an error.
func (b Bin) ThinToArch(arch string) error {
	return nativeThinToArch(b.Path, arch)
}
