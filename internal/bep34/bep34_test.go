package bep34

import (
	"fmt"
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name  string
		txts  []string
		raw   string
		prefs []Pref
		deny  bool
	}{
		{name: "no TXT records at all"},
		{
			name: "somebody else's TXT records",
			txts: []string{"v=spf1 -all", "google-site-verification=abc"},
		},
		{
			name:  "one udp tracker",
			txts:  []string{"BITTORRENT UDP:6969"},
			raw:   "BITTORRENT UDP:6969",
			prefs: []Pref{{"udp", 6969}},
		},
		{
			name:  "several, in the order given",
			txts:  []string{"BITTORRENT UDP:6969 TCP:80 TCP:443"},
			raw:   "BITTORRENT UDP:6969 TCP:80 TCP:443",
			prefs: []Pref{{"udp", 6969}, {"tcp", 80}, {"tcp", 443}},
		},
		// The whole opt-out: a record that names nowhere to connect says the
		// host runs no trackers.
		{name: "bare keyword denies", txts: []string{"BITTORRENT"}, raw: "BITTORRENT", deny: true},
		{
			// The spec's own example of a readable denial. DENY and ALL are
			// unrecognised words, so it means exactly the bare keyword does.
			name: "deny all is the same thing",
			txts: []string{"BITTORRENT DENY ALL"},
			raw:  "BITTORRENT DENY ALL",
			deny: true,
		},
		{
			name:  "unrecognised words are ignored",
			txts:  []string{"BITTORRENT SCTP:99 UDP:6969 nonsense"},
			raw:   "BITTORRENT SCTP:99 UDP:6969 nonsense",
			prefs: []Pref{{"udp", 6969}},
		},
		// The contents are case-sensitive, so these are not our record and not
		// our tokens.
		{name: "lowercase keyword is not ours", txts: []string{"bittorrent UDP:6969"}},
		{
			name: "lowercase tokens do not count",
			txts: []string{"BITTORRENT udp:6969"},
			raw:  "BITTORRENT udp:6969",
			deny: true,
		},
		{
			name: "the keyword has to come first",
			txts: []string{"prefix BITTORRENT UDP:6969"},
		},
		{
			name:  "ports out of range are not ports",
			txts:  []string{"BITTORRENT UDP:0 UDP:65536 UDP:-1 UDP:x UDP: UDP:65535"},
			raw:   "BITTORRENT UDP:0 UDP:65536 UDP:-1 UDP:x UDP: UDP:65535",
			prefs: []Pref{{"udp", 65535}},
		},
		{
			name:  "a repeated endpoint is one endpoint",
			txts:  []string{"BITTORRENT UDP:6969 UDP:6969 TCP:80"},
			raw:   "BITTORRENT UDP:6969 UDP:6969 TCP:80",
			prefs: []Pref{{"udp", 6969}, {"tcp", 80}},
		},
		{
			// Runs of whitespace and stray padding must not read as a changed
			// record on the next pass.
			name:  "whitespace is normalised",
			txts:  []string{"  BITTORRENT   UDP:6969  "},
			raw:   "BITTORRENT UDP:6969",
			prefs: []Pref{{"udp", 6969}},
		},
		{
			// Two records is malformed and the answer order is arbitrary, so
			// the choice has to be deterministic or the feed fills with noise.
			name:  "several records pick the lowest",
			txts:  []string{"BITTORRENT UDP:7000", "BITTORRENT UDP:6969"},
			raw:   "BITTORRENT UDP:6969",
			prefs: []Pref{{"udp", 6969}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Parse(tt.txts)
			if got.Raw != tt.raw {
				t.Errorf("Raw = %q, want %q", got.Raw, tt.raw)
			}
			if !reflect.DeepEqual(got.Prefs, tt.prefs) {
				t.Errorf("Prefs = %v, want %v", got.Prefs, tt.prefs)
			}
			if got.Denies() != tt.deny {
				t.Errorf("Denies() = %v, want %v", got.Denies(), tt.deny)
			}
			if got.Found() != (tt.raw != "") {
				t.Errorf("Found() = %v for raw %q", got.Found(), got.Raw)
			}
		})
	}
}

func TestParseBoundsTheRecord(t *testing.T) {
	// A record can be as long as DNS allows. Reading an unbounded number of
	// endpoints out of one would let a name aim us at the whole port range of
	// its own host.
	long := "BITTORRENT"
	for port := 1000; port < 1100; port++ {
		long += fmt.Sprintf(" UDP:%d", port)
	}
	if got := len(Parse([]string{long}).Prefs); got != maxPrefs {
		t.Errorf("read %d endpoints from a 100-endpoint record, want %d", got, maxPrefs)
	}
}
