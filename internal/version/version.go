// Package version reports what this binary is and what it was built against.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release of torrent-tracker.
const Version = "1.2.0"

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

// UserAgent identifies us to the HTTP trackers we probe, the registries we
// query and the tracker lists we fetch.
const UserAgent = "torrent-tracker/1.2 (+https://github.com/pawal/torrent-tracker)"
