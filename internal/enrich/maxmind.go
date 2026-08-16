package enrich

import (
	"context"
	"fmt"
	"net/netip"

	"github.com/oschwald/maxminddb-golang/v2"
)

// MaxMind reads a local .mmdb for city-level geolocation. Optional: Cymru and
// RDAP already give a country, this adds city and coordinates.
type MaxMind struct {
	reader *maxminddb.Reader
	path   string
}

// OpenMaxMind opens a .mmdb file. The caller owns Close.
func OpenMaxMind(path string) (*MaxMind, error) {
	r, err := maxminddb.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open geoip database %s: %w", path, err)
	}
	return &MaxMind{reader: r, path: path}, nil
}

// Close releases the database.
func (m *MaxMind) Close() error {
	if m == nil || m.reader == nil {
		return nil
	}
	return m.reader.Close()
}

// Name identifies the provider.
func (m *MaxMind) Name() string { return "maxmind" }

// cityRecord covers the GeoLite2-City and GeoLite2-Country layouts; missing
// sections simply decode to their zero values.
type cityRecord struct {
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// Lookup reads the record for ip. An address absent from the database yields
// an empty Info rather than an error.
func (m *MaxMind) Lookup(_ context.Context, ip netip.Addr) (Info, error) {
	if m == nil || m.reader == nil {
		return Info{}, nil
	}

	var rec cityRecord
	result := m.reader.Lookup(ip.Unmap())
	if !result.Found() {
		return Info{}, nil
	}
	if err := result.Decode(&rec); err != nil {
		return Info{}, fmt.Errorf("geoip decode for %s: %w", ip, err)
	}

	country := rec.Country.ISOCode
	if country == "" {
		country = rec.RegisteredCountry.ISOCode
	}

	return Info{
		IP:        ip,
		Country:   country,
		City:      rec.City.Names["en"],
		Latitude:  rec.Location.Latitude,
		Longitude: rec.Location.Longitude,
		Sources:   []string{"maxmind"},
	}, nil
}
