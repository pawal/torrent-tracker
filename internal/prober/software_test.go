package prober

import "testing"

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

func TestSoftwareNamesOnlyWhatItCanAttribute(t *testing.T) {
	if got := Software("no info_hash parameter supplied"); got != "opentracker" {
		t.Errorf("Software(opentracker's wording) = %q, want opentracker", got)
	}
	// A literal from somebody's source, copied between projects since.
	if got := Software("Your client forgot to send your torrent's info_hash"); got != "" {
		t.Errorf("Software(shared wording) = %q, want none", got)
	}
	if got := Software("complete,incomplete,interval,peers"); got != "" {
		t.Errorf("Software(a reply shape) = %q, want none", got)
	}
}
