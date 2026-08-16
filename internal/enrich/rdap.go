package enrich

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// IANA publishes which RIR serves which address range. Using the bootstrap
// directly keeps us off third-party redirectors like rdap.org.
const (
	bootstrapV4 = "https://data.iana.org/rdap/ipv4.json"
	bootstrapV6 = "https://data.iana.org/rdap/ipv6.json"
)

// RDAP queries the holding RIR for authoritative registry data: network name,
// holder and country. Requests are throttled because RIRs rate-limit hard.
type RDAP struct {
	Client *http.Client
	// MinInterval throttles consecutive requests. Defaults to 1s.
	MinInterval time.Duration
	// BootstrapV4/V6 override the IANA URLs in tests.
	BootstrapV4 string
	BootstrapV6 string

	mu       sync.Mutex
	lastCall time.Time
	services map[bool][]bootstrapService // keyed by isV6
}

type bootstrapService struct {
	prefixes []netip.Prefix
	urls     []string
}

// Name identifies the provider.
func (r *RDAP) Name() string { return "rdap" }

func (r *RDAP) client() *http.Client {
	if r.Client != nil {
		return r.Client
	}
	return &http.Client{Timeout: 20 * time.Second}
}

func (r *RDAP) bootstrapURL(v6 bool) string {
	if v6 {
		if r.BootstrapV6 != "" {
			return r.BootstrapV6
		}
		return bootstrapV6
	}
	if r.BootstrapV4 != "" {
		return r.BootstrapV4
	}
	return bootstrapV4
}

// rdapResponse is the subset of RFC 9083 we care about.
type rdapResponse struct {
	Handle    string `json:"handle"`
	Name      string `json:"name"`
	Country   string `json:"country"`
	Type      string `json:"type"`
	StartAddr string `json:"startAddress"`
	EndAddr   string `json:"endAddress"`
	Entities  []struct {
		Handle     string          `json:"handle"`
		Roles      []string        `json:"roles"`
		VCardArray json.RawMessage `json:"vcardArray"`
	} `json:"entities"`
}

// Lookup fetches the registry record for ip.
func (r *RDAP) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	ip = ip.Unmap()
	base, err := r.endpointFor(ctx, ip)
	if err != nil {
		return Info{}, err
	}

	body, err := r.get(ctx, strings.TrimSuffix(base, "/")+"/ip/"+ip.String())
	if err != nil {
		return Info{}, err
	}

	var resp rdapResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Info{}, fmt.Errorf("rdap decode for %s: %w", ip, err)
	}

	info := Info{
		IP:          ip,
		NetworkName: resp.Name,
		Country:     strings.ToUpper(resp.Country),
		Sources:     []string{"rdap"},
	}
	if org := pickOrg(resp); org != "" {
		info.Org = org
	}
	return info, nil
}

// pickOrg finds the most meaningful holder name among the entities, preferring
// the registrant over administrative or technical contacts.
func pickOrg(resp rdapResponse) string {
	best, bestRank := "", -1
	rank := map[string]int{"registrant": 3, "administrative": 2, "technical": 1}

	for _, e := range resp.Entities {
		r := 0
		for _, role := range e.Roles {
			if v, ok := rank[strings.ToLower(role)]; ok && v > r {
				r = v
			}
		}
		if r == 0 {
			continue
		}
		name := vcardName(e.VCardArray)
		if name == "" {
			name = e.Handle
		}
		if name != "" && r > bestRank {
			best, bestRank = name, r
		}
	}
	return best
}

// vcardName digs the "fn" (formatted name) out of a jCard array, which is
// nested as ["vcard", [ ["fn", {}, "text", "Example Org"], ... ]].
func vcardName(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var card []any
	if err := json.Unmarshal(raw, &card); err != nil || len(card) < 2 {
		return ""
	}
	props, ok := card[1].([]any)
	if !ok {
		return ""
	}
	for _, p := range props {
		entry, ok := p.([]any)
		if !ok || len(entry) < 4 {
			continue
		}
		if key, _ := entry[0].(string); strings.EqualFold(key, "fn") {
			if v, ok := entry[3].(string); ok {
				return v
			}
		}
	}
	return ""
}

// endpointFor resolves which RIR serves ip, loading the bootstrap on first use.
func (r *RDAP) endpointFor(ctx context.Context, ip netip.Addr) (string, error) {
	v6 := ip.Is6()

	r.mu.Lock()
	loaded := r.services != nil && r.services[v6] != nil
	r.mu.Unlock()

	if !loaded {
		if err := r.loadBootstrap(ctx, v6); err != nil {
			return "", err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Most specific matching prefix wins.
	best, bestBits := "", -1
	for _, svc := range r.services[v6] {
		for _, p := range svc.prefixes {
			if p.Contains(ip) && p.Bits() > bestBits {
				if url := preferHTTPS(svc.urls); url != "" {
					best, bestBits = url, p.Bits()
				}
			}
		}
	}
	if best == "" {
		return "", fmt.Errorf("no RDAP service for %s", ip)
	}
	return best, nil
}

func preferHTTPS(urls []string) string {
	for _, u := range urls {
		if strings.HasPrefix(u, "https://") {
			return u
		}
	}
	if len(urls) > 0 {
		return urls[0]
	}
	return ""
}

func (r *RDAP) loadBootstrap(ctx context.Context, v6 bool) error {
	body, err := r.get(ctx, r.bootstrapURL(v6))
	if err != nil {
		return fmt.Errorf("rdap bootstrap: %w", err)
	}

	var doc struct {
		Services [][][]string `json:"services"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("rdap bootstrap decode: %w", err)
	}

	var services []bootstrapService
	for _, entry := range doc.Services {
		if len(entry) < 2 {
			continue
		}
		svc := bootstrapService{urls: entry[1]}
		for _, cidr := range entry[0] {
			// The v4 table uses bare "1.0.0.0/8"; some entries omit the mask.
			p, err := netip.ParsePrefix(cidr)
			if err != nil {
				continue
			}
			svc.prefixes = append(svc.prefixes, p)
		}
		if len(svc.prefixes) > 0 && len(svc.urls) > 0 {
			services = append(services, svc)
		}
	}
	if len(services) == 0 {
		return fmt.Errorf("rdap bootstrap for v6=%t contained no usable services", v6)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.services == nil {
		r.services = map[bool][]bootstrapService{}
	}
	r.services[v6] = services
	return nil
}

// get performs a throttled GET.
func (r *RDAP) get(ctx context.Context, url string) ([]byte, error) {
	r.throttle(ctx)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/rdap+json, application/json")
	req.Header.Set("User-Agent", "torrent-tracker/1.0 (+https://github.com/pawal/torrent-tracker)")

	resp, err := r.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// throttle enforces MinInterval between requests so we stay welcome at the
// registries.
func (r *RDAP) throttle(ctx context.Context) {
	interval := r.MinInterval
	if interval <= 0 {
		interval = time.Second
	}

	r.mu.Lock()
	wait := time.Until(r.lastCall.Add(interval))
	if wait < 0 {
		wait = 0
	}
	r.lastCall = time.Now().Add(wait)
	r.mu.Unlock()

	if wait == 0 {
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
