// Package version holds build-time version information and renders
// the appliance's /etc/os-release contents from it.
package version

import (
	"fmt"
	"strings"
)

var (
	// Name is the appliance's display name, embedded in os-release and
	// printed by the version command. Set at build time via -ldflags.
	Name = "InvariOS"
	// Tag is the git tag/describe string this build was produced from,
	// e.g. "v0.1.0-3-gabc1234" or "none" when unset. Set at build time
	// via -ldflags.
	Tag = "none"
	// SHA is the abbreviated git commit this build was produced from.
	// Set at build time via -ldflags.
	SHA = "none"
)

// osReleaseTemplate is the template for /etc/os-release.
const osReleaseTemplate = `NAME="%[1]s"
ID=%[2]s
VERSION_ID=%[3]s
PRETTY_NAME="%[1]s (%[3]s)"
HOME_URL="https://invarios.io/"
`

// Version returns Tag, falling back to SHA when no tag was set at
// build time.
func Version() string {
	if Tag == "" || Tag == "none" {
		return SHA
	}

	return Tag
}

// Short returns "<Name> <Version>", for banners and CLI output.
func Short() string {
	return fmt.Sprintf("%s %s", Name, Version())
}

// OSRelease returns /etc/os-release contents for this binary's build.
func OSRelease() []byte {
	return OSReleaseFor(Name, Version())
}

// OSReleaseFor returns /etc/os-release contents for an arbitrary
// name/version pair. This is used by the build command, which stages a
// rootfs for the appliance binary it just cross-compiled rather than
// for the process currently running.
func OSReleaseFor(name, version string) []byte {
	id := strings.ToLower(strings.ReplaceAll(name, " ", "_"))

	return fmt.Appendf(nil, osReleaseTemplate, name, id, version)
}
