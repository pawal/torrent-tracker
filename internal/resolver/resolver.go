// Package resolver performs the collector's DNS lookups, exposing rcodes and
// timings so "gone" stays distinguishable from "our resolver broke".
package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"codeberg.org/miekg/dns"
	"codeberg.org/miekg/dns/dnsconf"

	"github.com/pawal/torrent-tracker/internal/store"
)

// RRType is the record type to query.
type RRType int

const (
	TypeA RRType = iota
	TypeAAAA
	// TypeTXT backs the Team Cymru IP-to-ASN service, which is published
	// entirely over DNS.
	TypeTXT
)

// Family returns the IP version a record type yields, or 0 for TXT.
func (t RRType) Family() int {
	switch t {
	case TypeAAAA:
		return 6
	case TypeTXT:
		return 0
	default:
		return 4
	}
}

func (t RRType) String() string {
	switch t {
	case TypeAAAA:
		return "AAAA"
	case TypeTXT:
		return "TXT"
	default:
		return "A"
	}
}

func (t RRType) dnsType() uint16 {
	switch t {
	case TypeAAAA:
		return dns.TypeAAAA
	case TypeTXT:
		return dns.TypeTXT
	default:
		return dns.TypeA
	}
}

// Result is the outcome of one query.
type Result struct {
	Status store.Status
	Addrs  []string
	// TXT holds the joined strings of each TXT record, one entry per record.
	TXT []string
	TTL uint32
	RTT time.Duration
	Err string
}

// Resolver looks up one record type for one name. The interface keeps the
// collector testable without a network.
type Resolver interface {
	Lookup(ctx context.Context, name string, rr RRType) Result
}

// DNS is a Resolver backed by a real recursive resolver.
type DNS struct {
	// Servers are the upstream resolvers as host:port. Queries fall through
	// the list until one answers.
	Servers []string
	// Timeout bounds a single query.
	Timeout time.Duration
	// Retries is the number of extra attempts per server after a failure.
	Retries int

	client *dns.Client
}

// New builds a DNS resolver. If servers is empty the system resolvers from
// /etc/resolv.conf are used. Bare addresses get the default port 53.
func New(servers []string, timeout time.Duration, retries int) (*DNS, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if len(servers) == 0 {
		conf, err := dnsconf.FromFile("/etc/resolv.conf")
		if err != nil {
			return nil, fmt.Errorf("read system resolvers: %w", err)
		}
		port := conf.Port
		if port == "" {
			port = "53"
		}
		for _, s := range conf.Servers {
			servers = append(servers, net.JoinHostPort(s, port))
		}
	}

	normalised := make([]string, 0, len(servers))
	for _, s := range servers {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(s); err != nil {
			s = net.JoinHostPort(s, "53")
		}
		normalised = append(normalised, s)
	}
	if len(normalised) == 0 {
		return nil, errors.New("no resolvers configured")
	}

	return &DNS{
		Servers: normalised,
		Timeout: timeout,
		Retries: retries,
		client: &dns.Client{Transport: &dns.Transport{
			Dialer:       &net.Dialer{Timeout: timeout},
			ReadTimeout:  timeout,
			WriteTimeout: timeout,
		}},
	}, nil
}

// Lookup queries the configured servers in turn and returns the first usable
// answer. "Usable" means the server actually responded, whatever the rcode;
// transport failures fall through to the next server.
func (d *DNS) Lookup(ctx context.Context, name string, rr RRType) Result {
	var last Result

	for _, server := range d.Servers {
		for attempt := 0; attempt <= d.Retries; attempt++ {
			if err := ctx.Err(); err != nil {
				return Result{Status: store.StatusTimeout, Err: err.Error()}
			}

			// A fresh message per attempt gets a fresh ID and a clean buffer.
			msg := dns.NewMsg(name, rr.dnsType())
			if msg == nil {
				return Result{Status: store.StatusError, Err: "unsupported record type " + rr.String()}
			}
			msg.UDPSize = 4096 // EDNS0: ask for answers that fit without TCP

			qctx, cancel := context.WithTimeout(ctx, d.Timeout)
			reply, rtt, err := d.client.Exchange(qctx, msg, "udp", server)
			cancel()

			if err != nil {
				last = Result{Status: classifyErr(err), RTT: rtt, Err: err.Error()}
				continue
			}
			if reply.Truncated {
				// Retry over TCP; keep the UDP answer if TCP also fails.
				tcpMsg := dns.NewMsg(name, rr.dnsType())
				qctx, cancel := context.WithTimeout(ctx, d.Timeout)
				tcpReply, tcpRTT, tcpErr := d.client.Exchange(qctx, tcpMsg, "tcp", server)
				cancel()
				if tcpErr == nil {
					reply, rtt = tcpReply, tcpRTT
				}
			}
			return fromReply(reply, rr, rtt)
		}
	}

	if last.Status == "" {
		last = Result{Status: store.StatusError, Err: "no resolver answered"}
	}
	return last
}

func classifyErr(err error) store.Status {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return store.StatusTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return store.StatusTimeout
	}
	return store.StatusError
}

func fromReply(reply *dns.Msg, rr RRType, rtt time.Duration) Result {
	res := Result{RTT: rtt}

	switch reply.Rcode {
	case dns.RcodeSuccess:
		// fall through to address extraction
	case dns.RcodeNameError:
		res.Status = store.StatusNXDomain
		return res
	case dns.RcodeServerFailure, dns.RcodeRefused:
		res.Status = store.StatusServFail
		res.Err = rcodeName(reply.Rcode)
		return res
	default:
		res.Status = store.StatusError
		res.Err = rcodeName(reply.Rcode)
		return res
	}

	for _, ans := range reply.Answer {
		switch a := ans.(type) {
		case *dns.A:
			if rr == TypeA && a.Addr.IsValid() {
				res.Addrs = append(res.Addrs, a.Addr.String())
				res.TTL = ttlMin(res.TTL, a.Hdr.TTL)
			}
		case *dns.AAAA:
			if rr == TypeAAAA && a.Addr.IsValid() {
				res.Addrs = append(res.Addrs, a.Addr.String())
				res.TTL = ttlMin(res.TTL, a.Hdr.TTL)
			}
		case *dns.TXT:
			if rr == TypeTXT {
				// A TXT record's character-strings are one logical value.
				res.TXT = append(res.TXT, strings.Join(a.Txt, ""))
				res.TTL = ttlMin(res.TTL, a.Hdr.TTL)
			}
		}
	}
	sort.Strings(res.Addrs)

	if len(res.Addrs) == 0 && len(res.TXT) == 0 {
		res.Status = store.StatusNoData
	} else {
		res.Status = store.StatusOK
	}
	return res
}

func rcodeName(rcode uint16) string {
	if s, ok := dns.RcodeToString[rcode]; ok {
		return s
	}
	return fmt.Sprintf("RCODE%d", rcode)
}

func ttlMin(cur, next uint32) uint32 {
	if cur == 0 || next < cur {
		return next
	}
	return cur
}
