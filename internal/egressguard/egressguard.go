// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package egressguard is the one shared, DNS-rebinding-safe egress guard for
// every server-side fetch of a user/config-supplied URL (ADR 0046).
//
// The guard runs at CONNECT time, on the resolved IP (post-DNS), so it closes
// the redirect/rebinding bypass a naive host allowlist misses. It:
//
//   - BLOCKS ALWAYS (never allowlistable): cloud-metadata IPs
//     (169.254.169.254, the IPv6 IMDS fd00:ec2::254 + equivalents), loopback
//     (127.0.0.0/8, ::1), link-local (169.254.0.0/16, fe80::/10), and
//     unspecified/multicast.
//   - SHADOW-WARNS private/internal ranges by default (RFC1918 10/8, 172.16/12,
//     192.168/16, CGNAT 100.64/10, ULA fc00::/7): it logs what WOULD be blocked
//     without blocking, so operators can capture their real SIEM/deploy/webhook
//     IPs and allowlist them BEFORE enforcement is flipped on (mirrors the
//     0042/0044/0045 warn-window pattern). Flip Mode to ModeEnforce to block.
//   - Supports a per-destination CIDR allowlist so an operator can permit a
//     specific private-range destination once enforcing — metadata/loopback are
//     NEVER allowlistable.
//   - DENIES redirects (CheckRedirect returns an error).
//   - Enforces a scheme allowlist (http/https only).
//
// The process-wide default guard is permissive for loopback until Configure is
// called (so unit tests that hit httptest servers on 127.0.0.1 keep working)
// but ALWAYS blocks cloud-metadata. Production wires Configure at startup from
// squadron.yaml before any server-side fetch runs.
package egressguard

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Mode is the enforcement posture for private/internal ranges (metadata and
// loopback are always blocked regardless of Mode).
type Mode int

const (
	// ModeShadow logs what would be blocked for private/internal ranges but
	// allows the connection. The rollout default.
	ModeShadow Mode = iota
	// ModeEnforce blocks private/internal ranges unless a destination is in the
	// per-destination CIDR allowlist.
	ModeEnforce
)

// Policy is the guard's configuration.
type Policy struct {
	// Mode selects shadow (warn-only) vs enforce for private/internal ranges.
	Mode Mode
	// PrivateAllowlist is the operator-maintained set of private-range CIDRs
	// that are permitted even under ModeEnforce (on-prem SIEM/Tower/webhook).
	// Metadata and loopback are never allowlistable.
	PrivateAllowlist []netip.Prefix
	// AllowInsecureTLS lets a per-destination client skip TLS verification
	// (the Splunk connector's insecure_skip_verify knob). Off by default.
	AllowInsecureTLS bool
	// AllowLoopback permits loopback/link-local/private ranges for tests and
	// for the unconfigured process default (so httptest servers on 127.0.0.1
	// work). It is NEVER set from operator config. Cloud-metadata is blocked
	// even when this is true.
	AllowLoopback bool
}

// Guard enforces the egress policy at dial time.
type Guard struct {
	policy  Policy
	logger  *zap.Logger
	resolve func(ctx context.Context, host string) ([]net.IP, error)
	dial    func(ctx context.Context, network, addr string) (net.Conn, error)
}

// New builds a Guard for an explicit policy. A nil logger is tolerated.
func New(p Policy, logger *zap.Logger) *Guard {
	if logger == nil {
		logger = zap.NewNop()
	}
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &Guard{
		policy:  p,
		logger:  logger,
		resolve: defaultResolve,
		dial:    d.DialContext,
	}
}

// metadataIPs are the cloud-metadata endpoints that are ALWAYS blocked and are
// never allowlistable — nothing legitimate targets them from this control
// plane, and they are the canonical SSRF credential-exfil target.
var metadataIPs = []netip.Addr{
	netip.MustParseAddr("169.254.169.254"), // AWS/GCP/Azure IMDS
	netip.MustParseAddr("fd00:ec2::254"),   // AWS IMDS over IPv6
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

type category int

const (
	catPublic category = iota
	catMetadata
	catLoopback
	catLinkLocal
	catUnspecMulticast
	catPrivate // RFC1918 + ULA + CGNAT
)

func classify(ip net.IP) category {
	if a, ok := toAddr(ip); ok {
		for _, m := range metadataIPs {
			if a == m {
				return catMetadata
			}
		}
	}
	switch {
	case ip.IsLoopback():
		return catLoopback
	case ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast():
		return catLinkLocal
	case ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast():
		return catUnspecMulticast
	case ip.IsPrivate(): // 10/8, 172.16/12, 192.168/16, fc00::/7
		return catPrivate
	}
	if a, ok := toAddr(ip); ok && cgnatPrefix.Contains(a) {
		return catPrivate
	}
	return catPublic
}

func toAddr(ip net.IP) (netip.Addr, bool) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func (g *Guard) inAllowlist(ip net.IP) bool {
	a, ok := toAddr(ip)
	if !ok {
		return false
	}
	for _, p := range g.policy.PrivateAllowlist {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// decision reports whether the resolved IP may be dialed, whether a shadow-mode
// warning should be emitted, and a sanitized reason when blocked.
func (g *Guard) decision(ip net.IP) (allow, warn bool, reason string) {
	switch classify(ip) {
	case catMetadata:
		// Never allowlistable, never permitted — even in test/loopback mode.
		return false, false, "cloud-metadata address blocked by egress policy"
	case catLoopback:
		if g.policy.AllowLoopback {
			return true, false, ""
		}
		return false, false, "loopback address blocked by egress policy"
	case catLinkLocal:
		if g.policy.AllowLoopback {
			return true, false, ""
		}
		return false, false, "link-local address blocked by egress policy"
	case catUnspecMulticast:
		if g.policy.AllowLoopback {
			return true, false, ""
		}
		return false, false, "unspecified/multicast address blocked by egress policy"
	case catPrivate:
		if g.policy.AllowLoopback || g.inAllowlist(ip) {
			return true, false, ""
		}
		if g.policy.Mode == ModeEnforce {
			return false, false, "private/internal address blocked by egress policy"
		}
		return true, true, "" // shadow: allow but warn
	default:
		return true, false, ""
	}
}

func (g *Guard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, ip := range ips {
		allow, warn, reason := g.decision(ip)
		if warn {
			g.logger.Warn("egressguard: destination resolves to a private/internal range — WOULD be blocked once enforcing (shadow mode). Add it to the egress allowlist before flipping enforce.",
				zap.String("host", host),
				zap.String("resolved_ip", ip.String()))
		}
		if !allow {
			g.logger.Warn("egressguard: blocked outbound connection",
				zap.String("host", host),
				zap.String("resolved_ip", ip.String()),
				zap.String("reason", reason))
			lastErr = &BlockedError{Host: host, Reason: reason}
			continue
		}
		conn, derr := g.dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if derr != nil {
			lastErr = derr
			continue
		}
		return conn, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("egressguard: no addresses resolved for host")
	}
	return nil, lastErr
}

// BlockedError is returned when the guard refuses a connection. Its message is
// deliberately generic (no upstream/dial detail) so it can't be used as an SSRF
// oracle.
type BlockedError struct {
	Host   string
	Reason string
}

func (e *BlockedError) Error() string { return "egressguard: " + e.Reason }

// Client returns an *http.Client that enforces this guard's policy: the dial
// guard, denied redirects, and the http/https scheme allowlist.
func (g *Guard) Client(timeout time.Duration) *http.Client {
	base := baseTransport()
	base.DialContext = g.dialContext
	if g.policy.AllowInsecureTLS {
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- per-destination operator opt-in (e.g. Splunk HEC with a self-signed cert); default is verify-on.
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     schemeGuard{base: base},
		CheckRedirect: denyRedirects,
	}
}

// --- process-wide default -------------------------------------------------

var (
	mu  sync.RWMutex
	def = New(Policy{Mode: ModeShadow, AllowLoopback: true}, zap.NewNop())
)

// Configure sets the process-wide default policy. Call once at startup, before
// any server-side fetch. Clients built by NewClient / NewClientInsecure read
// the default at dial time, so configuring after they are constructed still
// takes effect.
func Configure(p Policy, logger *zap.Logger) {
	mu.Lock()
	def = New(p, logger)
	mu.Unlock()
}

// Default returns the current process-wide guard.
func Default() *Guard {
	mu.RLock()
	defer mu.RUnlock()
	return def
}

// NewClient returns a client bound to the process-wide default guard, resolving
// the live policy at dial time.
func NewClient(timeout time.Duration) *http.Client {
	return newDefaultBackedClient(timeout, false)
}

// NewClientInsecure is NewClient plus a per-destination TLS-skip-verify knob
// (the Splunk connector's insecure_skip_verify). The dial guard still applies.
func NewClientInsecure(timeout time.Duration, insecure bool) *http.Client {
	return newDefaultBackedClient(timeout, insecure)
}

func newDefaultBackedClient(timeout time.Duration, insecure bool) *http.Client {
	base := baseTransport()
	base.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return Default().dialContext(ctx, network, addr)
	}
	if insecure {
		base.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // #nosec G402 -- per-destination operator opt-in (insecure_skip_verify); default is verify-on.
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     schemeGuard{base: base},
		CheckRedirect: denyRedirects,
	}
}

// IsGuarded reports whether c routes through the egress guard. Used by tests to
// assert every adopted call path uses a guarded client.
func IsGuarded(c *http.Client) bool {
	if c == nil {
		return false
	}
	_, ok := c.Transport.(schemeGuard)
	return ok
}

// BaseTransport returns the underlying *http.Transport of a guard-built client
// (unwrapping the scheme guard), or nil if c is not guarded. Intended for tests
// that need to inspect transport-level settings such as TLS verification.
func BaseTransport(c *http.Client) *http.Transport {
	if c == nil {
		return nil
	}
	sg, ok := c.Transport.(schemeGuard)
	if !ok {
		return nil
	}
	tr, _ := sg.base.(*http.Transport)
	return tr
}

func baseTransport() *http.Transport {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return &http.Transport{}
}

func denyRedirects(_ *http.Request, _ []*http.Request) error {
	return fmt.Errorf("egressguard: redirects are not permitted")
}

// schemeGuard enforces the http/https scheme allowlist before delegating to the
// dial-guarded transport.
type schemeGuard struct {
	base http.RoundTripper
}

func (s schemeGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
		return s.base.RoundTrip(req)
	default:
		return nil, &BlockedError{Host: req.URL.Host, Reason: "scheme not permitted (only http/https)"}
	}
}

func defaultResolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// SanitizeError maps a fetch/probe error to a coarse, safe category string for
// return through the SIEM /test and deploy /validate endpoints (ADR 0046). It
// never echoes an upstream response body or a raw dial detail, so it can't be
// used as an SSRF probing oracle, while still telling an operator whether the
// failure was auth, routing, DNS, TLS, or policy.
func SanitizeError(err error) string {
	if err == nil {
		return ""
	}
	var be *BlockedError
	if errors.As(err, &be) {
		return "blocked by egress policy"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized"):
		return "authentication failed (401)"
	case strings.Contains(msg, "403") || strings.Contains(msg, "forbidden"):
		return "forbidden — token scope or rate limit (403)"
	case strings.Contains(msg, "404") || strings.Contains(msg, "not found"):
		return "not found (404) — check the destination/repo/workflow"
	case strings.Contains(msg, "422"):
		return "rejected by the destination (422)"
	case strings.Contains(msg, "no such host") || strings.Contains(msg, "dns"):
		return "DNS resolution failed"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return "connection timed out"
	case strings.Contains(msg, "tls") || strings.Contains(msg, "certificate") || strings.Contains(msg, "x509"):
		return "TLS error"
	default:
		return "connection failed"
	}
}

// ParseAllowlist parses a list of CIDR strings (or bare IPs) into prefixes for
// Policy.PrivateAllowlist, skipping blanks. Invalid entries are returned as an
// error naming the offender.
func ParseAllowlist(entries []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, raw := range entries {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "/") {
			a, err := netip.ParseAddr(s)
			if err != nil {
				return nil, fmt.Errorf("egress allowlist: invalid address %q: %w", s, err)
			}
			bits := a.BitLen()
			out = append(out, netip.PrefixFrom(a, bits))
			continue
		}
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, fmt.Errorf("egress allowlist: invalid CIDR %q: %w", s, err)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}
