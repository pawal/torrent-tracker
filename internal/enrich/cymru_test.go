package enrich

import (
	"context"
	"net/netip"
	"testing"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// fakeResolver serves scripted TXT answers keyed by query name.
type fakeResolver struct {
	answers map[string]resolver.Result
	asked   []string
}

func (f *fakeResolver) Lookup(_ context.Context, name string, _ resolver.RRType) resolver.Result {
	f.asked = append(f.asked, name)
	if r, ok := f.answers[name]; ok {
		return r
	}
	return resolver.Result{Status: store.StatusNXDomain}
}

func txt(records ...string) resolver.Result {
	return resolver.Result{Status: store.StatusOK, TXT: records}
}

func TestOriginQName(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		zone string
		want string
	}{
		{
			"ipv4 reversed",
			"1.2.3.4", "origin.asn.cymru.com",
			"4.3.2.1.origin.asn.cymru.com",
		},
		{
			"ipv4 with zeroes",
			"93.158.213.92", "origin.asn.cymru.com",
			"92.213.158.93.origin.asn.cymru.com",
		},
		{
			// Reversed nibbles, exactly like ip6.arpa.
			"ipv6 nibbles",
			"2001:db8::1", "origin6.asn.cymru.com",
			"1.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.0.8.b.d.0.1.0.0.2.origin6.asn.cymru.com",
		},
		{
			// A v4-mapped v6 address must be treated as v4.
			"v4-in-v6 unmaps",
			"::ffff:1.2.3.4", "origin.asn.cymru.com",
			"4.3.2.1.origin.asn.cymru.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := OriginQName(netip.MustParseAddr(tt.ip), tt.zone)
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Errorf("OriginQName(%s) =\n %q\nwant\n %q", tt.ip, got, tt.want)
			}
		})
	}

	if _, err := OriginQName(netip.Addr{}, "z"); err == nil {
		t.Error("want an error for an invalid address")
	}
}

func TestParseOrigin(t *testing.T) {
	got, err := parseOrigin([]string{"13335 | 104.19.22.0/24 | US | arin | 2011-11-01"})
	if err != nil {
		t.Fatal(err)
	}
	want := Info{
		ASN: 13335, Prefix: "104.19.22.0/24", Country: "US",
		RIR: "arin", Allocated: "2011-11-01", Sources: []string{"cymru"},
	}
	if got.ASN != want.ASN || got.Prefix != want.Prefix || got.Country != want.Country ||
		got.RIR != want.RIR || got.Allocated != want.Allocated {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

// When several prefixes cover an address the most specific is the routed one.
func TestParseOriginPrefersMostSpecific(t *testing.T) {
	got, err := parseOrigin([]string{
		"100 | 10.0.0.0/8 | US | arin | 2000-01-01",
		"200 | 10.1.2.0/24 | US | arin | 2000-01-01",
		"300 | 10.1.0.0/16 | US | arin | 2000-01-01",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 200 {
		t.Errorf("ASN = %d, want 200 (the /24)", got.ASN)
	}
}

// Cymru lists multi-origin prefixes as "1234 5678 | ...".
func TestParseOriginMultiOrigin(t *testing.T) {
	got, err := parseOrigin([]string{"64500 64501 | 192.0.2.0/24 | NL | ripencc | 2010-01-01"})
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 64500 {
		t.Errorf("ASN = %d, want the first origin 64500", got.ASN)
	}
}

func TestParseOriginRejectsJunk(t *testing.T) {
	for _, in := range [][]string{
		nil,
		{""},
		{"not a record"},
		{"13335 | 104.19.22.0/24"}, // too few fields
	} {
		if _, err := parseOrigin(in); err == nil {
			t.Errorf("parseOrigin(%q) succeeded, want an error", in)
		}
	}
}

func TestParseASName(t *testing.T) {
	got := parseASName([]string{"13335 | US | arin | 2010-07-14 | CLOUDFLARENET - Cloudflare, Inc., US"})
	if got != "CLOUDFLARENET - Cloudflare, Inc., US" {
		t.Errorf("got %q", got)
	}
	if got := parseASName([]string{"13335 | US | arin | 2010-07-14"}); got != "" {
		t.Errorf("got %q, want empty when the name field is absent", got)
	}
}

func TestCymruLookup(t *testing.T) {
	f := &fakeResolver{answers: map[string]resolver.Result{
		"4.3.2.1.origin.asn.cymru.com": txt("13335 | 1.2.3.0/24 | US | arin | 2011-11-01"),
		"AS13335.asn.cymru.com":        txt("13335 | US | arin | 2010-07-14 | CLOUDFLARENET - Cloudflare, Inc., US"),
	}}
	c := &Cymru{Resolver: f}

	got, err := c.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 13335 || got.RIR != "arin" || got.Country != "US" {
		t.Errorf("got %+v", got)
	}
	if got.ASName != "CLOUDFLARENET - Cloudflare, Inc., US" {
		t.Errorf("ASName = %q", got.ASName)
	}
	if got.Prefix != "1.2.3.0/24" {
		t.Errorf("Prefix = %q", got.Prefix)
	}
}

func TestCymruLookupIPv6UsesOrigin6(t *testing.T) {
	f := &fakeResolver{answers: map[string]resolver.Result{}}
	c := &Cymru{Resolver: f}

	c.Lookup(context.Background(), netip.MustParseAddr("2001:db8::1"))

	if len(f.asked) == 0 {
		t.Fatal("no query was made")
	}
	if want := "origin6.asn.cymru.com"; !hasSuffix(f.asked[0], want) {
		t.Errorf("queried %q, want a name under %q", f.asked[0], want)
	}
}

// Unannounced space is NXDOMAIN, which is an answer rather than a failure.
func TestCymruLookupUnannounced(t *testing.T) {
	c := &Cymru{Resolver: &fakeResolver{answers: map[string]resolver.Result{}}}

	got, err := c.Lookup(context.Background(), netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		t.Errorf("NXDOMAIN should not be an error, got %v", err)
	}
	if !got.Empty() {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestCymruLookupResolverFailure(t *testing.T) {
	f := &fakeResolver{answers: map[string]resolver.Result{
		"4.3.2.1.origin.asn.cymru.com": {Status: store.StatusServFail, Err: "SERVFAIL"},
	}}
	c := &Cymru{Resolver: f}

	if _, err := c.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4")); err == nil {
		t.Error("a resolver failure should be reported as an error")
	}
}

// A missing AS-name record must not discard the origin data we already have.
func TestCymruLookupSurvivesMissingASName(t *testing.T) {
	f := &fakeResolver{answers: map[string]resolver.Result{
		"4.3.2.1.origin.asn.cymru.com": txt("13335 | 1.2.3.0/24 | US | arin | 2011-11-01"),
	}}
	c := &Cymru{Resolver: f}

	got, err := c.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ASN != 13335 {
		t.Errorf("ASN = %d, want 13335", got.ASN)
	}
	if got.ASName != "" {
		t.Errorf("ASName = %q, want empty", got.ASName)
	}
}

func TestPrefixBits(t *testing.T) {
	if got := prefixBits("10.0.0.0/8"); got != 8 {
		t.Errorf("got %d", got)
	}
	if got := prefixBits("nonsense"); got != -1 {
		t.Errorf("got %d, want -1", got)
	}
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
