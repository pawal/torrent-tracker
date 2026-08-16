// Package enrich annotates IP addresses with their origin AS, allocating RIR
// and location, so an address change can be read as a change of network.
package enrich

import (
	"context"
	"errors"
	"log/slog"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Info is what we know about an address's network placement. Every field is
// optional: providers fill in what they can and the chain merges the results.
type Info struct {
	IP     netip.Addr `json:"ip"`
	ASN    int        `json:"asn,omitempty"`
	ASName string     `json:"as_name,omitempty"`
	Prefix string     `json:"prefix,omitempty"`
	// RIR is the registry that allocated the prefix: ripencc, arin, apnic,
	// lacnic or afrinic.
	RIR         string  `json:"rir,omitempty"`
	Country     string  `json:"country,omitempty"`
	Allocated   string  `json:"allocated,omitempty"`
	NetworkName string  `json:"network_name,omitempty"`
	Org         string  `json:"org,omitempty"`
	City        string  `json:"city,omitempty"`
	Latitude    float64 `json:"latitude,omitempty"`
	Longitude   float64 `json:"longitude,omitempty"`
	// Sources lists the providers that contributed, for provenance.
	Sources []string `json:"sources,omitempty"`
}

// Empty reports whether nothing useful was learned.
func (i Info) Empty() bool {
	return i.ASN == 0 && i.RIR == "" && i.Country == "" && i.NetworkName == "" && i.Org == "" && i.City == ""
}

// merge copies non-empty fields from other, without overwriting what is
// already set. Providers are consulted cheapest-first, so first answer wins.
func (i *Info) merge(other Info) {
	if i.ASN == 0 {
		i.ASN = other.ASN
	}
	setStr(&i.ASName, other.ASName)
	setStr(&i.Prefix, other.Prefix)
	setStr(&i.RIR, other.RIR)
	setStr(&i.Country, other.Country)
	setStr(&i.Allocated, other.Allocated)
	setStr(&i.NetworkName, other.NetworkName)
	setStr(&i.Org, other.Org)
	setStr(&i.City, other.City)
	if i.Latitude == 0 && i.Longitude == 0 {
		i.Latitude, i.Longitude = other.Latitude, other.Longitude
	}
	i.Sources = append(i.Sources, other.Sources...)
}

func setStr(dst *string, v string) {
	if *dst == "" && v != "" {
		*dst = v
	}
}

// Provider looks up one aspect of an address.
type Provider interface {
	// Name identifies the provider in the Sources list.
	Name() string
	// Lookup returns what this provider knows. A provider that simply has no
	// data should return a zero Info and a nil error.
	Lookup(ctx context.Context, ip netip.Addr) (Info, error)
}

// Chain queries providers in order and merges their answers.
type Chain struct {
	Providers []Provider
	Log       *slog.Logger
}

// Name identifies the chain.
func (c *Chain) Name() string { return "chain" }

// Lookup consults every provider. A provider that fails is logged and skipped:
// partial enrichment is far more useful than none, and these are all
// best-effort third-party sources.
func (c *Chain) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	info := Info{IP: ip}
	var errs []error

	for _, p := range c.Providers {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		got, err := p.Lookup(ctx, ip)
		if err != nil {
			errs = append(errs, err)
			if c.Log != nil {
				c.Log.Debug("enrichment provider failed", "provider", p.Name(), "ip", ip, "err", err)
			}
			continue
		}
		if got.Empty() {
			continue
		}
		if len(got.Sources) == 0 {
			got.Sources = []string{p.Name()}
		}
		info.merge(got)
	}

	// Only surface an error when we learned nothing at all.
	if info.Empty() && len(errs) > 0 {
		return info, errors.Join(errs...)
	}
	return info, nil
}

// Describe renders the network placement for a log line or the change feed,
// e.g. "AS13335 Cloudflare, Inc. (arin, US)".
func (i Info) Describe() string {
	var b strings.Builder
	if i.ASN != 0 {
		b.WriteString("AS")
		b.WriteString(strconv.Itoa(i.ASN))
	}
	holder := i.Org
	if holder == "" {
		holder = i.ASName
	}
	if holder == "" {
		holder = i.NetworkName
	}
	if holder != "" {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString(holder)
	}

	var meta []string
	if i.RIR != "" {
		meta = append(meta, i.RIR)
	}
	if i.Country != "" {
		meta = append(meta, i.Country)
	}
	if len(meta) > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("(" + strings.Join(meta, ", ") + ")")
	}
	return b.String()
}

// Timeout wraps a provider with a per-lookup deadline so one slow registry
// cannot stall a whole collection pass.
type Timeout struct {
	Provider Provider
	Limit    time.Duration
}

// Name identifies the wrapped provider.
func (t Timeout) Name() string { return t.Provider.Name() }

// Lookup applies the deadline.
func (t Timeout) Lookup(ctx context.Context, ip netip.Addr) (Info, error) {
	if t.Limit <= 0 {
		return t.Provider.Lookup(ctx, ip)
	}
	ctx, cancel := context.WithTimeout(ctx, t.Limit)
	defer cancel()
	return t.Provider.Lookup(ctx, ip)
}
