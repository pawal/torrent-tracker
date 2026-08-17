package prober

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// signature reduces a reply to what identifies the software: the failure text
// it chose, or the shape of the dict it returned. Both are literals in the
// implementation. No tracker discloses a version.
func signature(body []byte) string {
	v, _, err := decode(body)
	if err != nil {
		return ""
	}
	d, ok := v.(bdict)
	if !ok {
		return ""
	}
	if reason, ok := d["failure reason"].(string); ok && reason != "" {
		return clean(reason)
	}
	return skeleton(d)
}

// maxSignature keeps a hostile or verbose reply from filling the column.
const maxSignature = 120

func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if len(s) > maxSignature {
		s = s[:maxSignature]
	}
	return s
}

// skeleton is the sorted key list of the reply, expanding nested dictionaries
// only when their keys are field names rather than data.
func skeleton(d bdict) string {
	var parts []string
	for _, k := range sortedKeys(d) {
		parts = append(parts, k)
		nested, ok := d[k].(bdict)
		if !ok || !fieldNames(nested) {
			continue
		}
		for _, nk := range sortedKeys(nested) {
			parts = append(parts, k+"."+nk)
		}
	}
	return clean(strings.Join(parts, ","))
}

func sortedKeys(d bdict) []string {
	keys := make([]string, 0, len(d))
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// fieldNames reports whether a dictionary is keyed by field names or by data.
// The scrape "files" dict is keyed by info_hash, which differs per request.
// Charset separates them; length does not, min_request_interval also being 20.
func fieldNames(d bdict) bool {
	if len(d) == 0 || len(d) > 8 {
		return false
	}
	for k := range d {
		if len(k) > 32 || !identifier(k) {
			return false
		}
	}
	return true
}

func identifier(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == ' ':
		default:
			return false
		}
	}
	return true
}

type bdict map[string]any

var errBencode = errors.New("not bencoded")

// decode reads one bencoded value, returning it and whatever follows. It is
// deliberately minimal: enough to see a reply's shape, not a general decoder.
func decode(b []byte) (any, []byte, error) {
	if len(b) == 0 {
		return nil, nil, errBencode
	}
	switch c := b[0]; {
	case c == 'i':
		end := indexByte(b, 'e')
		if end < 0 {
			return nil, nil, errBencode
		}
		n, err := strconv.ParseInt(string(b[1:end]), 10, 64)
		if err != nil {
			return nil, nil, errBencode
		}
		return n, b[end+1:], nil

	case c >= '0' && c <= '9':
		colon := indexByte(b, ':')
		if colon < 0 {
			return nil, nil, errBencode
		}
		n, err := strconv.Atoi(string(b[:colon]))
		if err != nil || n < 0 || colon+1+n > len(b) {
			return nil, nil, errBencode
		}
		return string(b[colon+1 : colon+1+n]), b[colon+1+n:], nil

	case c == 'l':
		rest := b[1:]
		var out []any
		for len(rest) > 0 && rest[0] != 'e' {
			v, tail, err := decode(rest)
			if err != nil {
				return nil, nil, err
			}
			out, rest = append(out, v), tail
		}
		if len(rest) == 0 {
			return nil, nil, errBencode
		}
		return out, rest[1:], nil

	case c == 'd':
		rest := b[1:]
		out := bdict{}
		for len(rest) > 0 && rest[0] != 'e' {
			k, tail, err := decode(rest)
			if err != nil {
				return nil, nil, err
			}
			key, ok := k.(string)
			if !ok {
				return nil, nil, fmt.Errorf("%w: non-string key", errBencode)
			}
			v, tail, err := decode(tail)
			if err != nil {
				return nil, nil, err
			}
			out[key], rest = v, tail
		}
		if len(rest) == 0 {
			return nil, nil, errBencode
		}
		return out, rest[1:], nil
	}
	return nil, nil, errBencode
}

func indexByte(b []byte, c byte) int {
	for i, got := range b {
		if got == c {
			return i
		}
	}
	return -1
}
