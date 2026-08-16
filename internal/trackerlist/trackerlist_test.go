package trackerlist

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHost(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"udp announce", "udp://tracker.example.com:1337/announce", "tracker.example.com"},
		{"http announce", "http://tracker.example.com:6969/announce", "tracker.example.com"},
		{"https announce", "https://tracker.example.com/announce", "tracker.example.com"},
		{"websocket announce", "ws://tracker.example.com:80/announce", "tracker.example.com"},
		{"php announce path", "http://bigfangroup.org/announce.php", "bigfangroup.org"},
		{"no port", "udp://open.demonii.com/announce", "open.demonii.com"},
		{"bare hostname", "tracker.example.com", "tracker.example.com"},
		{"bare host:port", "tracker.example.com:6969", "tracker.example.com"},
		{"uppercase is normalised", "UDP://Tracker.EXAMPLE.com:80/announce", "tracker.example.com"},
		{"trailing dot stripped", "http://tracker.example.com./announce", "tracker.example.com"},
		{"surrounding space", "  udp://tracker.example.com:80/announce  ", "tracker.example.com"},
		{"userinfo ignored", "http://user:pw@tracker.example.com:80/announce", "tracker.example.com"},
		{"hyphens and digits", "udp://1c.premierzal-2.ru:6969/announce", "1c.premierzal-2.ru"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Host(tt.in)
			if err != nil {
				t.Fatalf("Host(%q) returned error: %v", tt.in, err)
			}
			if got != tt.want {
				t.Errorf("Host(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestHostRejects(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		// Literal addresses have no DNS history to track.
		{"ipv4 literal", "http://107.189.2.131:1337/announce"},
		{"ipv6 literal", "udp://[2001:db8::1]:6969/announce"},
		// Overlay networks are not in the DNS.
		{"i2p", "http://tracker.example.i2p/announce"},
		{"onion", "http://abcdefgh.onion:6969/announce"},
		{"yggdrasil", "udp://tracker.example.ygg:6969/announce"},
		// Malformed input.
		{"empty", ""},
		{"whitespace", "   "},
		{"no dot", "http://localhost:8080/announce"},
		{"scheme only", "udp://"},
		{"underscore", "udp://bad_host.example.com:80/announce"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, err := Host(tt.in); err == nil {
				t.Errorf("Host(%q) = %q, want an error", tt.in, got)
			}
		})
	}
}

const sample = `
udp://tracker.example.com:1337/announce
http://tracker.example.com:6969/announce

# a comment
https://other.example.org/announce
http://107.189.2.131:1337/announce
udp://tracker.example.i2p:6969/announce
not a url at all
`

func TestParse(t *testing.T) {
	hosts, skipped, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []string{"other.example.org", "tracker.example.com"}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Errorf("hosts[%d] = %q, want %q (results must be sorted and deduplicated)", i, hosts[i], want[i])
		}
	}
	// IP literal, i2p address and the junk line.
	if len(skipped) != 3 {
		t.Errorf("skipped %d lines, want 3: %v", len(skipped), skipped)
	}
}

func TestParseEmpty(t *testing.T) {
	hosts, skipped, err := Parse(strings.NewReader("\n\n#only a comment\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(hosts) != 0 || len(skipped) != 0 {
		t.Errorf("got hosts=%v skipped=%v, want both empty", hosts, skipped)
	}
}

func TestParseFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	hosts, _, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if len(hosts) != 2 {
		t.Errorf("got %d hosts, want 2", len(hosts))
	}

	if _, _, err := ParseFile(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("ParseFile on a missing file should fail")
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(sample))
	}))
	defer srv.Close()

	hosts, _, err := Fetch(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(hosts) != 2 {
		t.Errorf("got %d hosts, want 2", len(hosts))
	}

	if _, _, err := Fetch(context.Background(), srv.URL+"/bad"); err == nil {
		t.Error("Fetch should fail on a 500 response")
	}
}

func TestSourcesAreParseableURLs(t *testing.T) {
	if len(Sources) == 0 {
		t.Fatal("no built-in sources defined")
	}
	for name, u := range Sources {
		if !strings.HasPrefix(u, "https://") {
			t.Errorf("source %q is not https: %q", name, u)
		}
	}
}
