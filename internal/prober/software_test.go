package prober

import (
	"strings"
	"testing"
)

// A Server header is the front end's to write, and nginx and the CDNs take it.
// Only one we recognise as a tracker names software; the rest identify nothing,
// however specific they look.
func TestServerSoftware(t *testing.T) {
	for _, tt := range []struct{ server, want string }{
		{"Ocelot 1.0", "Ocelot"},
		{"ocelot", "Ocelot"},
		{"cloudflare", ""},
		{"nginx/1.24.0", ""},
		{"Apache/2.4.62 (Debian)", ""},
		{"", ""},
	} {
		if got := ServerSoftware(tt.server); got != tt.want {
			t.Errorf("ServerSoftware(%q) = %q, want %q", tt.server, got, tt.want)
		}
	}
}

// Each of these was read off the string in that project's source, so the test
// is really asserting that nobody has since edited the wrong key. The two
// wordings are worth keeping apart by hand: they sound interchangeable, and
// swapping them once put opentracker's 27 trackers under Chihaya's name.
func TestSoftwareNamesOnlyWhatItCanAttribute(t *testing.T) {
	for _, tt := range []struct{ sig, want string }{
		{"Your client forgot to send your torrent's info_hash. Please upgrade your client.", "opentracker"},
		{"no info_hash parameter supplied", "Chihaya"},
		{"Requested download is not authorized for use with this tracker.", "BitTornado"},
		{"invalid info_hash", "bittorrent-tracker"},
		{"Malformed announce", "Ocelot"},
		// Truncated, so not opentracker's literal and not attributable.
		{"Your client forgot to send your torrent's info_hash", ""},
		{"complete,incomplete,interval,peers", ""},
		{"missing info_hash", ""},
		{"", ""},
	} {
		if got := Software(tt.sig); got != tt.want {
			t.Errorf("Software(%q) = %q, want %q", tt.sig, got, tt.want)
		}
	}
}

// Torrust puts the info_hash and a source path in the text, so its rows differ
// from each other and an exact key would match none of them.
func TestSoftwareMatchesAMarkerInsideTheText(t *testing.T) {
	sig := "Tracker whitelist error: The torrent: " + strings.Repeat("0", 40) +
		", is not whitelisted, packages/tracker-cor"
	if got := Software(sig); got != "Torrust" {
		t.Errorf("Software(Torrust's wording) = %q, want Torrust", got)
	}
	if got := Software("whitelist error"); got != "" {
		t.Errorf("Software(a fragment of the marker) = %q, want none", got)
	}
}
