// Package trackerlist parses BitTorrent announce URLs into resolvable tracker
// hostnames and fetches the published public tracker lists.
package trackerlist

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sources are the well-known public tracker lists, usable with Fetch.
var Sources = map[string]string{
	"ngosang":    "https://raw.githubusercontent.com/ngosang/trackerslist/master/trackers_all.txt",
	"xiu2":       "https://raw.githubusercontent.com/XIU2/TrackersListCollection/master/all.txt",
	"newtrackon": "https://newtrackon.com/api/all",
}

// Overlay networks that ordinary DNS cannot resolve.
var overlaySuffixes = []string{".i2p", ".onion", ".ygg"}

var hostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

// defaultPorts fill in an announce URL that named no port. 6969 is the
// convention for UDP trackers; the HTTP schemes take their usual ports.
var defaultPorts = map[string]int{"http": 80, "https": 443, "udp": 6969}

// Endpoint is an announce URL reduced to what it takes to reach the tracker:
// the hostname plus the transport, port and path to speak to it on. One
// hostname commonly serves several, and they can disagree about being alive.
type Endpoint struct {
	Host   string `json:"host"`
	Scheme string `json:"scheme"`
	Port   int    `json:"port"`
	Path   string `json:"path"`
}

// Probeable reports whether the endpoint speaks a tracker protocol we can
// check. Bare hostnames carry no scheme, and ws/wss trackers need a WebSocket
// handshake the prober does not implement.
func (e Endpoint) Probeable() bool {
	switch e.Scheme {
	case "udp", "http", "https":
		return e.Port > 0
	}
	return false
}

func (e Endpoint) String() string {
	if e.Scheme == "" {
		return e.Host
	}
	return fmt.Sprintf("%s://%s:%d%s", e.Scheme, e.Host, e.Port, e.Path)
}

// Label is the short form used in change details and the UI, where the
// hostname is already known from context.
func (e Endpoint) Label() string { return fmt.Sprintf("%s:%d", e.Scheme, e.Port) }

// ParseEndpoint splits an announce URL into its hostname and how to reach it.
// It returns an error for input that is not a DNS name we can usefully track
// over time: literal IP addresses, overlay-network addresses, and malformed
// URLs. Schemes we cannot probe still parse; they are simply not Probeable.
func ParseEndpoint(raw string) (Endpoint, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Endpoint{}, fmt.Errorf("empty line")
	}
	bare := !strings.Contains(raw, "://")
	if bare {
		// Accept a bare hostname or host:port too.
		raw = "tcp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Endpoint{}, fmt.Errorf("parse %q: %w", raw, err)
	}
	host := u.Hostname() // strips the port and any [] around a v6 literal
	if host == "" {
		return Endpoint{}, fmt.Errorf("no host in %q", raw)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	if net.ParseIP(host) != nil {
		return Endpoint{}, fmt.Errorf("%q is a literal IP address", host)
	}
	for _, s := range overlaySuffixes {
		if strings.HasSuffix(host, s) {
			return Endpoint{}, fmt.Errorf("%q is an overlay network address", host)
		}
	}
	if !hostPattern.MatchString(host) {
		return Endpoint{}, fmt.Errorf("%q is not a valid hostname", host)
	}

	ep := Endpoint{Host: host, Path: u.EscapedPath()}
	if !bare {
		// The tcp:// above was synthesised, not stated by the list.
		ep.Scheme = strings.ToLower(u.Scheme)
	}
	if p := u.Port(); p != "" {
		ep.Port, _ = strconv.Atoi(p)
	}
	if ep.Port == 0 {
		ep.Port = defaultPorts[ep.Scheme]
	}
	if ep.Path == "" {
		ep.Path = "/announce"
	}
	return ep, nil
}

// Host extracts the lowercase hostname from an announce URL.
func Host(raw string) (string, error) {
	ep, err := ParseEndpoint(raw)
	return ep.Host, err
}

// ParseEndpoints extracts the unique, sorted announce endpoints from a list of
// announce URLs, one per line. Blank lines and '#' comments are ignored.
// Unusable lines are returned as skip reasons rather than failing the parse.
func ParseEndpoints(r io.Reader) (eps []Endpoint, skipped []string, err error) {
	seen := map[Endpoint]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ep, err := ParseEndpoint(line)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if !seen[ep] {
			seen[ep] = true
			eps = append(eps, ep)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, err
	}
	sort.Slice(eps, func(i, j int) bool {
		a, b := eps[i], eps[j]
		switch {
		case a.Host != b.Host:
			return a.Host < b.Host
		case a.Scheme != b.Scheme:
			return a.Scheme < b.Scheme
		case a.Port != b.Port:
			return a.Port < b.Port
		}
		return a.Path < b.Path
	})
	return eps, skipped, nil
}

// Parse extracts the unique, sorted set of trackable hostnames from a list of
// announce URLs.
func Parse(r io.Reader) (hosts []string, skipped []string, err error) {
	eps, skipped, err := ParseEndpoints(r)
	if err != nil {
		return nil, skipped, err
	}
	return Hosts(eps), skipped, nil
}

// Hosts reduces endpoints to their unique, sorted hostnames. The endpoints are
// already sorted by host, so one pass suffices.
func Hosts(eps []Endpoint) []string {
	hosts := make([]string, 0, len(eps))
	for _, ep := range eps {
		if len(hosts) == 0 || hosts[len(hosts)-1] != ep.Host {
			hosts = append(hosts, ep.Host)
		}
	}
	return hosts
}

// ParseFile reads announce URLs from a file on disk.
func ParseFile(path string) (hosts []string, skipped []string, err error) {
	eps, skipped, err := ParseEndpointsFile(path)
	if err != nil {
		return nil, skipped, err
	}
	return Hosts(eps), skipped, nil
}

// ParseEndpointsFile reads announce endpoints from a file on disk.
func ParseEndpointsFile(path string) (eps []Endpoint, skipped []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return ParseEndpoints(f)
}

// Fetch downloads a tracker list. src may be a URL or one of the keys in
// Sources.
func Fetch(ctx context.Context, src string) (hosts []string, skipped []string, err error) {
	eps, skipped, err := FetchEndpoints(ctx, src)
	if err != nil {
		return nil, skipped, err
	}
	return Hosts(eps), skipped, nil
}

// FetchEndpoints downloads a tracker list and keeps the announce endpoints.
func FetchEndpoints(ctx context.Context, src string) (eps []Endpoint, skipped []string, err error) {
	target := src
	if u, ok := Sources[src]; ok {
		target = u
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("User-Agent", "torrent-tracker/1.0 (+https://github.com/pawal/torrent-tracker)")

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch %s: %w", target, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("fetch %s: unexpected status %s", target, resp.Status)
	}
	return ParseEndpoints(io.LimitReader(resp.Body, 8<<20))
}
