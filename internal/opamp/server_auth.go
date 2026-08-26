// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/extension/identity"
	"github.com/devopsmike2/squadron/internal/services"
)

// opampEnrollScope is the scope an OpAMP enrollment/agent bearer token must
// carry to authenticate a control-channel connection (ADR 0042). Mirrors
// services.ScopeOpAMPEnroll; duplicated as a package const so the hot connect
// path doesn't reach across packages for a string literal.
const opampEnrollScope = services.ScopeOpAMPEnroll

// connAuth is the per-connection authentication outcome captured at connect time
// (the only point the raw *http.Request — hence the Authorization header — is in
// scope). It is captured in the per-connection callback closures the same way
// the connection tenant already is, so onMessage/onDisconnect can stamp the
// resolved tenant without re-deriving it.
//
// tenant is the tenant every store write on this connection lands in. When
// authenticated it is the TOKEN's tenant (the spoofable x-squadron-tenant header
// and AgentDescription are ignored for authorization — ADR 0042 supersedes ADR
// 0012 §Decision 2's "trust the header" premise). When unauthenticated (grace),
// it falls back to the legacy header-derived tenant so OSS single-tenant and the
// pre-0042 multi-tenant-header behavior are preserved during the grace window.
type connAuth struct {
	tenant        string
	authenticated bool
}

// SetOpAMPAuth wires the ADR 0042 OpAMP channel-authentication seam. auth is the
// same AuthService the REST bearer middleware uses (hashed-token store); reusing
// it means an enrollment token is an ordinary API token carrying the opamp:enroll
// scope, minted + revoked through the existing token infrastructure. requireAuth
// is the enforcement flag: false (the default, and every test harness that never
// calls this) keeps GRACE mode — unauthenticated connections are accepted with a
// loud warning; true REJECTS any connection that does not present a valid
// opamp:enroll token. Call once at startup before Start; not safe for concurrent
// use with a running server.
func (s *Server) SetOpAMPAuth(auth services.AuthService, requireAuth bool) {
	s.auth = auth
	s.requireAuth = requireAuth
}

// SetOpAMPLimits wires the ADR 0042 DoS caps: maxMessageBytes bounds a single
// OpAMP message (<=0 disables the cap) and maxMessagesPerSecond is the per-
// connection token-bucket rate (<=0 disables rate limiting). Call once at startup
// before Start. Unset (the default, and every test harness) leaves both at zero,
// i.e. no cap — preserving pre-0042 behavior for existing tests.
func (s *Server) SetOpAMPLimits(maxMessageBytes int, maxMessagesPerSecond float64) {
	s.maxMessageBytes = maxMessageBytes
	s.maxMessagesPerSecond = maxMessagesPerSecond
}

// authenticateConn resolves the per-connection auth outcome at connect time.
// Returns (auth, accept). accept=false means the connection must be refused
// (401): it happens only under enforcement (requireAuth) when no valid
// opamp:enroll token is presented. Under grace (the default) it always accepts,
// deriving the tenant from a valid token when one is presented and otherwise
// falling back to the legacy header tenant with a loud, future-fatal warning.
//
// SECURITY (ADR 0042): when a valid token is presented, the tenant is taken from
// the TOKEN, never from the x-squadron-tenant header — a hostile agent can no
// longer stamp a victim tenant. A conflicting header is ignored and logged.
func (s *Server) authenticateConn(ctx context.Context, request *http.Request) (connAuth, bool) {
	raw := ""
	if request != nil {
		raw = request.Header.Get("Authorization")
	}
	plaintext := extractBearer(raw)

	// A credential was presented — validate it (independent of grace/enforce).
	if plaintext != "" && s.auth != nil {
		tok, err := s.auth.Validate(ctx, plaintext)
		if err == nil && tok != nil && tok.HasScope(opampEnrollScope) {
			tenant := tok.TenantID
			if tenant == "" {
				tenant = identity.DefaultTenant
			}
			// Demote the x-squadron-tenant header from an authorization input to a
			// display hint: if it disagrees with the token's tenant, the token wins
			// and we log the attempt (ADR 0042). This is the exact exploit the ADR
			// closes — an authenticated agent trying to write into another tenant.
			if hdr := rawConnTenant(request); hdr != "" && hdr != tenant {
				s.logger.Warn("ignoring x-squadron-tenant header that conflicts with the authenticated token's tenant (ADR 0042: tenant is bound to the credential, not the header)",
					zap.String("header_tenant", hdr),
					zap.String("token_tenant", tenant),
					zap.String("token_id", tok.ID))
			}
			if s.metrics != nil {
				s.metrics.AuthenticatedConnectionsTotal.Inc(1)
			}
			return connAuth{tenant: tenant, authenticated: true}, true
		}
		// A token was presented but it is invalid / expired / revoked / lacks the
		// opamp:enroll scope. Under enforcement this is a hard reject; under grace
		// we treat it as unauthenticated (a misconfigured header must not break an
		// agent during the rollout window) but warn loudly.
		if s.requireAuth {
			if s.metrics != nil {
				s.metrics.RejectedConnectionsTotal.Inc(1)
			}
			return connAuth{}, false
		}
		s.logger.Warn("SECURITY: OpAMP connection presented an INVALID or insufficiently-scoped token; accepting as UNAUTHENTICATED under grace mode. This will be REJECTED once opamp.require_auth=true. Fix the supervisor's server.headers Authorization token.")
		if s.metrics != nil {
			s.metrics.UnauthenticatedConnectionsTotal.Inc(1)
		}
		return connAuth{tenant: resolveConnTenant(request), authenticated: false}, true
	}

	// No credential presented (or auth not wired).
	if s.requireAuth {
		if s.metrics != nil {
			s.metrics.RejectedConnectionsTotal.Inc(1)
		}
		return connAuth{}, false
	}
	// Grace: accept, but make the risk impossible to miss (mirrors the ADR 0045
	// warn-window for REST auth). The tenant falls back to the legacy header.
	s.logger.Warn("SECURITY: accepting UNAUTHENTICATED OpAMP connection (grace mode). The OpAMP control channel is currently OPEN: any network peer that can reach this port can register agents, poison the fleet view, and receive config. This is a WARN-ONLY window — set opamp.require_auth=true after enrolling every agent's token via the supervisor server.headers. See ADR 0042.")
	if s.metrics != nil {
		s.metrics.UnauthenticatedConnectionsTotal.Inc(1)
	}
	return connAuth{tenant: resolveConnTenant(request), authenticated: false}, true
}

// extractBearer pulls the token out of an "Authorization: Bearer <token>" header
// value, case-insensitive on the scheme (RFC 7235 §2.1) and tolerant of
// surrounding whitespace. Returns "" when the header is absent or malformed. It
// mirrors the REST middleware's parsing so an operator can reuse the exact same
// token string in the supervisor's server.headers.
func extractBearer(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	parts := strings.SplitN(raw, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

// opampBodyLimitMiddleware returns an http.Handler wrapper that caps the request
// body at s.maxMessageBytes for the plain-HTTP OpAMP transport (ADR 0042 DoS).
// Returns nil when the cap is disabled (maxMessageBytes <= 0), which opamp-go
// treats as "no middleware" — so existing tests and the uncapped default are
// unchanged.
func (s *Server) opampBodyLimitMiddleware() func(http.Handler) http.Handler {
	max := s.maxMessageBytes
	if max <= 0 {
		return nil
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, int64(max))
			next.ServeHTTP(w, r)
		})
	}
}

// connRateLimiter is a minimal per-connection token-bucket rate limiter (ADR
// 0042 DoS hardening). A nil limiter allows everything, so callers can leave it
// unset to disable rate limiting. It is safe for concurrent use, though in
// practice each connection's messages are processed on a single goroutine.
type connRateLimiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time
}

// newConnRateLimiter returns a limiter permitting `rate` messages/second with a
// burst of 2×rate (minimum 1). A rate <= 0 returns nil (rate limiting disabled).
func newConnRateLimiter(rate float64) *connRateLimiter {
	if rate <= 0 {
		return nil
	}
	burst := rate * 2
	if burst < 1 {
		burst = 1
	}
	return &connRateLimiter{tokens: burst, burst: burst, rate: rate, last: time.Now()}
}

// allow reports whether a message may be processed now, consuming one token.
// A nil limiter always allows.
func (r *connRateLimiter) allow() bool {
	if r == nil {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	elapsed := now.Sub(r.last).Seconds()
	r.last = now
	r.tokens += elapsed * r.rate
	if r.tokens > r.burst {
		r.tokens = r.burst
	}
	if r.tokens < 1 {
		return false
	}
	r.tokens--
	return true
}
