// Package bootimage names the OCI repository that holds the boot
// artifact (UKI + sd-boot + loader.conf): the target for cmd/build.go's
// push, and the source for internal/install's pull. Both the build tool
// and the compiled appliance binary need to agree on the same
// repository and TLS posture, so both read it from this package rather
// than each hardcoding their own copy.
package bootimage

// Repository is the OCI repository holding the boot artifact. Set at
// build time via -ldflags, so the binary that pulls it (compiled by the
// same build that pushed it) always points at the right place.
var Repository = "ghcr.io/invarios/esp"

// Insecure allows plain HTTP against Repository, for a local test
// registry. Set at build time via -ldflags, as the literal string
// "true" or "false".
var Insecure = "false"

// Tag returns the OCI tag for a given version and architecture (an
// OCI/Go arch string, e.g. "amd64"), e.g. "v0.1.0-amd64". The arch
// suffix keeps same-version builds for different architectures from
// clobbering each other's tag: Push writes a single-platform image,
// not a multi-arch index, so there's nothing else distinguishing them.
func Tag(version, arch string) string {
	return version + "-" + arch
}

// Ref returns the full pull/push reference for version and arch.
func Ref(version, arch string) string {
	return Repository + ":" + Tag(version, arch)
}

// InsecureBool reports whether Insecure is set to "true".
func InsecureBool() bool {
	return Insecure == "true"
}
