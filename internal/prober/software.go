package prober

import "strings"

// software names the implementation behind a fingerprint. Nothing here is
// written to the database: naming a cluster is a guess, so the table is applied
// on read and every stored row is reinterpreted when it changes.
//
// Every entry was matched against the string in that project's source, cited
// beside it. Most fingerprints still stay unnamed on purpose: a failure text is
// a literal from somebody's source, but the generic ones were copied between
// projects, so they gather trackers that answer alike without saying what any
// of them runs.
var software = map[string]string{
	// ot_http.c, and the only bencoded failure opentracker has.
	"Your client forgot to send your torrent's info_hash. Please upgrade your client.": "opentracker",
	// frontend/http/parser.go, for announce and scrape alike.
	"no info_hash parameter supplied": "Chihaya",
	// track.py, so also the original bttrack this descends from.
	"Requested download is not authorized for use with this tracker.": "BitTornado",
	// lib/server/parse-http.js, thrown for an absent info_hash too.
	"invalid info_hash": "bittorrent-tracker",
	// jpopsuki.eu answers this and sets "Server: Ocelot 1.0" — the tracker
	// naming itself in the one header a front end had not overwritten.
	"Malformed announce": "Ocelot",
}

// softwareMarkers name an implementation whose failure text carries the request
// in it, which no exact key could match. First hit wins, so a marker must be
// specific enough to belong to one project.
var softwareMarkers = []struct{ marker, name string }{
	// http-core/src/services/error_mapping.rs wraps the info_hash and a source
	// path after this prefix.
	{"Tracker whitelist error: ", "Torrust"},
}

// Software names the implementation a signature belongs to, or "" when the
// signature only groups replies that look alike.
func Software(sig string) string {
	if name := software[sig]; name != "" {
		return name
	}
	for _, m := range softwareMarkers {
		if strings.Contains(sig, m.marker) {
			return m.name
		}
	}
	return ""
}

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
