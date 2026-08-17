package prober

import (
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// udpTracker stands in for a BEP 15 tracker. reply decides what it sends back
// for each connect request, so a test can be a healthy tracker, a broken one,
// or something else entirely that happens to be listening on the port.
func udpTracker(t *testing.T, reply func(req []byte) []byte) (host string, port int) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	go func() {
		buf := make([]byte, 1024)
		for {
			n, addr, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			if out := reply(buf[:n]); out != nil {
				conn.WriteTo(out, addr)
			}
		}
	}()

	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String(), addr.Port
}

// connectReply is the 16-byte answer a healthy tracker gives, echoing the
// transaction id the client chose.
func connectReply(req []byte) []byte {
	out := make([]byte, 16)
	binary.BigEndian.PutUint32(out[0:4], actionConnect)
	copy(out[4:8], req[12:16]) // transaction id
	binary.BigEndian.PutUint64(out[8:16], 0x1122334455667788)
	return out
}

func TestProbeUDPLive(t *testing.T) {
	ip, port := udpTracker(t, connectReply)

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "tracker.example.com", IP: ip, Scheme: "udp", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
}

// A tracker may answer a connect request with an error action. It still spoke
// the protocol, so it is alive.
func TestProbeUDPErrorActionIsLive(t *testing.T) {
	ip, port := udpTracker(t, func(req []byte) []byte {
		out := make([]byte, 16)
		binary.BigEndian.PutUint32(out[0:4], actionError)
		copy(out[4:8], req[12:16])
		return out
	})

	p := &Prober{Timeout: 2 * time.Second}
	if got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "udp", Port: port,
	}); got.State != Live {
		t.Errorf("state = %q, want live", got.State)
	}
}

// Something else listening on the port answers with a datagram that is not a
// tracker reply. Echoing our transaction id is the only proof that counts.
func TestProbeUDPWrongTransactionID(t *testing.T) {
	ip, port := udpTracker(t, func([]byte) []byte {
		out := make([]byte, 16)
		binary.BigEndian.PutUint32(out[0:4], actionConnect)
		binary.BigEndian.PutUint32(out[4:8], 0xdeadbeef)
		return out
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: ip, Scheme: "udp", Port: port})
	if got.State != Dead {
		t.Errorf("state = %q, want dead", got.State)
	}
	if !strings.Contains(got.Reason, "transaction id") {
		t.Errorf("reason = %q, want it to mention the transaction id", got.Reason)
	}
}

func TestProbeUDPSilence(t *testing.T) {
	ip, port := udpTracker(t, func([]byte) []byte { return nil })

	p := &Prober{Timeout: 300 * time.Millisecond}
	got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: ip, Scheme: "udp", Port: port})
	if got.State != Dead {
		t.Errorf("state = %q, want dead", got.State)
	}
	if got.Reason != "timed out" {
		t.Errorf("reason = %q, want %q", got.Reason, "timed out")
	}
}

// UDP has nothing to lean on, so a dropped datagram arrives here as silence,
// exactly like a tracker that no longer exists. Retransmitting is what keeps
// ordinary packet loss from spending one of an endpoint's two lives.
func TestProbeUDPRetransmitsAfterSilence(t *testing.T) {
	// The tracker replies from its own goroutine, so the count it keeps has to
	// be safe to read back here.
	var sent atomic.Int32
	ip, port := udpTracker(t, func(req []byte) []byte {
		if sent.Add(1) == 1 {
			return nil
		}
		return connectReply(req)
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: ip, Scheme: "udp", Port: port})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	if n := sent.Load(); n != 2 {
		t.Errorf("tracker saw %d requests, want 2", n)
	}
}

// Both attempts share the one budget, so a genuinely dead endpoint costs no
// more wall clock than it did before the retransmission existed.
func TestProbeUDPRetransmissionStaysWithinTheTimeout(t *testing.T) {
	ip, port := udpTracker(t, func([]byte) []byte { return nil })

	p := &Prober{Timeout: 400 * time.Millisecond}
	start := time.Now()
	got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: ip, Scheme: "udp", Port: port})
	elapsed := time.Since(start)

	if got.State != Dead {
		t.Errorf("state = %q, want dead", got.State)
	}
	// Generous headroom: this asserts the budget is shared rather than doubled,
	// not that the scheduler is prompt.
	if elapsed > 700*time.Millisecond {
		t.Errorf("probe took %s, want it inside the 400ms timeout", elapsed.Round(time.Millisecond))
	}
}

// An answer that proves the port is not a tracker is still an answer. Asking
// again would only double the traffic for a verdict that will not change.
func TestProbeUDPAnsweredIsNotRetransmitted(t *testing.T) {
	var sent atomic.Int32
	ip, port := udpTracker(t, func([]byte) []byte {
		sent.Add(1)
		out := make([]byte, 16)
		binary.BigEndian.PutUint32(out[0:4], actionConnect)
		binary.BigEndian.PutUint32(out[4:8], 0xdeadbeef)
		return out
	})

	p := &Prober{Timeout: 2 * time.Second}
	if got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "udp", Port: port,
	}); got.State != Dead {
		t.Errorf("state = %q, want dead", got.State)
	}
	if n := sent.Load(); n != 1 {
		t.Errorf("tracker saw %d requests, want 1", n)
	}
}

// httpTracker runs a local HTTP server and returns its address split into the
// pieces a Target needs.
func httpTracker(t *testing.T, h http.HandlerFunc) (srv *httptest.Server, ip string, port int) {
	t.Helper()
	srv = httptest.NewServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err = strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}
	return srv, u.Hostname(), port
}

// question names which of the prober's three requests this is. Two of them share
// the /announce path and differ only in what they leave out, so the path alone
// cannot tell a test what was asked.
func question(r *http.Request) string {
	switch {
	case r.URL.Path == "/scrape":
		return "scrape"
	case r.URL.Query().Has("info_hash"):
		return "announce"
	}
	return "incomplete"
}

// Scrape is the polite check: it asks about the tracker without pretending to be
// a peer, so it must be tried first, and a scrape that names the software must
// settle the matter on its own.
func TestProbeHTTPScrape(t *testing.T) {
	var asked []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, question(r))
		w.Write([]byte("d14:failure reason28:scrape requires query stringe"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "tracker.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	if len(asked) != 1 || asked[0] != "scrape" {
		t.Errorf("asked %v, want a single scrape", asked)
	}
	if got.Signature != "scrape requires query string" || got.Kind != KindFailure {
		t.Errorf("signature = %q (%s), want the failure text", got.Signature, got.Kind)
	}
}

// A bencoded failure is still a tracker talking. Rejecting it would mark every
// tracker that refuses unknown info_hashes as dead.
func TestProbeHTTPFailureReasonIsLive(t *testing.T) {
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("d14:failure reason21:torrent not registerede"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	if got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	}); got.State != Live {
		t.Errorf("state = %q, want live", got.State)
	}
}

// Plenty of trackers never implemented scrape, so a 404 there must not settle
// the question on its own.
func TestProbeHTTPFallsBackToAnnounce(t *testing.T) {
	var asked []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, question(r))
		if r.URL.Path == "/scrape" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("d8:intervali1800e5:peerslee"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	// The third question follows because a reply shape names nobody; it is the
	// second that decided the tracker is alive.
	if len(asked) < 2 || asked[0] != "scrape" || asked[1] != "announce" {
		t.Errorf("asked %v, want scrape then announce", asked)
	}
}

// Some trackers answer scrape with their entire table, tens of megabytes of it,
// ignoring the info_hash we asked about. Every one of those replies opens with
// the same "files" key, so the scrape proves the tracker lives and says nothing
// about which tracker it is. Announce is where the fingerprint is.
func TestProbeHTTPUninformativeScrapeTriesAnnounce(t *testing.T) {
	var paths []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/scrape" {
			w.Write([]byte("d5:filesdee"))
			return
		}
		w.Write([]byte("d14:failure reason17:missing info_hashe"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	if len(paths) != 2 || paths[1] != "/announce" {
		t.Fatalf("requested %v, want /scrape then /announce", paths)
	}
	if got.Signature != "missing info_hash" {
		t.Errorf("signature = %q, want the one announce disclosed", got.Signature)
	}
}

// Chasing a better signature must never cost a verdict. A tracker whose scrape
// works and whose announce does not is still a live tracker.
func TestProbeHTTPAnnounceCannotUnseatALiveScrape(t *testing.T) {
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/scrape" {
			w.Write([]byte("d5:filesdee"))
			return
		}
		http.Error(w, "go away", http.StatusForbidden)
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Errorf("state = %q (%s), want live", got.State, got.Reason)
	}
	if got.Signature != "files" {
		t.Errorf("signature = %q, want the scrape's own", got.Signature)
	}
}

// A tracker that can answer the question asked has no reason to say who it is:
// the reply is the same handful of BEP 3 keys whoever wrote it. Refusing a
// request is where implementations put their own words, so a live tracker with
// nothing but a shape is asked for an announce with no info_hash. In the live
// registry this was the difference between two dozen anonymous trackers and two
// dozen named ones.
func TestProbeHTTPProvokesAFailureText(t *testing.T) {
	var asked []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, question(r))
		if question(r) == "incomplete" {
			w.Write([]byte("d14:failure reason37:missing required parameter: info_hashe"))
			return
		}
		w.Write([]byte("d8:completei1e10:incompletei0e8:intervali1800e5:peers0:e"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	if want := []string{"scrape", "incomplete"}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if got.Signature != "missing required parameter: info_hash" || got.Kind != KindFailure {
		t.Errorf("signature = %q (%s), want the literal the refusal disclosed",
			got.Signature, got.Kind)
	}
}

// A tracker that already named itself is not asked again. The extra request buys
// nothing there, and every request is one a public tracker did not ask for.
func TestProbeHTTPDoesNotProvokeWhenAlreadyNamed(t *testing.T) {
	var asked []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, question(r))
		if r.URL.Path == "/scrape" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("d14:failure reason31:no info_hash parameter suppliede"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if want := []string{"scrape", "announce"}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
	if got.Signature != "no info_hash parameter supplied" {
		t.Errorf("signature = %q", got.Signature)
	}
}

// The extra question is for the fingerprint alone. Whatever it draws, the verdict
// and the RTT belong to the request that actually spoke the tracker protocol.
func TestProbeHTTPProvokingCannotChangeTheVerdict(t *testing.T) {
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		if question(r) == "incomplete" {
			http.Error(w, "<html>go away</html>", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("d8:intervali1800e5:peers0:e"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Errorf("state = %q (%s), want live", got.State, got.Reason)
	}
	if got.Signature != "interval,peers" || got.Kind != KindShape {
		t.Errorf("signature = %q (%s), want the shape the scrape disclosed",
			got.Signature, got.Kind)
	}
}

// Nothing is pressed for a fingerprint it cannot have. A dead endpoint gets the
// two questions that decide the verdict and not one more.
func TestProbeHTTPDeadEndpointIsNotProvoked(t *testing.T) {
	var asked []string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, question(r))
		http.NotFound(w, r)
	})

	p := &Prober{Timeout: 2 * time.Second}
	if got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	}); got.State != Dead {
		t.Errorf("state = %q, want dead", got.State)
	}
	if want := []string{"scrape", "announce"}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v", asked, want)
	}
}

// Probing every address of a CDN-fronted name draws a burst of requests, and
// the CDN throttles. Being turned away is not evidence the tracker is gone, so
// it must not count as dead.
func TestProbeHTTPThrottlingIsUnknown(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		var hits int
		_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
			hits++
			http.Error(w, "slow down", status)
		})

		p := &Prober{Timeout: 2 * time.Second}
		got := p.Probe(context.Background(), Target{
			Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
		})
		if got.State != Unknown {
			t.Errorf("HTTP %d: state = %q, want unknown", status, got.State)
		}
		// Retrying against announce would only add to the load.
		if hits != 1 {
			t.Errorf("HTTP %d: made %d requests, want 1", status, hits)
		}
	}
}

// The parked-domain case: a web server answers on the port, but it is not a
// tracker. This is the failure the DNS status cannot see.
func TestProbeHTTPParkingPage(t *testing.T) {
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("<html><body>This domain is for sale</body></html>"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Dead {
		t.Fatalf("state = %q, want dead", got.State)
	}
	if !strings.Contains(got.Reason, "HTML") {
		t.Errorf("reason = %q, want it to name the HTML body", got.Reason)
	}
}

// The Host header has to survive dialling by address, or every virtual-hosted
// tracker would answer for the wrong site.
func TestProbeHTTPSendsHostHeader(t *testing.T) {
	var seen string
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Host
		w.Write([]byte("d5:filesdee"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	p.Probe(context.Background(), Target{
		Host: "tracker.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	want := "tracker.example.com:" + strconv.Itoa(port)
	if seen != want {
		t.Errorf("Host header = %q, want %q", seen, want)
	}
}

func TestProbeRejectsUnusableTargets(t *testing.T) {
	p := &Prober{Timeout: time.Second}

	if got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: "", Scheme: "udp", Port: 80}); got.State != Unknown {
		t.Errorf("empty address: state = %q, want unknown", got.State)
	}
	if got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: "127.0.0.1", Scheme: "wss", Port: 443,
	}); got.State != Unknown {
		t.Errorf("websocket scheme: state = %q, want unknown", got.State)
	}
}

func TestScrapeURL(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"plain announce", "/announce", "http://t.example.com:80/scrape"},
		{"php announce", "/announce.php", "http://t.example.com:80/scrape.php"},
		{"nested", "/tracker/announce", "http://t.example.com:80/tracker/scrape"},
		// Nothing to rewrite, so the path is used as it stands.
		{"no announce segment", "/", "http://t.example.com:80/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scrapeURL(Target{Host: "t.example.com", Scheme: "http", Port: 80, Path: tt.path})
			if got != tt.want {
				t.Errorf("scrapeURL(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

func TestBencoded(t *testing.T) {
	live := []string{"d5:filesdee", "  d14:failure reason3:noe", "\r\nd8:completei0ee"}
	for _, body := range live {
		if !bencoded([]byte(body)) {
			t.Errorf("bencoded(%q) = false, want true", body)
		}
	}
	dead := []string{"", "<html>", "not found", "li1ee"}
	for _, body := range dead {
		if bencoded([]byte(body)) {
			t.Errorf("bencoded(%q) = true, want false", body)
		}
	}
}
