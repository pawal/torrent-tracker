// Package prober asks a tracker endpoint whether it still speaks the
// BitTorrent tracker protocol. Resolving in DNS proves nothing: dead trackers
// keep their names for years, so the only definitive check is the protocol.
package prober

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// State is the outcome of one probe.
type State string

const (
	Live    State = "live"    // spoke the tracker protocol
	Dead    State = "dead"    // reachable or not, but not a tracker
	Unknown State = "unknown" // the probe could not be made
)

// Target is one endpoint on one address. Host is kept alongside IP because
// HTTP needs it for SNI and the Host header even though the dial is by address.
type Target struct {
	Host   string
	IP     string
	Scheme string
	Port   int
	Path   string
}

func (t Target) String() string {
	return fmt.Sprintf("%s://%s:%d%s [%s]", t.Scheme, t.Host, t.Port, t.Path, t.IP)
}

// Result is one probe's verdict.
type Result struct {
	State  State
	Reason string
	RTT    time.Duration
}

// Prober probes endpoints. The zero value is usable.
type Prober struct {
	// Timeout bounds one attempt. Defaults to 5s.
	Timeout time.Duration
	// UserAgent identifies us to HTTP trackers.
	UserAgent string
}

func (p *Prober) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 5 * time.Second
}

func (p *Prober) userAgent() string {
	if p.UserAgent != "" {
		return p.UserAgent
	}
	return "torrent-tracker/1.0 (+https://github.com/pawal/torrent-tracker)"
}

// Probe checks one endpoint on one address.
func (p *Prober) Probe(ctx context.Context, t Target) Result {
	if net.ParseIP(t.IP) == nil {
		return Result{State: Unknown, Reason: "no address to probe"}
	}
	ctx, cancel := context.WithTimeout(ctx, p.timeout())
	defer cancel()

	switch t.Scheme {
	case "udp":
		return p.probeUDP(ctx, t)
	case "http", "https":
		return p.probeHTTP(ctx, t)
	}
	return Result{State: Unknown, Reason: "unsupported scheme " + t.Scheme}
}

// udpProtocolID is the magic constant every BEP 15 connect request opens with.
const udpProtocolID = 0x41727101980

// actionConnect and actionError are the two replies a connect request can
// legitimately draw. Either proves a tracker is listening.
const (
	actionConnect = 0
	actionError   = 3
)

// probeUDP performs the BEP 15 connect handshake: 16 bytes out, 16 back, no
// info_hash needed. A reply carrying our transaction id is proof of a tracker.
func (p *Prober) probeUDP(ctx context.Context, t Target) Result {
	start := time.Now()
	addr := net.JoinHostPort(t.IP, strconv.Itoa(t.Port))

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", addr)
	if err != nil {
		return Result{State: Dead, Reason: dialReason(err), RTT: time.Since(start)}
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}

	txid := rand.Uint32()
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], udpProtocolID)
	binary.BigEndian.PutUint32(req[8:12], actionConnect)
	binary.BigEndian.PutUint32(req[12:16], txid)
	if _, err := conn.Write(req); err != nil {
		return Result{State: Dead, Reason: dialReason(err), RTT: time.Since(start)}
	}

	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	rtt := time.Since(start)
	if err != nil {
		return Result{State: Dead, Reason: dialReason(err), RTT: rtt}
	}
	if n < 16 {
		return Result{State: Dead, Reason: fmt.Sprintf("short reply (%d bytes)", n), RTT: rtt}
	}

	// A stray datagram from something else on the port is not our answer.
	if got := binary.BigEndian.Uint32(buf[4:8]); got != txid {
		return Result{State: Dead, Reason: "transaction id mismatch", RTT: rtt}
	}
	switch binary.BigEndian.Uint32(buf[0:4]) {
	case actionConnect, actionError:
		return Result{State: Live, RTT: rtt}
	}
	return Result{State: Dead, Reason: "not a tracker reply", RTT: rtt}
}

// probeHTTP tries scrape (BEP 48) first, falling back to announce for the
// trackers that never implemented scrape. A bencoded reply is the signature:
// even a rejection comes back as a bencoded failure reason.
func (p *Prober) probeHTTP(ctx context.Context, t Target) Result {
	client := p.httpClient(t)
	defer client.CloseIdleConnections()

	res := p.request(ctx, client, t, scrapeURL(t))
	// A second request only helps when the server answered but not as a
	// tracker: plenty of trackers never implemented scrape.
	if res.State == Live || !res.answered {
		return res.Result
	}
	if fallback := p.request(ctx, client, t, announceURL(t)); fallback.State == Live {
		return fallback.Result
	}
	return res.Result
}

// httpClient dials the address we chose while presenting the hostname, so
// virtual hosts and SNI still work. Certificates go unverified on purpose: an
// expired cert says nothing about whether a tracker is answering, and nothing
// secret crosses this connection.
func (p *Prober) httpClient(t Target) *http.Client {
	dialer := &net.Dialer{Timeout: p.timeout()}
	target := net.JoinHostPort(t.IP, strconv.Itoa(t.Port))

	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, target)
			},
			TLSClientConfig:     &tls.Config{ServerName: t.Host, InsecureSkipVerify: true},
			TLSHandshakeTimeout: p.timeout(),
			DisableKeepAlives:   true,
		},
		// Redirects would leave the address we set out to probe.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// probeResponse adds whether the server replied at all, which decides if the
// announce fallback is worth a second round trip.
type probeResponse struct {
	Result
	answered bool
}

func (p *Prober) request(ctx context.Context, client *http.Client, t Target, u string) probeResponse {
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return probeResponse{Result: Result{State: Unknown, Reason: err.Error()}}
	}
	// The URL already carries host:port, so Go derives the right Host header;
	// overriding it here would drop the port a virtual host may key on.
	req.Header.Set("User-Agent", p.userAgent())

	resp, err := client.Do(req)
	if err != nil {
		return probeResponse{Result: Result{State: Dead, Reason: dialReason(err), RTT: time.Since(start)}}
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	rtt := time.Since(start)
	if bencoded(body) {
		return probeResponse{Result: Result{State: Live, RTT: rtt}, answered: true}
	}
	// Being turned away is not evidence the tracker is gone, and a second
	// request would only add to the load that caused it.
	if throttled(resp.StatusCode) {
		return probeResponse{Result: Result{
			State:  Unknown,
			Reason: fmt.Sprintf("throttled (HTTP %d)", resp.StatusCode),
			RTT:    rtt,
		}}
	}
	return probeResponse{
		Result:   Result{State: Dead, Reason: notTracker(resp.StatusCode, body), RTT: rtt},
		answered: true,
	}
}

// bencoded reports whether the body opens a bencoded dictionary, which is what
// every tracker reply is, success or failure.
func bencoded(body []byte) bool {
	trimmed := strings.TrimLeft(string(body), " \t\r\n")
	return strings.HasPrefix(trimmed, "d")
}

// throttled reports the statuses that mean "come back later" rather than
// "there is nothing here". Common behind CDNs, which see a burst when a name
// with many addresses is probed on all of them.
func throttled(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable
}

func notTracker(status int, body []byte) string {
	if len(body) == 0 {
		return fmt.Sprintf("HTTP %d, empty body", status)
	}
	if head := strings.ToLower(strings.TrimSpace(string(body[:min(len(body), 64)]))); strings.HasPrefix(head, "<") {
		return fmt.Sprintf("HTTP %d, HTML not a tracker reply", status)
	}
	return fmt.Sprintf("HTTP %d, not a bencoded reply", status)
}

// scrapeURL converts an announce path to its scrape counterpart (BEP 48),
// which reports on the tracker without pretending to be a peer.
func scrapeURL(t Target) string {
	p := t.Path
	dir, file := path.Split(p)
	if strings.Contains(file, "announce") {
		p = dir + strings.Replace(file, "announce", "scrape", 1)
	}
	return endpointURL(t, p, "")
}

// announceURL asks for a torrent the tracker cannot know about. The expected
// answer is a bencoded failure, which is all the proof we need.
func announceURL(t Target) string {
	q := url.Values{
		"info_hash":  {strings.Repeat("\x00", 20)},
		"peer_id":    {"-TT0001-" + strings.Repeat("0", 12)},
		"port":       {"6881"},
		"uploaded":   {"0"},
		"downloaded": {"0"},
		"left":       {"0"},
		"compact":    {"1"},
	}
	return endpointURL(t, t.Path, q.Encode())
}

func endpointURL(t Target, p, query string) string {
	u := url.URL{
		Scheme:   t.Scheme,
		Host:     net.JoinHostPort(t.Host, strconv.Itoa(t.Port)),
		Path:     p,
		RawQuery: query,
	}
	return u.String()
}

// dialReason turns a network error into something short enough for a change
// feed entry.
func dialReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded), isTimeout(err):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	// Strip the wrappers that only restate the URL the caller already knows.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Err != nil {
		err = urlErr.Err
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		err = opErr.Err
	}
	return err.Error()
}

func isTimeout(err error) bool {
	var nerr net.Error
	return errors.As(err, &nerr) && nerr.Timeout()
}
