package enrich

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/pawal/torrent-tracker/internal/resolver"
	"github.com/pawal/torrent-tracker/internal/store"
)

// Cymru queries the Team Cymru IP-to-ASN service, published as DNS TXT
// records. Keyless and reuses the collector's resolver, so it is the default.
// See https://team-cymru.com/community-services/ip-asn-mapping/
type Cymru struct {
	Resolver resolver.Resolver
	// Zone overrides the IPv4 origin zone (tests point this at a fake).
	Zone string
	// Zone6 overrides the IPv6 origin zone.
	Zone6 string
	// ASZone overrides the AS-name zone.
	ASZone string
}

// Name identifies the provider.
func (c *Cymru) Name() string { return "cymru" }

func (c *Cymru) zone(ip netip.Addr) string {
	if ip.Is6() && !ip.Is4In6() {
		if c.Zone6 != "" {
			return c.Zone6
		}
		return "origin6.asn.cymru.com"
	}
	if c.Zone != "" {
		return c.Zone
	}
	return "origin.asn.cymru.com"
}

func (c *Cymru) asZone() string {
	if c.ASZone != "" {
		return c.ASZone
	}
	return "asn.cymru.com"
}

// Lookup resolves the origin record for ip, then the AS name for the origin AS.
func (c *Cymru) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	name, err := OriginQName(ip, c.zone(ip))
	if err != nil {
		return Info{}, err
	}

	res := c.Resolver.Lookup(ctx, name, resolver.TypeTXT)
	switch {
	case res.Status == store.StatusNXDomain || res.Status == store.StatusNoData:
		// Not announced in BGP, or bogon space. Not an error.
		return Info{}, nil
	case res.Status != store.StatusOK:
		return Info{}, fmt.Errorf("cymru origin lookup for %s: %s %s", ip, res.Status, res.Err)
	}

	info, err := parseOrigin(res.TXT)
	if err != nil {
		return Info{}, fmt.Errorf("cymru origin for %s: %w", ip, err)
	}
	info.IP = ip

	// A second lookup turns the AS number into a human-readable holder.
	if info.ASN != 0 {
		asName := fmt.Sprintf("AS%d.%s", info.ASN, c.asZone())
		if r := c.Resolver.Lookup(ctx, asName, resolver.TypeTXT); r.Status == store.StatusOK {
			if n := parseASName(r.TXT); n != "" {
				info.ASName = n
			}
		}
	}
	return info, nil
}

// OriginQName builds the reversed-nibble query name for an address, e.g.
// 1.2.3.4 becomes 4.3.2.1.origin.asn.cymru.com.
func OriginQName(ip netip.Addr, zone string) (string, error) {
	if !ip.IsValid() {
		return "", fmt.Errorf("invalid address")
	}
	ip = ip.Unmap()

	if ip.Is4() {
		b := ip.As4()
		return fmt.Sprintf("%d.%d.%d.%d.%s", b[3], b[2], b[1], b[0], zone), nil
	}

	// IPv6 uses reversed nibbles, like ip6.arpa.
	b := ip.As16()
	var sb strings.Builder
	const hex = "0123456789abcdef"
	for i := len(b) - 1; i >= 0; i-- {
		sb.WriteByte(hex[b[i]&0x0f])
		sb.WriteByte('.')
		sb.WriteByte(hex[b[i]>>4])
		sb.WriteByte('.')
	}
	sb.WriteString(zone)
	return sb.String(), nil
}

// parseOrigin reads "13335 | 104.19.22.0/24 | US | arin | 2011-11-01".
// Multiple origins yield multiple records; the most specific prefix is the
// one actually routed, so it wins.
func parseOrigin(txt []string) (Info, error) {
	var (
		best     Info
		bestBits = -1
		found    bool
	)
	for _, rec := range txt {
		fields := splitPipe(rec)
		if len(fields) < 5 {
			continue
		}

		// The AS field may list several origins for the same prefix; take the
		// first, which is what the service reports as primary.
		asn, err := strconv.Atoi(strings.Fields(fields[0])[0])
		if err != nil {
			continue
		}

		info := Info{
			ASN:       asn,
			Prefix:    fields[1],
			Country:   strings.ToUpper(fields[2]),
			RIR:       strings.ToLower(fields[3]),
			Allocated: fields[4],
			Sources:   []string{"cymru"},
		}

		bits := prefixBits(info.Prefix)
		if bits > bestBits {
			best, bestBits, found = info, bits, true
		}
	}
	if !found {
		return Info{}, fmt.Errorf("no parseable origin record in %q", txt)
	}
	return best, nil
}

// parseASName reads records shaped like
//
//	"13335 | US | arin | 2010-07-14 | CLOUDFLARENET - Cloudflare, Inc., US"
func parseASName(txt []string) string {
	for _, rec := range txt {
		fields := splitPipe(rec)
		if len(fields) >= 5 && fields[4] != "" {
			return fields[4]
		}
	}
	return ""
}

func splitPipe(rec string) []string {
	parts := strings.Split(strings.Trim(rec, `"`), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// prefixBits returns the mask length of a CIDR string, or -1 if unparseable.
func prefixBits(cidr string) int {
	p, err := netip.ParsePrefix(cidr)
	if err != nil {
		return -1
	}
	return p.Bits()
}
