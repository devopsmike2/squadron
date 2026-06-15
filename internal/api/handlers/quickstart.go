// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/quickstart"
)

// QuickstartHandlers wraps the v0.27.1 onboarding registry in
// HTTP shells. The package owns the templates; handlers own
// request parsing and the OpAMP server URL resolution.
type QuickstartHandlers struct {
	// OpAMPPort is the port the Squadron OpAMP server listens on.
	// Combined with the request Host (or an explicit ?host=)
	// query param to build the OpAMP server URL the agent will
	// dial.
	OpAMPPort int
	Logger    *zap.Logger
}

func NewQuickstartHandlers(opampPort int, logger *zap.Logger) *QuickstartHandlers {
	return &QuickstartHandlers{OpAMPPort: opampPort, Logger: logger}
}

// HandleCatalog — GET /api/v1/quickstart/backends
//
// Returns the list of backends Squadron ships starter configs
// for. The UI renders this as the backend picker.
func (h *QuickstartHandlers) HandleCatalog(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"items": quickstart.Catalog(),
	})
}

// HandleStarterConfig — GET /api/v1/quickstart/starter-config?backend=...&host=...
//
// Returns a complete OTel Collector config for the chosen backend,
// wired to talk back to this Squadron via OpAMP. The host param
// overrides the OpAMP host the agent will dial; defaults to the
// request's Host header so a localhost-running Squadron returns
// localhost URLs to its own UI.
func (h *QuickstartHandlers) HandleStarterConfig(c *gin.Context) {
	backend := strings.TrimSpace(c.Query("backend"))
	if backend == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "backend query param required"})
		return
	}
	opampURL := h.resolveOpAMPURL(c)
	cfg, err := quickstart.StarterConfig(quickstart.Backend(backend), opampURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"backend":          backend,
		"opamp_server_url": opampURL,
		"yaml":             cfg,
	})
}

// HandleOpAMPSnippet — GET /api/v1/quickstart/opamp-snippet?host=...
//
// Returns just the opamp extension YAML for pasting into an
// existing collector config. The killer feature for the "I have
// collectors already running" branch.
func (h *QuickstartHandlers) HandleOpAMPSnippet(c *gin.Context) {
	opampURL := h.resolveOpAMPURL(c)
	snippet, err := quickstart.OpAMPExtensionSnippet(opampURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"opamp_server_url": opampURL,
		"yaml":             snippet,
	})
}

// HandleAdoptionSnippet — GET /api/v1/quickstart/adoption-snippet?host=...&hostname=...&label=k=v&label=k2=v2
//
// Per-host adoption snippet for the v0.45 "bring this existing
// agent under management" workflow. Same extension config as
// /opamp-snippet but optionally tagged with a hostname + labels
// that get registered as agent attributes so Squadron sees this
// agent as the right inventory row instead of just "another
// collector that connected."
//
// The query params:
//
//	host:     optional — overrides the OpAMP URL host segment
//	          (same semantics as /opamp-snippet)
//	hostname: optional — registered as host.name attribute
//	label:    repeatable — k=v pairs registered as non-identifying
//	          attributes. Skips entries missing the "=" separator.
//
// Added in v0.45.0 (adoption workflow).
func (h *QuickstartHandlers) HandleAdoptionSnippet(c *gin.Context) {
	opampURL := h.resolveOpAMPURL(c)
	hostname := c.Query("hostname")
	labels := map[string]string{}
	for _, kv := range c.QueryArray("label") {
		idx := strings.Index(kv, "=")
		if idx <= 0 || idx == len(kv)-1 {
			continue
		}
		labels[kv[:idx]] = kv[idx+1:]
	}
	snippet, err := quickstart.AdoptionSnippet(opampURL, hostname, labels)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"opamp_server_url": opampURL,
		"hostname":         hostname,
		"labels":           labels,
		"yaml":             snippet,
	})
}

// resolveOpAMPURL builds the ws://host:port/v1/opamp URL the
// generated configs reference.
//
// Priority order:
//  1. Explicit ?host= query param (operator's deployed hostname,
//     e.g. squadron.example.com)
//  2. Request's Host header (works for local dev — the UI is
//     served from the same Squadron, so its Host header is the
//     reachable hostname)
//  3. localhost fallback
//
// We strip any :port suffix from the host before appending our
// own OpAMP port, since the API port (8080) and OpAMP port (4320)
// are different.
func (h *QuickstartHandlers) resolveOpAMPURL(c *gin.Context) string {
	host := strings.TrimSpace(c.Query("host"))
	if host == "" {
		host = c.Request.Host
	}
	if host == "" {
		host = "localhost"
	}
	// Strip :port if present so we can append the OpAMP port.
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return fmt.Sprintf("ws://%s:%d/v1/opamp", host, h.OpAMPPort)
}
