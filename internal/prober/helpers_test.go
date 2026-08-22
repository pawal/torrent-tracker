package prober

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"
)

// probeTimeout is generous: every probe here is answered by a loopback server,
// so the only thing the budget has to survive is a busy machine.
const probeTimeout = 5 * time.Second

// probe checks one local endpoint on the /announce path, which is what nearly
// every test here is after.
func probe(t *testing.T, scheme, ip string, port int) Result {
	t.Helper()
	return (&Prober{Timeout: probeTimeout}).Probe(t.Context(), Target{
		Host: "t.example.com", IP: ip, Scheme: scheme, Port: port, Path: "/announce",
	})
}

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
