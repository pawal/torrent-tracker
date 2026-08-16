package enrich

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// stubProvider returns a canned answer.
type stubProvider struct {
	name  string
	info  Info
	err   error
	calls int
	delay time.Duration
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	s.calls++
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return Info{}, ctx.Err()
		}
	}
	return s.info, s.err
}

var testIP = netip.MustParseAddr("1.2.3.4")

func TestChainMerges(t *testing.T) {
	cheap := &stubProvider{name: "cymru", info: Info{ASN: 13335, RIR: "arin", Country: "US"}}
	rich := &stubProvider{name: "rdap", info: Info{NetworkName: "CLOUDFLARENET", Org: "Cloudflare, Inc.", Country: "GB"}}
	geo := &stubProvider{name: "maxmind", info: Info{City: "London", Latitude: 51.5, Longitude: -0.1}}

	got, err := (&Chain{Providers: []Provider{cheap, rich, geo}}).Lookup(context.Background(), testIP)
	if err != nil {
		t.Fatal(err)
	}

	if got.ASN != 13335 || got.RIR != "arin" {
		t.Errorf("lost the cheap provider's data: %+v", got)
	}
	if got.Org != "Cloudflare, Inc." || got.NetworkName != "CLOUDFLARENET" {
		t.Errorf("lost the registry data: %+v", got)
	}
	if got.City != "London" || got.Latitude != 51.5 {
		t.Errorf("lost the geo data: %+v", got)
	}
	// First answer wins: the earlier provider's country is not overwritten.
	if got.Country != "US" {
		t.Errorf("Country = %q, want the first provider's US", got.Country)
	}
	if want := "cymru,rdap,maxmind"; strings.Join(got.Sources, ",") != want {
		t.Errorf("Sources = %v, want %s", got.Sources, want)
	}
}

// A failing provider must not sink the whole lookup.
func TestChainToleratesPartialFailure(t *testing.T) {
	ok := &stubProvider{name: "cymru", info: Info{ASN: 64500, RIR: "ripencc"}}
	bad := &stubProvider{name: "rdap", err: errors.New("registry down")}

	got, err := (&Chain{Providers: []Provider{ok, bad}}).Lookup(context.Background(), testIP)
	if err != nil {
		t.Fatalf("partial failure should not error: %v", err)
	}
	if got.ASN != 64500 {
		t.Errorf("got %+v, want the working provider's data", got)
	}
}

// If nothing worked, say so.
func TestChainReportsTotalFailure(t *testing.T) {
	a := &stubProvider{name: "a", err: errors.New("boom")}
	b := &stubProvider{name: "b", err: errors.New("bang")}

	_, err := (&Chain{Providers: []Provider{a, b}}).Lookup(context.Background(), testIP)
	if err == nil {
		t.Fatal("want an error when every provider failed")
	}
	for _, want := range []string{"boom", "bang"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
}

// An address nothing knows about is not an error.
func TestChainEmptyIsNotAnError(t *testing.T) {
	got, err := (&Chain{Providers: []Provider{&stubProvider{name: "a"}}}).Lookup(context.Background(), testIP)
	if err != nil {
		t.Errorf("err = %v, want nil", err)
	}
	if !got.Empty() {
		t.Errorf("got %+v, want empty", got)
	}
}

func TestChainStopsOnCancel(t *testing.T) {
	p := &stubProvider{name: "a", info: Info{ASN: 1}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&Chain{Providers: []Provider{p}}).Lookup(ctx, testIP); err == nil {
		t.Error("want an error for a cancelled context")
	}
	if p.calls != 0 {
		t.Errorf("provider was called %d times despite cancellation", p.calls)
	}
}

func TestChainTagsSourceWhenProviderDoesNot(t *testing.T) {
	p := &stubProvider{name: "anon", info: Info{ASN: 7}}
	got, _ := (&Chain{Providers: []Provider{p}}).Lookup(context.Background(), testIP)
	if len(got.Sources) != 1 || got.Sources[0] != "anon" {
		t.Errorf("Sources = %v, want [anon]", got.Sources)
	}
}

func TestInfoEmpty(t *testing.T) {
	if !(Info{}).Empty() {
		t.Error("zero Info should be empty")
	}
	// A prefix or allocation date alone tells us nothing about the holder.
	if !(Info{Prefix: "1.2.3.0/24", Allocated: "2020-01-01"}).Empty() {
		t.Error("prefix and date alone should count as empty")
	}
	for _, i := range []Info{{ASN: 1}, {RIR: "arin"}, {Country: "US"}, {Org: "x"}, {City: "y"}, {NetworkName: "z"}} {
		if i.Empty() {
			t.Errorf("%+v should not be empty", i)
		}
	}
}

func TestInfoDescribe(t *testing.T) {
	tests := []struct {
		name string
		in   Info
		want string
	}{
		{"full", Info{ASN: 13335, Org: "Cloudflare, Inc.", RIR: "arin", Country: "US"},
			"AS13335 Cloudflare, Inc. (arin, US)"},
		{"as name fallback", Info{ASN: 24940, ASName: "HETZNER-AS", RIR: "ripencc"},
			"AS24940 HETZNER-AS (ripencc)"},
		{"network name fallback", Info{NetworkName: "SOME-NET", Country: "NL"},
			"SOME-NET (NL)"},
		{"asn only", Info{ASN: 64500}, "AS64500"},
		{"nothing", Info{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Describe(); got != tt.want {
				t.Errorf("Describe() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTimeout(t *testing.T) {
	slow := &stubProvider{name: "slow", delay: 500 * time.Millisecond, info: Info{ASN: 1}}
	wrapped := Timeout{Provider: slow, Limit: 20 * time.Millisecond}

	if wrapped.Name() != "slow" {
		t.Errorf("Name() = %q", wrapped.Name())
	}
	start := time.Now()
	if _, err := wrapped.Lookup(context.Background(), testIP); err == nil {
		t.Error("want a deadline error")
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("took %v, want the deadline to cut it short", elapsed)
	}
}

func TestTimeoutZeroMeansNoLimit(t *testing.T) {
	p := &stubProvider{name: "p", info: Info{ASN: 1}}
	got, err := Timeout{Provider: p}.Lookup(context.Background(), testIP)
	if err != nil || got.ASN != 1 {
		t.Errorf("got %+v, %v", got, err)
	}
}

// A nil MaxMind must behave like a provider with nothing to say.
func TestMaxMindNilIsHarmless(t *testing.T) {
	var m *MaxMind
	got, err := m.Lookup(context.Background(), testIP)
	if err != nil || !got.Empty() {
		t.Errorf("got %+v, %v", got, err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close() = %v", err)
	}
}

func TestOpenMaxMindMissingFile(t *testing.T) {
	if _, err := OpenMaxMind("/nonexistent/does-not-exist.mmdb"); err == nil {
		t.Error("want an error for a missing database")
	}
}
