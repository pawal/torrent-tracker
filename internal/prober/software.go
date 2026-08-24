package prober

import "strings"

// software names the implementation behind a fingerprint. Nothing here is
// written to the database: naming a cluster is a guess, so the table is applied
// on read and every stored row is reinterpreted when it changes.
//
// Most fingerprints stay unnamed on purpose. A failure text is a literal from
// somebody's source, but the generic ones were copied between projects, so they
// gather trackers that answer alike without saying what any of them runs.
var software = map[string]string{
	"no info_hash parameter supplied": "opentracker",
	// jpopsuki.eu answers this and sets "Server: Ocelot 1.0" — the tracker
	// naming itself in the one header a front end had not overwritten.
	"Malformed announce": "Ocelot",
}

// Software names the implementation a signature belongs to, or "" when the
// signature only groups replies that look alike.
func Software(sig string) string { return software[sig] }

// serverSoftware are the Server headers a tracker wrote itself. Any other
// header names the front end: nginx and the CDNs overwrite whatever the tracker
// set, and "cloudflare" is on 88 of the live probes without identifying one.
var serverSoftware = []string{"Ocelot"}

// ServerSoftware names the tracker software a Server header discloses, or ""
// for a header that names a front end.
func ServerSoftware(server string) string {
	name, _, _ := strings.Cut(server, " ") // "Ocelot 1.0"
	for _, s := range serverSoftware {
		if strings.EqualFold(name, s) {
			return s
		}
	}
	return ""
}
