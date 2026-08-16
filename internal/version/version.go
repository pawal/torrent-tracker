// Package version reports what this binary is and what it was built against.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release of torrent-tracker.
const Version = "1.0.0"

// DNSLib returns the version of the miekg/dns module this binary was built
// with, or "" when the build information is unavailable (as with `go run`).
// The module has lived at more than one host, so match on the tail.
func DNSLib() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range info.Deps {
		if strings.HasSuffix(dep.Path, "miekg/dns") {
			return dep.Version
		}
	}
	return ""
}
