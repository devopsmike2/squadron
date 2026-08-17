// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package opamp

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/open-telemetry/opamp-go/protobufs"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/confignorm"
	"github.com/devopsmike2/squadron/internal/services"
)

// adoptOnSuperviseEnabled gates adopt-on-first-supervise (ADR 0039). Default
// true: when a newly-supervised agent that advertises accepts_remote_config has
// NO assigned managed config but IS reporting a non-empty effective config,
// Squadron seeds its initial managed config FROM that reported effective config
// instead of pushing the synthesized DefaultOTelConfig skeleton. The skeleton
// (otlp -> batch -> otlp(localhost:4317)) deep-merges over the agent's local base
// config in the supervisor and REPLACES its wired service.pipelines, so a
// brownfield collector's filelog/hostmetrics/otlphttp pipelines regress to
// defined-but-unreferenced — the Southern-pilot regression documented in
// knowledge/2026-08-12-supervisor-initial-config-skeleton-clobber.md. Flip to
// false via SetAdoptOnSupervise to restore the pre-0039 skeleton-always default.
var adoptOnSuperviseEnabled = true

// SetAdoptOnSupervise toggles adopt-on-first-supervise (ADR 0039). Default
// enabled; a wire may call this at startup to disable it and restore the
// skeleton-always initial config. The guard conditions (accepts_remote_config +
// no assigned config + non-empty reported effective config) already scope the
// behavior tightly, so this is an escape hatch, not the primary gate. Not safe
// for concurrent use with a running server; call before Start.
func SetAdoptOnSupervise(v bool) { adoptOnSuperviseEnabled = v }

// auditRecorder is the optional audit seam used to record the
// agent.config_adopted_on_supervise event when adopt-on-first-supervise seeds a
// config (ADR 0039). Satisfied by services.AuditService. nil (the default, and
// every test harness that does not set it) makes the audit write a no-op, so
// pre-0039 behavior — and every existing test — is unchanged. Wired at startup
// via Server.SetAuditRecorder.
type auditRecorder interface {
	Record(ctx context.Context, entry services.AuditEntry) error
}

// SetAuditRecorder wires the optional audit seam (ADR 0039). Called once at
// startup (main.go) with the audit service. Until called, adopt-on-supervise
// still seeds the config but records no audit event. Not safe for concurrent use
// with a running server; call before Start.
func (s *Server) SetAuditRecorder(a auditRecorder) { s.audit = a }

// tryAdoptEffectiveConfig implements adopt-on-first-supervise (ADR 0039). The
// connect path calls it from getConfigForAgent ONLY after resolveStoredConfig
// found no managed config assigned to the agent (neither agent- nor
// group-scoped). When the agent advertises accepts_remote_config AND is reporting
// a non-empty effective config, it seeds a managed Config FROM that reported
// effective config — stripping the direct opamp extension, preserving ${ENV}
// references, validating — assigns it to the agent, records an audit event, and
// returns (content, true) so the caller pushes it instead of the skeleton.
//
// On any miss — feature disabled, no accepts_remote_config, empty or invalid
// reported effective config, or a store error — it returns ("", false) and the
// caller falls back to the DefaultOTelConfig skeleton. Invalid config is never
// captured or pushed: a fresh agent still gets the skeleton, garbage never does.
//
// Idempotent: it re-checks GetLatestConfigForAgent and returns the already
// assigned config (without re-creating) if one exists, so repeated resolutions
// (reconnects, the HA reconcile loop, a second connect-path pass) can never spawn
// a duplicate config. It runs on the OpAMP connection goroutine, the sole writer
// of agent.EffectiveConfig, so the snapshot read is race-free — but it reads
// through EffectiveConfigSnapshot's read lock defensively, mirroring the other
// external accessors.
func (s *Server) tryAdoptEffectiveConfig(ctx context.Context, agent *Agent) (string, bool) {
	if !adoptOnSuperviseEnabled || s.agentService == nil || agent == nil {
		return "", false
	}

	// Guard: only agents that advertise remote-config support are eligible. The
	// connect path's call site already holds this, but the check keeps the adopt
	// self-contained — a report-only agent never adopts (and never gets a
	// skeleton pushed either, since the caller gates the whole config push on the
	// same capability).
	if !agent.HasCapability(protobufs.AgentCapabilities_AgentCapabilities_AcceptsRemoteConfig) {
		return "", false
	}

	effective := agent.EffectiveConfigSnapshot()
	if strings.TrimSpace(effective) == "" {
		// Fresh agent with nothing to adopt — caller uses the skeleton.
		return "", false
	}

	agentID := agent.storeID()

	// Idempotency guard: never re-adopt if a config is already assigned. In the
	// normal flow resolveStoredConfig already ruled this out upstream; the
	// re-check makes create+assign exactly-once even under a reconnect/reconcile
	// race.
	if existing, err := s.agentService.GetLatestConfigForAgent(ctx, agentID); err == nil && existing != nil {
		return existing.Content, true
	}

	// Strip the direct opamp extension. The supervisor injects its OWN local
	// opamp extension to talk to Squadron; a direct-to-Squadron opamp extension
	// carried over from the brownfield config would open a conflicting second
	// control connection. Everything else — all pipelines, receivers, processors,
	// exporters, oauth2client, health_check, zpages, and service.telemetry
	// (self-telemetry) — is kept intact. ${ENV} references round-trip untouched
	// (they are plain YAML scalars), so no redacted secret literal is baked in.
	stripped, err := stripOpampExtension(effective)
	if err != nil {
		s.logger.Warn("adopt-on-supervise: could not parse reported effective config to strip opamp extension; falling back to skeleton",
			zap.String("agentId", agent.InstanceIdStr), zap.Error(err))
		return "", false
	}

	// Validate structurally before storing — reuse the same YAML check the
	// adopt-config endpoint (PR #24) uses so an unparseable config is rejected
	// here rather than captured as a template that could later brick an agent.
	if err := validateAdoptedConfig(stripped); err != nil {
		s.logger.Warn("adopt-on-supervise: reported effective config is not valid YAML; falling back to skeleton",
			zap.String("agentId", agent.InstanceIdStr), zap.Error(err))
		return "", false
	}

	// Skeleton-persist guard (deferred #39 follow-up,
	// knowledge/2026-08-12-supervisor-initial-config-skeleton-clobber.md).
	// Do NOT capture a pipelines-bare / bootstrap-only effective config as a
	// managed intent for a genuinely-unconfigured agent. Two shapes are refused:
	//   1. no meaningfully-wired pipeline — components may be DEFINED but no
	//      service::pipelines entry references a non-"nop" receiver AND a
	//      non-"nop" exporter (an empty/absent service.pipelines, an unwired
	//      config, or the supervisor's nop bootstrap fragment); and
	//   2. the exact synthesized skeleton (DefaultOTelConfig, otlp -> otlp) that
	//      Squadron itself would otherwise push as the fallback.
	// Persisting either would seed a broken intent that then SHADOWS a real
	// agent- or group-config assigned later (agent-scoped precedence), the exact
	// clobber family this file's ADR 0039 work fixes. When adopt can't safely
	// capture a real config we return ("", false): getConfigForAgent falls
	// through to the transient DefaultOTelConfig push but persists NO intent, so
	// the agent stays unconfigured and correctly resolves to any later
	// agent/group config instead of a seeded skeleton. The guard is deliberately
	// conservative — a legitimate real config always has at least one wired
	// pipeline, so it is never blocked (ADR 0039 stays intact).
	if !isAdoptableConfig(stripped) {
		s.logger.Info("adopt-on-supervise: reported effective config has no wired pipelines (skeleton/bootstrap-only); leaving agent unconfigured rather than seeding a skeleton intent",
			zap.String("agentId", agent.InstanceIdStr))
		return "", false
	}

	cfg := &services.Config{
		ID:         uuid.New().String(),
		Name:       "auto-seeded on supervise",
		AgentID:    &agentID,
		ConfigHash: confignorm.Hash(stripped),
		Content:    stripped,
		Version:    1,
		CreatedAt:  time.Now(),
	}
	if err := s.agentService.CreateConfig(ctx, cfg); err != nil {
		s.logger.Error("adopt-on-supervise: failed to create seeded config; falling back to skeleton",
			zap.String("agentId", agent.InstanceIdStr), zap.Error(err))
		return "", false
	}

	s.logger.Info("adopt-on-supervise: seeded initial config from reported effective config",
		zap.String("agentId", agent.InstanceIdStr),
		zap.String("configId", cfg.ID),
		zap.String("note", "auto-seeded from reported effective config; direct opamp extension stripped"))

	if s.audit != nil {
		_ = s.audit.Record(ctx, services.AuditEntry{
			Actor:      services.AuditActorOpAMP,
			EventType:  services.AuditEventAgentConfigAdopted,
			TargetType: services.AuditTargetConfig,
			TargetID:   cfg.ID,
			Action:     "adopted",
			Payload: map[string]any{
				"agent_id":    agentID.String(),
				"config_hash": cfg.ConfigHash,
				"note":        "auto-seeded from the agent's reported effective config on first supervise; direct opamp extension stripped",
			},
		})
	}

	return stripped, true
}

// validateAdoptedConfig validates that content is parseable YAML. It mirrors the
// adopt-config endpoint's validateYAMLConfig (internal/api/handlers) — kept as a
// separate copy here to avoid an opamp -> api/handlers import cycle. An
// unparseable config returns an error so the caller falls back to the skeleton
// rather than pushing garbage.
func validateAdoptedConfig(content string) error {
	var m map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &m); err != nil {
		return fmt.Errorf("invalid YAML syntax: %w", err)
	}
	return nil
}

// stripOpampExtension removes the direct OpAMP extension from an OTel collector
// config: it deletes the `opamp` (and any `opamp/<name>`) key from the top-level
// `extensions` mapping AND removes the same id(s) from the `service.extensions`
// sequence. Everything else is preserved. It edits the parsed yaml.Node tree so
// interior structure and ${ENV} scalars round-trip; only whitespace/indentation
// may be re-emitted canonically (semantically inert for the collector and for
// confignorm hashing). A config with no opamp extension is returned effectively
// unchanged. A YAML parse failure is surfaced as an error (the caller then falls
// back to the skeleton).
func stripOpampExtension(content string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return "", err
	}
	if len(doc.Content) == 0 {
		return content, nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return content, nil
	}

	// 1) Remove opamp key(s) from the top-level `extensions` mapping.
	if ext := mappingValueNode(root, "extensions"); ext != nil && ext.Kind == yaml.MappingNode {
		kept := make([]*yaml.Node, 0, len(ext.Content))
		for i := 0; i+1 < len(ext.Content); i += 2 {
			if isOpampExtensionName(ext.Content[i].Value) {
				continue
			}
			kept = append(kept, ext.Content[i], ext.Content[i+1])
		}
		ext.Content = kept
	}

	// 2) Remove opamp id(s) from the `service.extensions` sequence.
	if svc := mappingValueNode(root, "service"); svc != nil && svc.Kind == yaml.MappingNode {
		if se := mappingValueNode(svc, "extensions"); se != nil && se.Kind == yaml.SequenceNode {
			kept := make([]*yaml.Node, 0, len(se.Content))
			for _, item := range se.Content {
				if isOpampExtensionName(item.Value) {
					continue
				}
				kept = append(kept, item)
			}
			se.Content = kept
		}
	}

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// isOpampExtensionName reports whether an extension id refers to the direct OpAMP
// extension: the bare `opamp` or a named `opamp/<name>` instance. Other
// extensions (health_check, zpages, oauth2client, ...) are never matched.
func isOpampExtensionName(name string) bool {
	return name == "opamp" || strings.HasPrefix(name, "opamp/")
}

// isAdoptableConfig reports whether an OTel collector config is a real, wired
// config that is safe to persist as an adopted managed intent (ADR 0039 skeleton
// guard). It refuses two shapes that must never be captured:
//
//  1. a config with no meaningfully-wired pipeline — see hasMeaningfulPipeline;
//     covers an empty/absent service.pipelines, components defined but referenced
//     in no pipeline, and the supervisor's nop bootstrap fragment; and
//  2. the exact synthesized DefaultOTelConfig skeleton (otlp -> batch ->
//     otlp(localhost:4317)) Squadron would otherwise push as the transient
//     fallback — it has a technically-wired otlp pipeline so (1) passes it, but
//     persisting the skeleton as an intent is precisely the bootstrap-only case
//     the guard exists to block. Compared by the canonical confignorm hash so
//     incidental whitespace differences don't defeat the check.
//
// It is conservative by construction: any legitimate real config has at least
// one wired non-nop pipeline and is not the skeleton, so it is always adoptable.
func isAdoptableConfig(content string) bool {
	if !hasMeaningfulPipeline(content) {
		return false
	}
	// Compare against the skeleton run through the SAME opamp-strip + YAML
	// re-emit as `content`, so the check is indentation/formatting independent
	// (confignorm normalizes values but not structural whitespace, and
	// stripOpampExtension re-emits the tree canonically). A reported effective
	// config that reduces to exactly the synthesized skeleton is refused.
	if ref, err := stripOpampExtension(DefaultOTelConfig); err == nil {
		if confignorm.Hash(content) == confignorm.Hash(ref) {
			return false
		}
	}
	return true
}

// hasMeaningfulPipeline reports whether content declares at least one
// service::pipelines entry that references a non-"nop" receiver AND a non-"nop"
// exporter — i.e. a pipeline that actually moves telemetry. An absent or empty
// service.pipelines, pipelines with empty receiver/exporter lists (components
// defined but unwired), and nop-only bootstrap pipelines all return false. A
// YAML parse failure returns false (fail closed: don't adopt something we can't
// understand). Processors are intentionally ignored — a pipeline with receivers
// and exporters but no processors is still meaningfully wired.
func hasMeaningfulPipeline(content string) bool {
	var doc struct {
		Service struct {
			Pipelines map[string]struct {
				Receivers []string `yaml:"receivers"`
				Exporters []string `yaml:"exporters"`
			} `yaml:"pipelines"`
		} `yaml:"service"`
	}
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return false
	}
	for _, p := range doc.Service.Pipelines {
		if hasNonNopComponent(p.Receivers) && hasNonNopComponent(p.Exporters) {
			return true
		}
	}
	return false
}

// hasNonNopComponent reports whether ids contains at least one component id that
// is neither empty nor the "nop" receiver/exporter (bare "nop" or a named
// "nop/<name>" instance). The nop component is the supervisor's bootstrap
// placeholder and carries no real telemetry, so a pipeline wired only to nop is
// not "meaningful".
func hasNonNopComponent(ids []string) bool {
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || id == "nop" || strings.HasPrefix(id, "nop/") {
			continue
		}
		return true
	}
	return false
}

// mappingValueNode returns the value node for key in a YAML mapping node, or nil
// if the node is not a mapping or the key is absent.
func mappingValueNode(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}
