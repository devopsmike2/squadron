package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/demoseed"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
)

// handleDemoDataEnable is the one-click "Enable demo data" endpoint
// (POST /api/v1/demo/enable). It seeds the full demo scenario across feature
// areas so a first-time user sees Squadron's flagship loops working on sample
// data — no real cloud account, agent, or config required:
//   - Fleet + Configs + a cost spike (internal/demoseed): a demo group, its
//     baseline config, a demo agent, and a +312% cost spike the AI proposer
//     picks up to draft a rollout.
//   - Discovery: the reserved demo AWS connection, so the Inventory +
//     Recommendations surfaces serve canned sample data on scan.
//
// Idempotent: every underlying seed upserts / checks-before-create, so repeated
// enables are harmless. Everything is demo-scoped (reserved ids) and removable
// via DELETE /api/v1/demo.
func (s *Server) handleDemoDataEnable(c *gin.Context) {
	if s.appStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "application store is not configured"})
		return
	}

	summary, err := demoseed.Seed(c.Request.Context(), s.appStore, false)
	if err != nil {
		if s.logger != nil {
			s.logger.Error("demo data enable: seed failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not seed demo data; the error has been logged"})
		return
	}

	// Discovery demo connection is best-effort: it lights up Inventory +
	// Recommendations, but the fleet/cost demo above is already committed, so a
	// credstore hiccup shouldn't fail the whole enable.
	discoveryEnabled := false
	if s.discoveryCredStore != nil {
		if derr := s.discoveryCredStore.StoreConnection(c.Request.Context(), demo.Connection()); derr != nil {
			if s.logger != nil {
				s.logger.Warn("demo data enable: discovery demo connection failed", zap.Error(derr))
			}
		} else {
			discoveryEnabled = true
		}
	}

	// Live simulated production: stand up the ~500-agent fleet + its
	// continuous telemetry loop so every screen (fleet, per-agent
	// logs/metrics/traces, cost, savings) is populated and alive. Best-effort
	// and idempotent: the static demoseed rows above are already committed, so a
	// simulator hiccup shouldn't fail the enable. nil simulator = feature not
	// wired (e.g. no telemetry backend) → static demo only.
	fleetAgents := 0
	if s.demoSimulator != nil {
		if n, serr := s.demoSimulator.Enable(c.Request.Context()); serr != nil {
			if s.logger != nil {
				s.logger.Warn("demo data enable: simulator failed", zap.Error(serr))
			}
		} else {
			fleetAgents = n
		}
	}

	// Register GCP / Azure / OCI demo connections too, so all four Discovery
	// pages populate inventory + recommendations from the one click (not just
	// AWS). Best-effort + idempotent.
	s.enableCloudDemoConnections(c.Request.Context())

	// Light up the conversational AI surfaces (Ask Squadron / Explain / Merge)
	// with the rest of the demo. Only takes effect when no real ANTHROPIC_API_KEY
	// is configured — a real key always wins — so keyed installs are untouched.
	if s.aiService != nil {
		s.aiService.SetDemoMode(true)
	}

	c.JSON(http.StatusOK, gin.H{
		"status":            "enabled",
		"seeded":            summary,
		"discovery_enabled": discoveryEnabled,
		"fleet_agents":      fleetAgents,
	})
}

// handleDemoDataDisable removes the demo-scoped data (DELETE /api/v1/demo).
// Best-effort + idempotent: missing rows are treated as success.
func (s *Server) handleDemoDataDisable(c *gin.Context) {
	if s.appStore == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "application store is not configured"})
		return
	}

	// Turn the conversational AI demo responder back off.
	if s.aiService != nil {
		s.aiService.SetDemoMode(false)
	}

	// Tear down the live simulated fleet + stop its telemetry loop first.
	if s.demoSimulator != nil {
		if serr := s.demoSimulator.Disable(c.Request.Context()); serr != nil && s.logger != nil {
			s.logger.Warn("demo data disable: simulator teardown failed", zap.Error(serr))
		}
	}

	if err := demoseed.Remove(c.Request.Context(), s.appStore); err != nil {
		if s.logger != nil {
			s.logger.Error("demo data disable: remove failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not remove demo data; the error has been logged"})
		return
	}

	if s.discoveryCredStore != nil {
		if derr := s.discoveryCredStore.DeleteConnection(c.Request.Context(), demo.SentinelAccountID); derr != nil {
			if s.logger != nil {
				s.logger.Warn("demo data disable: discovery demo connection delete failed", zap.Error(derr))
			}
		}
	}
	// Remove the GCP / Azure / OCI demo connections too.
	s.removeCloudDemoConnections(c.Request.Context())

	c.JSON(http.StatusOK, gin.H{"status": "disabled"})
}
