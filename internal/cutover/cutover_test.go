// Copyright (c) 2026 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package cutover

import (
	"testing"

	"github.com/devopsmike2/squadron/internal/config"
	"github.com/devopsmike2/squadron/internal/storage/applicationstore/types"
)

func boolp(b bool) *bool { return &b }

// noEnv is a getenv that returns nothing.
func noEnv(string) string { return "" }

// envMap builds a getenv from a map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func dim(rep *Report, d Dimension) DimensionReadiness {
	for _, x := range rep.Dimensions {
		if x.Dimension == d {
			return x
		}
	}
	return DimensionReadiness{}
}

func TestEvaluate_Auth(t *testing.T) {
	cases := []struct {
		name      string
		enabled   *bool
		host      string
		wantMode  Mode
		wantReady bool
	}{
		{"enabled=enforcing", boolp(true), "0.0.0.0", ModeEnforcing, true},
		{"disabled+loopback=grace-ready", boolp(false), "127.0.0.1", ModeGrace, true},
		{"disabled+exposed=grace-blocked", boolp(false), "0.0.0.0", ModeGrace, false},
		{"disabled+empty-host=grace-blocked", boolp(false), "", ModeGrace, false},
		{"omitted=secure-default-enforcing", nil, "0.0.0.0", ModeEnforcing, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Auth.Enabled = tc.enabled
			cfg.Server.Host = tc.host
			cfg.Ingest.OTLP.TenantID = "default" // keep tenanting from adding noise
			rep := Evaluate(cfg, cleanAudit(), noEnv)
			a := dim(rep, DimensionAuth)
			if a.Mode != tc.wantMode {
				t.Errorf("mode = %q, want %q", a.Mode, tc.wantMode)
			}
			if a.Ready != tc.wantReady {
				t.Errorf("ready = %v, want %v (blockers=%v)", a.Ready, tc.wantReady, a.Blockers)
			}
		})
	}
}

func TestEvaluate_OpAMP(t *testing.T) {
	cfg := &config.Config{}
	cfg.Ingest.OTLP.TenantID = "default"

	cfg.OpAMP.RequireAuth = boolp(true)
	if o := dim(Evaluate(cfg, cleanAudit(), noEnv), DimensionOpAMP); o.Mode != ModeEnforcing || !o.Ready {
		t.Errorf("require_auth=true: mode=%q ready=%v, want enforcing/true", o.Mode, o.Ready)
	}

	cfg.OpAMP.RequireAuth = boolp(false)
	o := dim(Evaluate(cfg, cleanAudit(), noEnv), DimensionOpAMP)
	if o.Mode != ModeGrace || !o.Ready {
		t.Errorf("require_auth=false: mode=%q ready=%v, want grace/true (watch-gated)", o.Mode, o.Ready)
	}
	if len(o.Watch) == 0 {
		t.Errorf("grace opamp should carry watch items")
	}
}

func TestEvaluate_Egress(t *testing.T) {
	base := func() *config.Config {
		c := &config.Config{}
		c.Ingest.OTLP.TenantID = "default"
		return c
	}

	// config enforce
	c := base()
	c.Egress.Enforce = boolp(true)
	if e := dim(Evaluate(c, cleanAudit(), noEnv), DimensionEgress); e.Mode != ModeEnforcing {
		t.Errorf("egress config enforce: mode=%q want enforcing", e.Mode)
	}

	// env enforce
	c = base()
	if e := dim(Evaluate(c, cleanAudit(), envMap(map[string]string{EgressEnforceEnvVar: "true"})), DimensionEgress); e.Mode != ModeEnforcing {
		t.Errorf("egress env enforce: mode=%q want enforcing", e.Mode)
	}

	// shadow (default)
	c = base()
	e := dim(Evaluate(c, cleanAudit(), noEnv), DimensionEgress)
	if e.Mode != ModeGrace || !e.Ready {
		t.Errorf("egress shadow: mode=%q ready=%v want grace/true", e.Mode, e.Ready)
	}
	if len(e.Watch) == 0 {
		t.Errorf("shadow egress should carry watch items for building the allowlist")
	}
}

func TestEvaluate_Tenanting(t *testing.T) {
	base := func() *config.Config {
		c := &config.Config{}
		c.Ingest.OTLP.TenantID = "default"
		return c
	}

	// armed via env
	if tn := dim(Evaluate(base(), cleanAudit(), envMap(map[string]string{StrictTenantingEnvVar: "1"})), DimensionTenanting); tn.Mode != ModeEnforcing || !tn.Ready {
		t.Errorf("strict armed: mode=%q ready=%v want enforcing/true", tn.Mode, tn.Ready)
	}

	// grace + clean + otlp set => ready
	if tn := dim(Evaluate(base(), cleanAudit(), noEnv), DimensionTenanting); tn.Mode != ModeGrace || !tn.Ready {
		t.Errorf("grace clean: ready=%v blockers=%v want ready", tn.Ready, tn.Blockers)
	}

	// commingled => blocked
	if tn := dim(Evaluate(base(), &types.TenantComminglingReport{AnyCommingled: true}, noEnv), DimensionTenanting); tn.Ready {
		t.Errorf("commingled: want not ready")
	}

	// nil audit => blocked (unverifiable)
	if tn := dim(Evaluate(base(), nil, noEnv), DimensionTenanting); tn.Ready {
		t.Errorf("nil audit: want not ready")
	}

	// empty OTLP tenant => blocked
	c := &config.Config{}
	c.Ingest.OTLP.TenantID = ""
	if tn := dim(Evaluate(c, cleanAudit(), noEnv), DimensionTenanting); tn.Ready {
		t.Errorf("empty otlp tenant: want not ready")
	}
}

func TestEvaluate_AllReady(t *testing.T) {
	// A clean, safe-to-flip-everything config.
	c := &config.Config{}
	c.Auth.Enabled = boolp(true)
	c.OpAMP.RequireAuth = boolp(false) // grace but watch-gated => ready
	c.Ingest.OTLP.TenantID = "default"
	rep := Evaluate(c, cleanAudit(), noEnv)
	if !rep.AllReady {
		var bad []string
		for _, d := range rep.Dimensions {
			if !d.Ready {
				bad = append(bad, string(d.Dimension))
			}
		}
		t.Errorf("AllReady=false, not-ready dims: %v", bad)
	}

	// One blocker (exposed auth-off) flips AllReady false.
	c.Auth.Enabled = boolp(false)
	c.Server.Host = "0.0.0.0"
	if Evaluate(c, cleanAudit(), noEnv).AllReady {
		t.Errorf("AllReady should be false with an exposed auth-off blocker")
	}
}

func cleanAudit() *types.TenantComminglingReport {
	return &types.TenantComminglingReport{AnyCommingled: false}
}
