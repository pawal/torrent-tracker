package prober

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The bodies below are verbatim replies from public trackers. They are the
// whole point of the fingerprint: each string is a literal in some tracker's
// source, so it partitions the registry by software at no extra cost.
func TestSignature(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"opentracker", "d14:failure reason31:no info_hash parameter suppliede",
			"no info_hash parameter supplied"},
		{"another implementation's wording", "d14:failure reason28:scrape requires query stringe",
			"scrape requires query string"},
		{"a third", "d14:failure reason17:missing info_hashe", "missing info_hash"},

		// No error to go on, so the reply's shape stands in.
		{"empty scrape", "d5:filesdee", "files"},
		{"scrape with flags", "d5:filesde5:flagsd20:min_request_intervali36956eee",
			"files,flags,flags.min_request_interval"},
		{"announce shape", "d8:intervali1800e5:peersdee", "interval,peers"},
		{"announce with counts", "d8:completei0e10:incompletei0e8:intervali1800e5:peers0:e",
			"complete,incomplete,interval,peers"},

		// Per-torrent data must not reach the fingerprint: the info_hash keys
		// under "files" differ per request and would make every reply unique.
		{"populated scrape", "d5:filesd20:\xf3\x9a\x01\xbe\x44\x7c\x28\xd0\x91\xff\x02\x5e\xa7\x13\xcc\x60\x88\x1d\xb4\x35d8:completei3eeee", "files"},

		{"not bencoded", "<html>404</html>", ""},
		{"empty body", "", ""},
		{"a list, not a dict", "li1ee", ""},

		// A value we never saw the start of proves nothing, so its key is not
		// allowed to stand in for a signature.
		{"truncated", "d14:failure reason31:no info", ""},
		{"truncated before any key", "d5:fil", ""},
		{"garbage after a good key", "d8:intervali18Xe", ""},

		// But a key whose value merely ran out still shows what the value was,
		// which is how a reply the read limit cut short keeps its shape.
		{"scrape cut off mid-table", "d5:filesd20:\xf3\x9a\x01\xbe\x44\x7c\x28\xd0\x91\xff\x02\x5e\xa7\x13\xcc\x60\x88\x1d\xb4\x35d8:complet", "files"},
		{"announce cut off mid-peers", "d8:completei1e8:intervali1800e5:peers12:\xac\x12\x00", "complete,interval"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signature([]byte(tt.body)); got != tt.want {
				t.Errorf("signature(%q) = %q, want %q", tt.body, got, tt.want)
			}
		})
	}
}

// The prober reads a bounded prefix of every reply, so a tracker with a long
// enough answer used to come back live and unidentified: the shape was there in
// the first hundred bytes, but nothing would decode. Real trackers answer
// scrape with 50MB tables, so this was most of the unidentified ones.
func TestSignatureSurvivesTheReadLimit(t *testing.T) {
	var body strings.Builder
	body.WriteString("d8:completei1e10:downloadedi0e10:incompletei9e8:intervali1800e5:peers")
	// A peer list far past what the prober will read, so the reply is cut off
	// inside it exactly as a real one is.
	peers := 200 << 10
	fmt.Fprintf(&body, "%d:%s", peers, strings.Repeat("\xac\x12\x00\x01\x1a\xe1", peers/6))

	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/scrape" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(body.String()))
	})

	p := &Prober{Timeout: 5 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q (%s), want live", got.State, got.Reason)
	}
	// "peers" is missing because that is the key the truncation landed in; the
	// keys ahead of it are the shape worth having.
	if got.Signature != "complete,downloaded,incomplete,interval" {
		t.Errorf("signature = %q, want the keys that arrived before the cut", got.Signature)
	}
}

// A populated scrape and an empty one are the same software, so they must land
// in the same bucket rather than splitting by how many torrents were listed.
func TestSignatureIgnoresContent(t *testing.T) {
	empty := signature([]byte("d5:filesdee"))
	full := signature([]byte("d5:filesd20:\xf3\x9a\x01\xbe\x44\x7c\x28\xd0\x91\xff\x02\x5e\xa7\x13\xcc\x60\x88\x1d\xb4\x35d8:completei9e10:incompletei4eeee"))
	if empty != full {
		t.Errorf("empty scrape = %q, populated = %q; want the same signature", empty, full)
	}
}

func TestSignatureIsBounded(t *testing.T) {
	long := make([]byte, 4000)
	for i := range long {
		long[i] = 'x'
	}
	body := append([]byte("d14:failure reason4000:"), long...)
	body = append(body, 'e')

	if got := signature(body); len(got) > maxSignature {
		t.Errorf("signature is %d bytes, want at most %d", len(got), maxSignature)
	}
}

// Control characters in a reply must not end up in the column, the change feed
// or the page.
func TestSignatureStripsControlCharacters(t *testing.T) {
	if got := signature([]byte("d14:failure reason10:bad\x00\x1b[31mxe")); got != "bad[31mx" {
		t.Errorf("signature = %q, want the control bytes gone", got)
	}
}

// The probe already fetches this body to decide live or dead, so recording the
// software costs nothing extra.
func TestProbeCapturesSignatureAndServer(t *testing.T) {
	_, ip, port := httpTracker(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "nginx/1.28.0")
		w.Write([]byte("d14:failure reason31:no info_hash parameter suppliede"))
	})

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{
		Host: "t.example.com", IP: ip, Scheme: "http", Port: port, Path: "/announce",
	})
	if got.State != Live {
		t.Fatalf("state = %q, want live", got.State)
	}
	if got.Signature != "no info_hash parameter supplied" {
		t.Errorf("signature = %q", got.Signature)
	}
	if got.Server != "nginx/1.28.0" {
		t.Errorf("server = %q, want nginx/1.28.0", got.Server)
	}
}

// UDP has nowhere to carry either, so it must report neither rather than
// inventing something.
func TestProbeUDPHasNoSignature(t *testing.T) {
	ip, port := udpTracker(t, connectReply)

	p := &Prober{Timeout: 2 * time.Second}
	got := p.Probe(context.Background(), Target{Host: "t.example.com", IP: ip, Scheme: "udp", Port: port})
	if got.State != Live {
		t.Fatalf("state = %q, want live", got.State)
	}
	if got.Signature != "" || got.Server != "" {
		t.Errorf("signature = %q, server = %q; want both empty", got.Signature, got.Server)
	}
}
