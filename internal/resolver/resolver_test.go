package resolver

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnstest"

	"github.com/pawal/torrent-tracker/internal/store"
)

func TestRRType(t *testing.T) {
	if TypeA.Family() != 4 || TypeAAAA.Family() != 6 {
		t.Error("families are wrong")
	}
	if TypeA.String() != "A" || TypeAAAA.String() != "AAAA" {
		t.Error("names are wrong")
	}
	if TypeA.dnsType() != dns.TypeA || TypeAAAA.dnsType() != dns.TypeAAAA {
		t.Error("qtypes are wrong")
	}
}

func TestNewNormalisesServers(t *testing.T) {
	d, err := New([]string{"127.0.0.1", "9.9.9.9:5353", "  8.8.8.8  ", ""}, time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"127.0.0.1:53", "9.9.9.9:5353", "8.8.8.8:53"}
	if len(d.Servers) != len(want) {
		t.Fatalf("servers = %v, want %v", d.Servers, want)
	}
	for i := range want {
		if d.Servers[i] != want[i] {
			t.Errorf("servers[%d] = %q, want %q", i, d.Servers[i], want[i])
		}
	}
}

func TestNewDefaultTimeout(t *testing.T) {
	d, err := New([]string{"127.0.0.1"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if d.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want the 5s default", d.Timeout)
	}
}

func TestNewRejectsEmptyServerList(t *testing.T) {
	if _, err := New([]string{"", "  "}, time.Second, 0); err == nil {
		t.Error("want an error when every server entry is blank")
	}
}

func TestNewFallsBackToSystemResolvers(t *testing.T) {
	if _, err := os.Stat("/etc/resolv.conf"); err != nil {
		t.Skip("no /etc/resolv.conf on this host")
	}
	d, err := New(nil, time.Second, 0)
	if err != nil {
		t.Fatalf("New(nil): %v", err)
	}
	if len(d.Servers) == 0 {
		t.Error("no servers picked up from /etc/resolv.conf")
	}
	for _, s := range d.Servers {
		if _, _, err := net.SplitHostPort(s); err != nil {
			t.Errorf("server %q has no port", s)
		}
	}
}

// --- fromReply: rcode and answer mapping ---------------------------------

func mustRR(t *testing.T, s string) dns.RR {
	t.Helper()
	rr, err := dns.New(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return rr
}

func reply(t *testing.T, rcode uint16, rrs ...string) *dns.Msg {
	t.Helper()
	m := dns.NewMsg("tracker.example.com.", dns.TypeA)
	m.Response = true
	m.Rcode = rcode
	for _, s := range rrs {
		m.Answer = append(m.Answer, mustRR(t, s))
	}
	return m
}

func TestFromReplyRcodes(t *testing.T) {
	tests := []struct {
		name  string
		rcode uint16
		want  store.Status
	}{
		{"nxdomain", dns.RcodeNameError, store.StatusNXDomain},
		{"servfail", dns.RcodeServerFailure, store.StatusServFail},
		// REFUSED is a resolver problem, not a fact about the name, so it must
		// map to servfail and leave stored addresses alone.
		{"refused", dns.RcodeRefused, store.StatusServFail},
		{"formerr", dns.RcodeFormatError, store.StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromReply(reply(t, tt.rcode), TypeA, time.Millisecond)
			if got.Status != tt.want {
				t.Errorf("status = %q, want %q", got.Status, tt.want)
			}
			if len(got.Addrs) != 0 {
				t.Errorf("addrs = %v, want none", got.Addrs)
			}
			if tt.want != store.StatusNXDomain && got.Err == "" {
				t.Error("failure results should carry the rcode name")
			}
		})
	}
}

func TestFromReplyNoError(t *testing.T) {
	// NOERROR with answers.
	got := fromReply(reply(t, dns.RcodeSuccess,
		"tracker.example.com. 300 IN A 5.6.7.8",
		"tracker.example.com. 120 IN A 1.2.3.4",
	), TypeA, time.Millisecond)

	if got.Status != store.StatusOK {
		t.Fatalf("status = %q, want ok", got.Status)
	}
	// Sorted, so the diff is stable across answer rotation.
	if len(got.Addrs) != 2 || got.Addrs[0] != "1.2.3.4" || got.Addrs[1] != "5.6.7.8" {
		t.Errorf("addrs = %v, want them sorted", got.Addrs)
	}
	// The shortest TTL in the set.
	if got.TTL != 120 {
		t.Errorf("TTL = %d, want 120", got.TTL)
	}
}

func TestFromReplyNoData(t *testing.T) {
	// NOERROR with an empty answer section means the name exists without
	// addresses, which is a real change, not a failure.
	got := fromReply(reply(t, dns.RcodeSuccess), TypeA, time.Millisecond)
	if got.Status != store.StatusNoData {
		t.Errorf("status = %q, want nodata", got.Status)
	}
	if !got.Status.Resolved() {
		t.Error("nodata must count as a resolved answer")
	}
}

func TestFromReplyIgnoresOtherTypes(t *testing.T) {
	// A CNAME chain puts records of other types in the answer section; only
	// the requested type should be harvested.
	m := reply(t, dns.RcodeSuccess,
		"tracker.example.com. 300 IN CNAME real.example.net.",
		"real.example.net. 300 IN A 1.2.3.4",
	)
	got := fromReply(m, TypeA, time.Millisecond)
	if len(got.Addrs) != 1 || got.Addrs[0] != "1.2.3.4" {
		t.Errorf("addrs = %v, want just the A record", got.Addrs)
	}

	// Asking for AAAA must not pick up the A record.
	got = fromReply(m, TypeAAAA, time.Millisecond)
	if len(got.Addrs) != 0 {
		t.Errorf("addrs = %v, want none for an AAAA query", got.Addrs)
	}
	if got.Status != store.StatusNoData {
		t.Errorf("status = %q, want nodata", got.Status)
	}
}

func TestFromReplyAAAA(t *testing.T) {
	m := reply(t, dns.RcodeSuccess, "tracker.example.com. 60 IN AAAA 2001:db8::1")
	got := fromReply(m, TypeAAAA, time.Millisecond)
	if got.Status != store.StatusOK || len(got.Addrs) != 1 || got.Addrs[0] != "2001:db8::1" {
		t.Errorf("got %+v, want the AAAA address", got)
	}
}

func TestClassifyErr(t *testing.T) {
	if got := classifyErr(context.DeadlineExceeded); got != store.StatusTimeout {
		t.Errorf("deadline exceeded -> %q, want timeout", got)
	}
	timeoutErr := &net.OpError{Err: &timeoutError{}}
	if got := classifyErr(timeoutErr); got != store.StatusTimeout {
		t.Errorf("net timeout -> %q, want timeout", got)
	}
	if got := classifyErr(errors.New("connection refused")); got != store.StatusError {
		t.Errorf("generic error -> %q, want error", got)
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestTTLMin(t *testing.T) {
	if got := ttlMin(0, 300); got != 300 {
		t.Errorf("ttlMin(0,300) = %d, want 300 (zero means unset)", got)
	}
	if got := ttlMin(300, 60); got != 60 {
		t.Errorf("ttlMin(300,60) = %d, want 60", got)
	}
	if got := ttlMin(60, 300); got != 60 {
		t.Errorf("ttlMin(60,300) = %d, want 60", got)
	}
}

// --- end-to-end against a local DNS server -------------------------------

// testZone answers a handful of names so the whole client path is exercised.
type testZone struct{ t *testing.T }

func (z testZone) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.ID = r.ID
	m.Response = true
	m.RecursionAvailable = true
	m.Question = r.Question

	name, qtype := "", uint16(0)
	if len(r.Question) > 0 {
		name = r.Question[0].Header().Name
		qtype = dns.RRToType(r.Question[0])
	}

	switch name {
	case "dual.example.com.":
		if qtype == dns.TypeA {
			m.Answer = []dns.RR{mustRR(z.t, "dual.example.com. 300 IN A 192.0.2.10")}
		} else if qtype == dns.TypeAAAA {
			m.Answer = []dns.RR{mustRR(z.t, "dual.example.com. 300 IN AAAA 2001:db8::10")}
		}
	case "v4only.example.com.":
		if qtype == dns.TypeA {
			m.Answer = []dns.RR{mustRR(z.t, "v4only.example.com. 60 IN A 192.0.2.20")}
		}
		// AAAA falls through to NOERROR/NODATA.
	case "gone.example.com.":
		m.Rcode = dns.RcodeNameError
	case "broken.example.com.":
		m.Rcode = dns.RcodeServerFailure
	default:
		m.Rcode = dns.RcodeNameError
	}

	// WriteTo handles UDP session routing and the TCP length prefix.
	if _, err := m.WriteTo(w); err != nil {
		z.t.Errorf("write reply: %v", err)
	}
}

// testServer starts a local resolver serving testZone and returns its address.
func testServer(t *testing.T) string {
	t.Helper()
	cancel, addr, err := dnstest.UDPServer(":0", func(s *dns.Server) {
		s.Handler = testZone{t: t}
	})
	if err != nil {
		t.Fatalf("start test DNS server: %v", err)
	}
	t.Cleanup(cancel)
	return addr
}

func newTestResolver(t *testing.T) *DNS {
	t.Helper()
	d, err := New([]string{testServer(t)}, 3*time.Second, 0)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestLookupAgainstLiveServer(t *testing.T) {
	d := newTestResolver(t)
	ctx := context.Background()

	tests := []struct {
		name       string
		host       string
		rr         RRType
		wantStatus store.Status
		wantAddr   string
	}{
		{"dual stack A", "dual.example.com", TypeA, store.StatusOK, "192.0.2.10"},
		{"dual stack AAAA", "dual.example.com", TypeAAAA, store.StatusOK, "2001:db8::10"},
		{"v4 only A", "v4only.example.com", TypeA, store.StatusOK, "192.0.2.20"},
		{"v4 only AAAA is nodata", "v4only.example.com", TypeAAAA, store.StatusNoData, ""},
		{"nxdomain", "gone.example.com", TypeA, store.StatusNXDomain, ""},
		{"servfail", "broken.example.com", TypeA, store.StatusServFail, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := d.Lookup(ctx, tt.host, tt.rr)
			if got.Status != tt.wantStatus {
				t.Fatalf("status = %q (err %q), want %q", got.Status, got.Err, tt.wantStatus)
			}
			if tt.wantAddr == "" {
				if len(got.Addrs) != 0 {
					t.Errorf("addrs = %v, want none", got.Addrs)
				}
				return
			}
			if len(got.Addrs) != 1 || got.Addrs[0] != tt.wantAddr {
				t.Errorf("addrs = %v, want [%s]", got.Addrs, tt.wantAddr)
			}
		})
	}
}

// A name given without a trailing dot must still be queried as an FQDN.
func TestLookupQualifiesNames(t *testing.T) {
	d := newTestResolver(t)
	got := d.Lookup(context.Background(), "dual.example.com.", TypeA)
	if got.Status != store.StatusOK {
		t.Errorf("an already-qualified name failed: %q", got.Status)
	}
}

func TestLookupUnreachableServer(t *testing.T) {
	// Port 1 on the loopback should refuse or drop; either way it is a failure
	// that must not be mistaken for an authoritative answer.
	d, err := New([]string{"127.0.0.1:1"}, 250*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := d.Lookup(context.Background(), "example.com", TypeA)
	if got.Status.Resolved() {
		t.Errorf("status = %q, want an unresolved failure status", got.Status)
	}
	if got.Err == "" {
		t.Error("want an error message describing the failure")
	}
}

// A dead server must not stop a healthy one further down the list.
func TestLookupFallsThroughToWorkingServer(t *testing.T) {
	d, err := New([]string{"127.0.0.1:1", testServer(t)}, 250*time.Millisecond, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := d.Lookup(context.Background(), "dual.example.com", TypeA)
	if got.Status != store.StatusOK {
		t.Errorf("status = %q (err %q), want ok from the second server", got.Status, got.Err)
	}
}

func TestLookupHonoursCancelledContext(t *testing.T) {
	d := newTestResolver(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := d.Lookup(ctx, "dual.example.com", TypeA)
	if got.Status.Resolved() {
		t.Errorf("status = %q, want a failure for a cancelled context", got.Status)
	}
}
