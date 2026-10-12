package enrich

import (
	"bytes"
	"encoding/binary"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// mmdbEntry maps one network to the record a lookup inside it decodes.
type mmdbEntry struct {
	prefix netip.Prefix
	record map[string]any
}

// writeMMDB writes a MaxMind DB holding entries and returns its path. It is
// the smallest writer the format allows: an IPv6 search tree with 32-bit
// records, IPv4 networks under ::/96 the way GeoLite2 stores them, and a data
// section of the few types these fixtures use. The point is to run the real
// reader over a real file, so a reader upgrade that decodes differently fails
// here rather than in production. Prefixes must not overlap.
func writeMMDB(t *testing.T, entries ...mmdbEntry) string {
	t.Helper()

	type node struct {
		next [2]*node
		leaf [2]int // 1 + index into entries; 0 is no data
	}
	root := &node{}
	for i, e := range entries {
		var addr [16]byte
		bits := e.prefix.Bits()
		if e.prefix.Addr().Is4() {
			a4 := e.prefix.Addr().As4()
			copy(addr[12:], a4[:])
			bits += 96
		} else {
			addr = e.prefix.Addr().As16()
		}
		n := root
		for d := range bits - 1 {
			b := addr[d/8] >> (7 - d%8) & 1
			if n.next[b] == nil {
				n.next[b] = &node{}
			}
			n = n.next[b]
		}
		last := bits - 1
		n.leaf[addr[last/8]>>(7-last%8)&1] = i + 1
	}

	// Number the nodes breadth-first; the root must be node 0.
	nodes := []*node{root}
	index := map[*node]uint32{root: 0}
	for i := 0; i < len(nodes); i++ {
		for _, c := range nodes[i].next {
			if c != nil {
				index[c] = uint32(len(nodes))
				nodes = append(nodes, c)
			}
		}
	}

	var data bytes.Buffer
	offsets := make([]uint32, len(entries))
	for i, e := range entries {
		offsets[i] = uint32(data.Len())
		mmdbEncode(&data, e.record)
	}

	// A record below node_count points at a node, node_count itself means no
	// data, and anything above is node_count + 16 + an offset into the data
	// section (the 16 being the zeroed separator between tree and data).
	nodeCount := uint32(len(nodes))
	var out bytes.Buffer
	for _, n := range nodes {
		for b := range 2 {
			v := nodeCount
			switch {
			case n.next[b] != nil:
				v = index[n.next[b]]
			case n.leaf[b] != 0:
				v = nodeCount + 16 + offsets[n.leaf[b]-1]
			}
			out.Write(binary.BigEndian.AppendUint32(nil, v))
		}
	}
	out.Write(make([]byte, 16))
	out.Write(data.Bytes())
	out.WriteString("\xab\xcd\xefMaxMind.com")
	mmdbEncode(&out, map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 uint64(1_790_000_000),
		"database_type":               "GeoLite2-City",
		"description":                 map[string]any{"en": "torrent-tracker test fixture"},
		"ip_version":                  uint16(6),
		"languages":                   []any{"en"},
		"node_count":                  nodeCount,
		"record_size":                 uint16(32),
	})

	path := filepath.Join(t.TempDir(), "test.mmdb")
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// mmdbEncode appends v in the data section encoding. Map keys are sorted so
// the file is the same on every run.
func mmdbEncode(buf *bytes.Buffer, v any) {
	switch v := v.(type) {
	case string:
		mmdbControl(buf, 2, len(v))
		buf.WriteString(v)
	case float64:
		mmdbControl(buf, 3, 8)
		buf.Write(binary.BigEndian.AppendUint64(nil, math.Float64bits(v)))
	case uint16:
		mmdbControl(buf, 5, 2)
		buf.Write(binary.BigEndian.AppendUint16(nil, v))
	case uint32:
		mmdbControl(buf, 6, 4)
		buf.Write(binary.BigEndian.AppendUint32(nil, v))
	case uint64:
		mmdbControl(buf, 9, 8)
		buf.Write(binary.BigEndian.AppendUint64(nil, v))
	case map[string]any:
		mmdbControl(buf, 7, len(v))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			mmdbEncode(buf, k)
			mmdbEncode(buf, v[k])
		}
	case []any:
		mmdbControl(buf, 11, len(v))
		for _, e := range v {
			mmdbEncode(buf, e)
		}
	default:
		panic("mmdbEncode: unsupported fixture type")
	}
}

// mmdbControl writes a field's control byte: the type in the top three bits
// (types above 7 are "extended", spilling into a second byte), the size in the
// low five, with one extra size byte for 29 to 284.
func mmdbControl(buf *bytes.Buffer, typ, size int) {
	sizeBits, extra := byte(size), []byte(nil)
	if size >= 29 {
		if size >= 29+256 {
			panic("mmdbControl: fixture field too long")
		}
		sizeBits, extra = 29, []byte{byte(size - 29)}
	}
	if typ < 8 {
		buf.WriteByte(byte(typ)<<5 | sizeBits)
	} else {
		buf.WriteByte(sizeBits)
		buf.WriteByte(byte(typ - 7))
	}
	buf.Write(extra)
}

func openTestMaxMind(t *testing.T, entries ...mmdbEntry) *MaxMind {
	t.Helper()
	m, err := OpenMaxMind(writeMMDB(t, entries...))
	if err != nil {
		t.Fatalf("OpenMaxMind: %v", err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}

func TestMaxMindLookup(t *testing.T) {
	m := openTestMaxMind(t,
		mmdbEntry{netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
			"city":               map[string]any{"names": map[string]any{"en": "Stockholm", "sv": "Stockholm"}},
			"country":            map[string]any{"iso_code": "SE"},
			"registered_country": map[string]any{"iso_code": "SE"},
			"location":           map[string]any{"latitude": 59.3293, "longitude": 18.0686},
		}},
		mmdbEntry{netip.MustParsePrefix("2001:db8::/32"), map[string]any{
			"country":  map[string]any{"iso_code": "DE"},
			"location": map[string]any{"latitude": 51.2993, "longitude": 9.491},
		}},
	)
	if m.Name() != "maxmind" {
		t.Errorf("Name() = %q, want maxmind", m.Name())
	}

	tests := []struct {
		ip            string
		country, city string
		lat, lon      float64
	}{
		{"192.0.2.10", "SE", "Stockholm", 59.3293, 18.0686},
		// A dual-stack socket reports IPv4 peers as mapped addresses. The tree
		// keeps IPv4 under ::/96, not ::ffff:0:0/96, so the address must be
		// unmapped before the lookup or it finds nothing.
		{"::ffff:192.0.2.10", "SE", "Stockholm", 59.3293, 18.0686},
		{"2001:db8::1", "DE", "", 51.2993, 9.491},
	}
	for _, tt := range tests {
		ip := netip.MustParseAddr(tt.ip)
		got, err := m.Lookup(t.Context(), ip)
		if err != nil {
			t.Errorf("Lookup(%s): %v", tt.ip, err)
			continue
		}
		want := Info{
			IP: ip, Country: tt.country, City: tt.city,
			Latitude: tt.lat, Longitude: tt.lon,
			Sources: []string{"maxmind"},
		}
		if got.IP != want.IP || got.Country != want.Country || got.City != want.City ||
			got.Latitude != want.Latitude || got.Longitude != want.Longitude ||
			!slices.Equal(got.Sources, want.Sources) {
			t.Errorf("Lookup(%s) = %+v, want %+v", tt.ip, got, want)
		}
	}
}

// GeoLite2-Country has no city or location, and some networks carry only the
// country the block is registered to. Both must still yield a country.
func TestMaxMindLookupFallsBackToRegisteredCountry(t *testing.T) {
	m := openTestMaxMind(t,
		mmdbEntry{netip.MustParsePrefix("198.51.100.0/24"), map[string]any{
			"registered_country": map[string]any{"iso_code": "NL"},
		}},
		mmdbEntry{netip.MustParsePrefix("203.0.113.0/24"), map[string]any{
			"city":    map[string]any{"names": map[string]any{"de": "München"}},
			"country": map[string]any{"iso_code": "DE"},
		}},
	)

	got, err := m.Lookup(t.Context(), netip.MustParseAddr("198.51.100.7"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Country != "NL" || got.City != "" || got.Latitude != 0 {
		t.Errorf("registered-only record = %+v, want country NL and nothing else", got)
	}

	// Only English names are used; a city named in other languages alone is
	// left blank rather than shown in whichever one happened to be there.
	got, err = m.Lookup(t.Context(), netip.MustParseAddr("203.0.113.7"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Country != "DE" || got.City != "" {
		t.Errorf("non-English city = %+v, want country DE and no city", got)
	}
}

func TestMaxMindLookupAbsentAddress(t *testing.T) {
	m := openTestMaxMind(t, mmdbEntry{netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"country": map[string]any{"iso_code": "SE"},
	}})
	for _, ip := range []string{"198.51.100.1", "2001:db8::1"} {
		got, err := m.Lookup(t.Context(), netip.MustParseAddr(ip))
		if err != nil || !got.Empty() {
			t.Errorf("Lookup(%s) = %+v, %v; want an empty Info and no error", ip, got, err)
		}
	}
}

// A record the struct cannot hold is an error to report, not a zero value to
// pass off as "somewhere at 0,0".
func TestMaxMindLookupDecodeError(t *testing.T) {
	m := openTestMaxMind(t, mmdbEntry{netip.MustParsePrefix("192.0.2.0/24"), map[string]any{
		"location": map[string]any{"latitude": "north"},
	}})
	if _, err := m.Lookup(t.Context(), netip.MustParseAddr("192.0.2.1")); err == nil {
		t.Error("want an error for a latitude that is not a number")
	}
}
