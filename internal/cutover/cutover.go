// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package cutover evaluates how ready a Squadron deployment is to flip each of
// its grace-first enforcement dimensions from grace/shadow to enforce.
//
// Squadron ships four security postures grace-first (accept-with-warning, flip
// to fatal deliberately): API authentication (ADR 0045), OpAMP channel auth
// (ADR 0042), SSRF egress controls (ADR 0046), and strict multi-tenancy
// (ADR 0048, enterprise). Each has its own flag and its own "watch this, then
// flip" playbook, but there was no single command that told an operator where
// they stand across all four. This package is that aggregator: given the loaded
// config, the tenant-commingling store audit, and the environment, it produces a
// per-dimension readiness report with concrete blockers (must fix before
// flipping) and watch-items (runtime signals a static tool can't assert).
//
// It is pure and read-only: no store writes, no network. The CLI
// (cmd/squadron-cutover) wires config + store + env into Evaluate and prints the
// report; exit 0 = every grace dimension is ready (or already enforcing), 2 =
// at least one has a blocker.
package cutover

import (
	"time"

	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

// Dimension names one grace-first enforcement posture.
type Dimension string

const (
	DimensionAuth      Dimension = "authentication"     // ADR 0045 — auth.enabled
	DimensionOpAMP     Dimension = "opamp_channel_auth" // ADR 0042 — opamp.require_auth
	DimensionEgress    Dimension = "egress_controls"    // ADR 0046 — egress.enforce
	DimensionTenanting Dimension = "strict_tenanting"   // ADR 0048 — SQUADRON_STRICT_TENANTING
)

// Mode is the current posture of a dimension.
type Mode string

const (
	ModeEnforcing Mode = "enforcing" // already flipped — nothing to do
	ModeGrace     Mode = "grace"     // accept-with-warning; can be flipped
)

// StrictTenantingEnvVar is the env var (enterprise) that arms strict tenanting.
// Mirrored here so the OSS preflight can read the same knob the enterprise wire
// consumes, without importing the enterprise package.
const StrictTenantingEnvVar = "SQUADRON_STRICT_TENANTING"

// EgressEnforceEnvVar is the env override for egress enforcement (ADR 0046).
const EgressEnforceEnvVar = "SQUADRON_EGRESS_ENFORCE"

// DimensionReadiness is the verdict for one dimension.
type DimensionReadiness struct {
	Dimension Dimension `json:"dimension"`
	ADR       string    `json:"adr"`
	Mode      Mode      `json:"mode"`
	// Ready is true when the dimension is either already enforcing, or in grace
	// with no blockers (the operator may flip it — after satisfying Watch items).
	Ready bool `json:"ready"`
	// Flag is the config/env knob that flips this dimension to enforce.
	Flag string `json:"flag"`
	// Blockers are conditions a STATIC preflight can prove would break on flip.
	// A non-empty Blockers means Ready=false.
	Blockers []string `json:"blockers,omitempty"`
	// Watch are runtime signals a static tool cannot assert; the operator must
	// confirm these (via metrics/logs) before flipping.
	Watch []string `json:"watch,omitempty"`
	// Notes are informational.
	Notes []string `json:"notes,omitempty"`
}

// Report aggregates all dimensions.
type Report struct {
	GeneratedAt time.Time            `json:"generated_at"`
	Dimensions  []DimensionReadiness `json:"dimensions"`
	// AllReady is true when no dimension has a blocker (dimensions already
	// enforcing, or in grace with only watch-items, all count as ready).
	AllReady bool `json:"all_ready"`
}

// truthyEnv reports whether an env value means "on" (mirrors the enterprise
// strict-gate + egress env parsing: 1/true/yes, case-insensitive).
func truthyEnv(v string) bool {
	switch v {
	case "1", "true", "TRUE", "True", "yes", "YES", "Yes", "on", "ON", "On":
		return true
	default:
		return false
	}
}

func isLoopbackHost(h string) bool {
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// Evaluate builds the readiness report. tenantAudit may be nil (e.g. a backend
// that doesn't support the commingling audit) — the tenanting dimension then
// reports that as a blocker-to-verify rather than asserting safety. getenv is
// injected for testability (pass os.Getenv in production).
func Evaluate(cfg *config.Config, tenantAudit *types.TenantComminglingReport, getenv func(string) string) *Report {
	rep := &Report{GeneratedAt: time.Now().UTC()}
	rep.Dimensions = []DimensionReadiness{
		evalAuth(cfg),
		evalOpAMP(cfg),
		evalEgress(cfg, getenv),
		evalTenanting(cfg, tenantAudit, getenv),
	}
	rep.AllReady = true
	for _, d := range rep.Dimensions {
		if !d.Ready {
			rep.AllReady = false
		}
	}
	return rep
}

func evalAuth(cfg *config.Config) DimensionReadiness {
	d := DimensionReadiness{Dimension: DimensionAuth, ADR: "0045", Flag: "auth.enabled"}
	if cfg.Auth.IsEnabled() {
		d.Mode = ModeEnforcing
		d.Ready = true
		d.Notes = append(d.Notes, "API authentication is enabled — enforcing.")
		return d
	}
	d.Mode = ModeGrace
	if !isLoopbackHost(cfg.Server.Host) {
		d.Blockers = append(d.Blockers,
			"auth is DISABLED on a non-loopback bind (server.host="+hostOrAll(cfg.Server.Host)+
				") — this is the warn-window that becomes fatal in a future release; set auth.enabled: true")
	} else {
		d.Notes = append(d.Notes, "auth disabled but bound to loopback ("+cfg.Server.Host+") — supported local-dev posture")
	}
	d.Watch = append(d.Watch,
		"before flipping: confirm every client (UI, squadronctl, integrations) presents a bearer token, then set auth.enabled: true and hand out the printed bootstrap token")
	d.Ready = len(d.Blockers) == 0
	return d
}

func hostOrAll(h string) string {
	if h == "" {
		return "0.0.0.0 (all interfaces)"
	}
	return h
}

func evalOpAMP(cfg *config.Config) DimensionReadiness {
	d := DimensionReadiness{Dimension: DimensionOpAMP, ADR: "0042", Flag: "opamp.require_auth"}
	if cfg.OpAMP.IsAuthRequired() {
		d.Mode = ModeEnforcing
		d.Ready = true
		d.Notes = append(d.Notes, "OpAMP channel auth is required — enforcing; unauthenticated connections are rejected.")
		return d
	}
	d.Mode = ModeGrace
	// A static tool cannot see live connections; the readiness signal is runtime.
	d.Watch = append(d.Watch,
		"mint an opamp:enroll token per agent and add it to each supervisor's server.headers",
		"watch metrics opamp_authenticated_connections_total climb and opamp_unauthenticated_connections_total flatten to 0",
		"only then set opamp.require_auth: true (untenanted/invalid connections are then rejected 401)")
	d.Ready = true // no static blocker; gated on the operator confirming the watch-items
	d.Notes = append(d.Notes, "grace mode — no static blocker; readiness is confirmed from the live connection metrics above")
	return d
}

func evalEgress(cfg *config.Config, getenv func(string) string) DimensionReadiness {
	d := DimensionReadiness{Dimension: DimensionEgress, ADR: "0046", Flag: "egress.enforce (or " + EgressEnforceEnvVar + ")"}
	enforced := cfg.Egress.IsEnforced() || truthyEnv(getenv(EgressEnforceEnvVar))
	if enforced {
		d.Mode = ModeEnforcing
		d.Ready = true
		d.Notes = append(d.Notes, "egress controls are enforcing; private/internal ranges are denied unless allowlisted.")
		if n := len(cfg.Egress.AllowPrivateCIDRs); n > 0 {
			d.Notes = append(d.Notes, "allowlist has "+itoa(n)+" entr"+plural(n, "y", "ies"))
		}
		return d
	}
	d.Mode = ModeGrace
	d.Watch = append(d.Watch,
		"collect the egressguard 'WOULD be blocked' shadow warnings from the logs (each carries host + resolved_ip)",
		"add every legitimate internal destination (SIEM, Tower, webhooks) to egress.allow_private_cidrs",
		"then set egress.enforce: true (or "+EgressEnforceEnvVar+"=true)")
	if n := len(cfg.Egress.AllowPrivateCIDRs); n > 0 {
		d.Notes = append(d.Notes, "allowlist already has "+itoa(n)+" entr"+plural(n, "y", "ies"))
	} else {
		d.Notes = append(d.Notes, "allowlist is empty — build it from the shadow warnings before flipping")
	}
	d.Ready = true // no static blocker; cloud-metadata/loopback already blocked always
	return d
}

func evalTenanting(cfg *config.Config, tenantAudit *types.TenantComminglingReport, getenv func(string) string) DimensionReadiness {
	d := DimensionReadiness{Dimension: DimensionTenanting, ADR: "0048", Flag: StrictTenantingEnvVar}
	if truthyEnv(getenv(StrictTenantingEnvVar)) {
		d.Mode = ModeEnforcing
		d.Ready = true
		d.Notes = append(d.Notes, "strict tenanting is armed — enforcing (enterprise).")
		return d
	}
	d.Mode = ModeGrace

	// Blocker 1: commingled store data (the squadron-audit-tenants check).
	switch {
	case tenantAudit == nil:
		d.Blockers = append(d.Blockers,
			"tenant-commingling audit unavailable for this store backend — run squadron-audit-tenants against sqlite/postgres to confirm no table holds >1 tenant before arming")
	case tenantAudit.AnyCommingled:
		d.Blockers = append(d.Blockers,
			"store holds COMMINGLED data (>1 tenant in a table) — reconcile the flagged tables (see squadron-audit-tenants) before arming, or strict mode will hide rows")
	default:
		d.Notes = append(d.Notes, "no commingled store data (tenant audit clean)")
	}

	// Blocker 2: OTLP ingest tenant must be set or the strict wire log.Fatals.
	if cfg.Ingest.OTLP.TenantID == "" {
		d.Blockers = append(d.Blockers,
			"ingest.otlp.tenant_id is empty — strict tenanting fatals at boot on an unbound OTLP tenant; set it (e.g. 'default') before arming")
	} else {
		d.Notes = append(d.Notes, "ingest.otlp.tenant_id set to '"+cfg.Ingest.OTLP.TenantID+"'")
	}

	// Runtime signals a static tool cannot assert.
	d.Watch = append(d.Watch,
		"confirm every OpAMP agent presents a tenant (x-squadron-tenant header or an opamp:enroll token whose tenant is set)",
		"confirm operator tokens are validated identity sources (oidc:/scim:/bootstrap) — raw bearers are rejected once strict identity-source is armed",
		"then set "+StrictTenantingEnvVar+"=true (enterprise)")

	d.Ready = len(d.Blockers) == 0
	return d
}

// tiny local int/format helpers (avoid strconv import churn in a leaf pkg)
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
