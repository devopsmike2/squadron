// Package agentid derives Squadron's stable, UUID-shaped agent identity from a
// set of resource/description attributes. It is the single source of truth for
// "what agent id does this host map to", shared by the OTLP ingest path
// (internal/otlp/parser) and the OpAMP path (internal/opamp) so that a host which
// is both shipping telemetry AND OpAMP-managed resolves to ONE agent id.
package agentid

import "github.com/google/uuid"

// squadronAgentNamespace is a fixed UUIDv5 namespace used to derive a stable
// agent identity when the source lacks a UUID service.instance.id. It is an
// arbitrary but constant UUID — DO NOT change it, or previously derived agent
// IDs would shift and existing agents would re-register as new ones.
var squadronAgentNamespace = uuid.MustParse("7c0f5d2e-6b1a-4f3c-9e2d-1a2b3c4d5e6f")

// stableUUID derives a deterministic UUIDv5 from a seed string. The same seed
// always yields the same UUID, so an agent that restarts keeps its identity.
func stableUUID(seed string) string {
	return uuid.NewSHA1(squadronAgentNamespace, []byte(seed)).String()
}

// Derive resolves a stable, UUID-shaped agent identity from resource/description
// attributes. Agent identity is keyed off a UUID everywhere downstream
// (telemetry storage, enrichment, passive OTLP discovery, and OpAMP fleet
// registration). To cover the many real-world collectors that omit
// service.instance.id or set a non-UUID value, we synthesize a deterministic
// UUID from the best available identifier:
//
//  1. A UUID service.instance.id is used verbatim (the OTel-compliant case).
//  2. A non-UUID service.instance.id is hashed into a stable UUID.
//  3. Otherwise host.name (then service.name) is hashed — this is what lets a
//     plain hostmetrics collector register.
//  4. Only input with no identifying attributes at all falls back to the legacy
//     "default" sentinel (treated as unidentifiable by the enricher and
//     discovery).
func Derive(attrs map[string]string) string {
	if v, exists := attrs["service.instance.id"]; exists && v != "" {
		if _, err := uuid.Parse(v); err == nil {
			return v
		}
		return stableUUID("service.instance.id:" + v)
	}
	if host := attrs["host.name"]; host != "" {
		return stableUUID("host.name:" + host)
	}
	if svc := attrs["service.name"]; svc != "" {
		return stableUUID("service.name:" + svc)
	}
	return "default"
}
