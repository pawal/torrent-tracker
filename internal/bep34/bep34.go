// Package bep34 reads the tracker preferences a hostname publishes in DNS. A
// BITTORRENT TXT record names the ports the host runs trackers on, and one
// naming none says it runs no trackers at all — the spec's opt-out.
package bep34

import (
	"sort"
	"strconv"
	"strings"
)

// keyword opens every record. BEP 34 makes the contents case-sensitive, so a
// lowercase spelling belongs to somebody else's TXT record.
const keyword = "BITTORRENT"

// maxPrefs bounds how much work one record can ask of us.
const maxPrefs = 10

// Pref is one advertised endpoint.
type Pref struct {
	Proto string // "udp" or "tcp"
	Port  int
}

// Record is what one name publishes. Prefs keep the order given, which the spec
// defines as most preferred first.
type Record struct {
	Raw   string
	Prefs []Pref
}

// Found reports whether the name published a record at all.
func (r Record) Found() bool { return r.Raw != "" }

// Denies reports whether the host says it runs no trackers. A record naming no
// endpoint is how the spec says it; there is no DENY keyword.
func (r Record) Denies() bool { return r.Found() && len(r.Prefs) == 0 }

// Parse reads the BITTORRENT record out of a name's TXT records. Whitespace is
// normalised so the stored text can be compared from one pass to the next.
func Parse(txts []string) Record {
	var found []string
	for _, txt := range txts {
		if fields := strings.Fields(txt); len(fields) > 0 && fields[0] == keyword {
			found = append(found, strings.Join(fields, " "))
		}
	}
	if len(found) == 0 {
		return Record{}
	}
	// More than one record is malformed. Taking the lowest keeps the stored
	// value stable instead of flapping with the order the answer arrived in.
	sort.Strings(found)

	rec := Record{Raw: found[0]}
	seen := map[Pref]bool{}
	for _, word := range strings.Fields(rec.Raw)[1:] {
		p, ok := pref(word)
		// Unrecognised words are ignored, which is what makes the format
		// extensible.
		if !ok || seen[p] {
			continue
		}
		seen[p] = true
		rec.Prefs = append(rec.Prefs, p)
		if len(rec.Prefs) == maxPrefs {
			break
		}
	}
	return rec
}

func pref(word string) (Pref, bool) {
	proto, port, ok := strings.Cut(word, ":")
	if !ok {
		return Pref{}, false
	}
	switch proto {
	case "UDP":
		proto = "udp"
	case "TCP":
		proto = "tcp"
	default:
		return Pref{}, false
	}
	if port == "" || strings.TrimLeft(port, "0123456789") != "" {
		return Pref{}, false
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return Pref{}, false
	}
	return Pref{Proto: proto, Port: n}, true
}
