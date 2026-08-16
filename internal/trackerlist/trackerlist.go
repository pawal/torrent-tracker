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

// Host extracts the lowercase hostname from an announce URL. It returns an
// error for input that is not a DNS name we can usefully track over time:
// literal IP addresses, overlay-network addresses, and malformed URLs.
func Host(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("empty line")
	}
	if !strings.Contains(raw, "://") {
		// Accept a bare hostname or host:port too.
		raw = "tcp://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", raw, err)
	}
	host := u.Hostname() // strips the port and any [] around a v6 literal
	if host == "" {
		return "", fmt.Errorf("no host in %q", raw)
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))

	if net.ParseIP(host) != nil {
		return "", fmt.Errorf("%q is a literal IP address", host)
	}
	for _, s := range overlaySuffixes {
		if strings.HasSuffix(host, s) {
			return "", fmt.Errorf("%q is an overlay network address", host)
		}
	}
	if !hostPattern.MatchString(host) {
		return "", fmt.Errorf("%q is not a valid hostname", host)
	}
	return host, nil
}

// Parse extracts the unique, sorted set of trackable hostnames from a list of
// announce URLs, one per line. Blank lines and '#' comments are ignored.
// Unusable lines are returned as skip reasons rather than failing the parse.
func Parse(r io.Reader) (hosts []string, skipped []string, err error) {
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		host, err := Host(line)
		if err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		if !seen[host] {
			seen[host] = true
			hosts = append(hosts, host)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, skipped, err
	}
	sort.Strings(hosts)
	return hosts, skipped, nil
}

// ParseFile reads announce URLs from a file on disk.
func ParseFile(path string) (hosts []string, skipped []string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	return Parse(f)
}

// Fetch downloads a tracker list. src may be a URL or one of the keys in
// Sources.
func Fetch(ctx context.Context, src string) (hosts []string, skipped []string, err error) {
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
	return Parse(io.LimitReader(resp.Body, 8<<20))
}
