package enrich

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

const ripeRecord = `{
  "handle": "93.158.213.0 - 93.158.213.255",
  "name": "SERVERIUSCUSTOMER",
  "country": "nl",
  "type": "ASSIGNED PA",
  "entities": [
    {"handle": "SN1", "roles": ["technical"],
     "vcardArray": ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Tech Contact"]]]},
    {"handle": "SERVERIUS-MNT", "roles": ["registrant"],
     "vcardArray": ["vcard", [["version", {}, "text", "4.0"], ["fn", {}, "text", "Serverius Holding B.V."]]]}
  ]
}`

// newRDAPServer serves both the bootstrap tables and the registry records.
func newRDAPServer(t *testing.T) (*RDAP, *httptest.Server, *int) {
	t.Helper()
	var hits int

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/bootstrap/ipv4", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"services":[
			[["93.0.0.0/8","94.0.0.0/8"],["` + srv.URL + `/ripe/"]],
			[["1.0.0.0/8"],["` + srv.URL + `/apnic/"]]
		]}`))
	})
	mux.HandleFunc("/bootstrap/ipv6", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"services":[[["2001:db8::/32"],["` + srv.URL + `/ripe/"]]]}`))
	})
	mux.HandleFunc("/ripe/ip/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/rdap+json")
		w.Write([]byte(ripeRecord))
	})
	mux.HandleFunc("/apnic/ip/", func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "not found", http.StatusNotFound)
	})

	return &RDAP{
		BootstrapV4: srv.URL + "/bootstrap/ipv4",
		BootstrapV6: srv.URL + "/bootstrap/ipv6",
		MinInterval: time.Millisecond,
	}, srv, &hits
}

func TestRDAPLookup(t *testing.T) {
	r, _, _ := newRDAPServer(t)

	got, err := r.Lookup(context.Background(), netip.MustParseAddr("93.158.213.92"))
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkName != "SERVERIUSCUSTOMER" {
		t.Errorf("NetworkName = %q", got.NetworkName)
	}
	// Country is normalised to upper case regardless of what the RIR sends.
	if got.Country != "NL" {
		t.Errorf("Country = %q, want NL", got.Country)
	}
	// The registrant outranks the technical contact.
	if got.Org != "Serverius Holding B.V." {
		t.Errorf("Org = %q, want the registrant", got.Org)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "rdap" {
		t.Errorf("Sources = %v", got.Sources)
	}
}

func TestRDAPBootstrapPicksTheRightRegistry(t *testing.T) {
	r, srv, _ := newRDAPServer(t)

	ep, err := r.endpointFor(context.Background(), netip.MustParseAddr("93.158.213.92"))
	if err != nil {
		t.Fatal(err)
	}
	if ep != srv.URL+"/ripe/" {
		t.Errorf("endpoint = %q, want the RIPE one", ep)
	}

	ep, err = r.endpointFor(context.Background(), netip.MustParseAddr("1.2.3.4"))
	if err != nil {
		t.Fatal(err)
	}
	if ep != srv.URL+"/apnic/" {
		t.Errorf("endpoint = %q, want the APNIC one", ep)
	}
}

func TestRDAPBootstrapIsCached(t *testing.T) {
	var fetches int
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	mux.HandleFunc("/bootstrap/ipv4", func(w http.ResponseWriter, r *http.Request) {
		fetches++
		w.Write([]byte(`{"services":[[["1.0.0.0/8"],["` + srv.URL + `/r/"]]]}`))
	})
	mux.HandleFunc("/r/ip/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"NET"}`))
	})

	r := &RDAP{BootstrapV4: srv.URL + "/bootstrap/ipv4", MinInterval: time.Millisecond}
	for i := 0; i < 3; i++ {
		if _, err := r.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4")); err != nil {
			t.Fatal(err)
		}
	}
	if fetches != 1 {
		t.Errorf("bootstrap fetched %d times, want 1", fetches)
	}
}

func TestRDAPUnknownRange(t *testing.T) {
	r, _, _ := newRDAPServer(t)
	// 200.0.0.0/8 is in neither bootstrap entry.
	if _, err := r.endpointFor(context.Background(), netip.MustParseAddr("200.1.2.3")); err == nil {
		t.Error("want an error when no registry serves the address")
	}
}

func TestRDAPHTTPError(t *testing.T) {
	r, _, _ := newRDAPServer(t)
	if _, err := r.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4")); err == nil {
		t.Error("a 404 from the registry should be an error")
	}
}

func TestRDAPIPv6(t *testing.T) {
	r, _, _ := newRDAPServer(t)
	got, err := r.Lookup(context.Background(), netip.MustParseAddr("2001:db8::1"))
	if err != nil {
		t.Fatal(err)
	}
	if got.NetworkName != "SERVERIUSCUSTOMER" {
		t.Errorf("NetworkName = %q", got.NetworkName)
	}
}

func TestRDAPThrottles(t *testing.T) {
	r, _, _ := newRDAPServer(t)
	r.MinInterval = 60 * time.Millisecond

	start := time.Now()
	for i := 0; i < 3; i++ {
		r.Lookup(context.Background(), netip.MustParseAddr("93.158.213.92"))
	}
	// Bootstrap plus three lookups is four requests, so at least three gaps.
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("4 requests took %v, want the throttle to space them out", elapsed)
	}
}

func TestRDAPRespectsCancelledContext(t *testing.T) {
	r, _, _ := newRDAPServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := r.Lookup(ctx, netip.MustParseAddr("93.158.213.92")); err == nil {
		t.Error("want an error for a cancelled context")
	}
}

func TestVCardName(t *testing.T) {
	raw := []byte(`["vcard",[["version",{},"text","4.0"],["fn",{},"text","Example Org"]]]`)
	if got := vcardName(raw); got != "Example Org" {
		t.Errorf("got %q", got)
	}
	for _, bad := range []string{``, `null`, `[]`, `["vcard"]`, `["vcard",[["x",{},"text","y"]]]`} {
		if got := vcardName([]byte(bad)); got != "" {
			t.Errorf("vcardName(%s) = %q, want empty", bad, got)
		}
	}
}

func TestPreferHTTPS(t *testing.T) {
	if got := preferHTTPS([]string{"http://a/", "https://b/"}); got != "https://b/" {
		t.Errorf("got %q", got)
	}
	if got := preferHTTPS([]string{"http://a/"}); got != "http://a/" {
		t.Errorf("got %q, want the only entry", got)
	}
	if got := preferHTTPS(nil); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestRDAPBadBootstrap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"services":[]}`))
	}))
	defer srv.Close()

	r := &RDAP{BootstrapV4: srv.URL, MinInterval: time.Millisecond}
	_, err := r.Lookup(context.Background(), netip.MustParseAddr("1.2.3.4"))
	if err == nil || !strings.Contains(err.Error(), "bootstrap") {
		t.Errorf("err = %v, want a bootstrap error", err)
	}
}
