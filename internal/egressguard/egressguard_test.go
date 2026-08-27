// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package egressguard

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// fakeGuard builds a Guard with a resolver that maps every host to the given
// IPs and a dialer that records whether it was invoked (so we can assert a
// blocked connection never reached the network).
func fakeGuard(t *testing.T, p Policy, ips ...string) (*Guard, *bool, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.WarnLevel)
	g := New(p, zap.New(core))
	parsed := make([]net.IP, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil {
			t.Fatalf("bad test IP %q", s)
		}
		parsed = append(parsed, ip)
	}
	dialed := false
	g.resolve = func(_ context.Context, _ string) ([]net.IP, error) { return parsed, nil }
	g.dial = func(_ context.Context, _, _ string) (net.Conn, error) {
		dialed = true
		return nil, errors.New("dialed (test stub)")
	}
	return g, &dialed, logs
}

func TestDial_MetadataAlwaysBlocked(t *testing.T) {
	// Even with AllowLoopback (test/unconfigured mode), metadata is blocked.
	for _, ip := range []string{"169.254.169.254", "fd00:ec2::254"} {
		g, dialed, _ := fakeGuard(t, Policy{Mode: ModeShadow, AllowLoopback: true}, ip)
		_, err := g.dialContext(context.Background(), "tcp", "evil.example.com:80")
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("ip %s: want BlockedError, got %v", ip, err)
		}
		if *dialed {
			t.Fatalf("ip %s: dialer must not be called when blocked", ip)
		}
		if strings.Contains(be.Error(), ip) {
			t.Fatalf("blocked error must not leak the target IP: %q", be.Error())
		}
	}
}

func TestDial_LoopbackBlockedWhenEnforcedPosture(t *testing.T) {
	for _, ip := range []string{"127.0.0.1", "::1"} {
		g, dialed, _ := fakeGuard(t, Policy{Mode: ModeShadow}, ip) // AllowLoopback false
		_, err := g.dialContext(context.Background(), "tcp", "localhost:80")
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Fatalf("ip %s: want BlockedError, got %v", ip, err)
		}
		if *dialed {
			t.Fatalf("ip %s: dialer must not be called", ip)
		}
	}
}

func TestDial_PrivateShadowWarnsButAllows(t *testing.T) {
	for _, ip := range []string{"10.1.2.3", "192.168.5.5", "172.16.9.9", "100.64.1.1"} {
		g, dialed, logs := fakeGuard(t, Policy{Mode: ModeShadow}, ip)
		_, _ = g.dialContext(context.Background(), "tcp", "internal.example.com:443")
		if !*dialed {
			t.Fatalf("ip %s: shadow mode must ALLOW (dial the private IP)", ip)
		}
		if logs.FilterMessageSnippet("private/internal range").Len() == 0 {
			t.Fatalf("ip %s: shadow mode must emit a would-block WARN", ip)
		}
	}
}

func TestDial_PrivateEnforceBlocksUnlessAllowlisted(t *testing.T) {
	// Enforce: a 10.x is blocked...
	g, dialed, _ := fakeGuard(t, Policy{Mode: ModeEnforce}, "10.1.2.3")
	_, err := g.dialContext(context.Background(), "tcp", "internal.example.com:443")
	if _, ok := err.(*BlockedError); !ok || *dialed {
		t.Fatalf("enforce mode must block a non-allowlisted private IP; err=%v dialed=%v", err, *dialed)
	}
	// ...unless it's in the allowlist.
	allow, err := ParseAllowlist([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatal(err)
	}
	g2, dialed2, _ := fakeGuard(t, Policy{Mode: ModeEnforce, PrivateAllowlist: allow}, "10.1.2.3")
	_, _ = g2.dialContext(context.Background(), "tcp", "internal.example.com:443")
	if !*dialed2 {
		t.Fatalf("an allowlisted private IP must be dialed under enforce")
	}
	// ...and metadata stays blocked EVEN WITH allow_private + a broad allowlist.
	broad, _ := ParseAllowlist([]string{"0.0.0.0/0"})
	g3, dialed3, _ := fakeGuard(t, Policy{Mode: ModeEnforce, AllowLoopback: true, PrivateAllowlist: broad}, "169.254.169.254")
	_, err = g3.dialContext(context.Background(), "tcp", "evil:80")
	if _, ok := err.(*BlockedError); !ok || *dialed3 {
		t.Fatalf("metadata must be blocked even with a 0.0.0.0/0 allowlist; err=%v", err)
	}
}

func TestDial_PublicAllowed(t *testing.T) {
	g, dialed, _ := fakeGuard(t, Policy{Mode: ModeEnforce}, "93.184.216.34")
	_, _ = g.dialContext(context.Background(), "tcp", "example.com:443")
	if !*dialed {
		t.Fatalf("a public IP must be dialed")
	}
}

func TestClient_RejectsNonHTTPScheme(t *testing.T) {
	c := New(Policy{AllowLoopback: true}, zap.NewNop()).Client(2 * time.Second)
	req, _ := http.NewRequest(http.MethodGet, "file:///etc/passwd", nil)
	_, err := c.Transport.RoundTrip(req)
	var be *BlockedError
	if !errors.As(err, &be) {
		t.Fatalf("want scheme BlockedError, got %v", err)
	}
}

func TestClient_DeniesRedirects(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()
	c := New(Policy{AllowLoopback: true}, zap.NewNop()).Client(2 * time.Second)
	_, err := c.Get(srv.URL)
	if err == nil {
		t.Fatalf("a redirect to a blocked host must fail")
	}
	if !strings.Contains(err.Error(), "redirects are not permitted") {
		t.Fatalf("want redirect-denied error, got %v", err)
	}
}

func TestClient_PublicLoopbackRoundTrips(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// AllowLoopback lets the guard reach the httptest server (127.0.0.1).
	c := New(Policy{AllowLoopback: true}, zap.NewNop()).Client(2 * time.Second)
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatalf("guarded client should reach an allowed host: %v", err)
	}
	_ = resp.Body.Close()
	if !IsGuarded(c) {
		t.Fatalf("IsGuarded should report true for a guard-built client")
	}
}

func TestIsGuarded_PlainClientFalse(t *testing.T) {
	if IsGuarded(&http.Client{}) {
		t.Fatalf("a plain client is not guarded")
	}
}

func TestParseAllowlist(t *testing.T) {
	got, err := ParseAllowlist([]string{" 10.0.0.0/8 ", "", "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 prefixes, got %d", len(got))
	}
	if _, err := ParseAllowlist([]string{"not-a-cidr"}); err == nil {
		t.Fatalf("want error for bad CIDR")
	}
}
