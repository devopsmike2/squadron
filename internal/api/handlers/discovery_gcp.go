// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/ai"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/gcpconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
)

// GCPScannerFactory builds a scanner.Scanner for a given GCP
// connection. The factory accepts the unsealed Service Account JSON
// bytes alongside the connection row so the production wire (chunk-2
// boundary) can construct a *gcp.Scanner without the handler ever
// importing the chunk-2 GCP package directly. Tests substitute a
// fakeGCPScannerFactory that returns a pre-seeded fake scanner.
//
// The factory is per-request because the SA JSON is unsealed
// per-request (it's never held in memory across calls); production
// implementations may close over the credstore.Key the handler does
// not see.
//
// See docs/proposals/gcp-discovery-slice1.md §10 contract item 7 for
// the boundary rationale: chunks 2 and 3 ship in parallel worktrees,
// so the handler depends on the provider-agnostic scanner.Scanner
// interface and main.go composes the concrete *gcp.Scanner at
// startup.
type GCPScannerFactory interface {
	Build(conn *gcpconnstore.GCPConnection, saJSON []byte) (scanner.Scanner, error)
}

// gcpProjectIDPattern enforces the GCP project naming rule from
// design doc §7 step 1 / §13 acceptance test 10: a lower-case ASCII
// letter followed by 5 to 29 characters of [a-z0-9-], with the
// trailing character a letter or digit. Mirrors GCP's documented
// project-ID regex.
var gcpProjectIDPattern = regexp.MustCompile(`^[a-z][-a-z0-9]{4,28}[a-z0-9]$`)

// gcpSAClientEmailSuffix is the trailing fragment every legitimate
// GCP service-account client_email carries. Per design doc §12 Q1 the
// Create handler optionally parses the SA JSON's client_email field and
// rejects values that do not end here — a small validation that catches
// the operator who pasted the wrong JSON file. Keeping the constant
// pinned here lets the SAJSON parsing test assert against a stable
// shape.
const gcpSAClientEmailSuffix = ".iam.gserviceaccount.com"

// gcpValidateHandlerTimeout caps the GCP validate endpoint at 30s.
// Validate calls compute.instances.list once on the first available
// zone; the happy path round-trips well under a second. The
// shorter-than-AWS budget reflects the lighter probe — validate's
// purpose is "fast confidence check", not a full inventory walk.
const gcpValidateHandlerTimeout = 30 * time.Second

// gcpScanHandlerTimeout caps the GCP scan endpoint at 5 minutes —
// same upper bound as the AWS scan handler. A multi-zone Compute
// Engine walk can legitimately span minutes on a fleet with thousands
// of instances.
const gcpScanHandlerTimeout = 5 * time.Minute

// DiscoveryGCPHandlers serves the GCP-side connector + scan + validate
// + recommendations surface — the slice-1 mirror of the AWS handler
// in discovery.go. Chunk 3 of #667 introduces it; chunks 2 (scanner)
// and 5 (proposer) are in parallel worktrees and bolt onto the
// scanner.Scanner interface / GCPScannerFactory respectively.
//
// store is consumed by every endpoint except the recommendations
// stub. credstoreKey is consumed by Validate / Scan to unseal the
// connection's SA JSON. auditService is consumed by Create / Delete /
// Scan; Validate intentionally produces no audit signal (per design
// doc §11.3 + runbook note: validate is the operator's confidence
// probe, lighter weight than a real scan). logger is required.
type DiscoveryGCPHandlers struct {
	store          gcpconnstore.Store
	credstoreKey   *credstore.Key
	auditService   services.AuditService
	scannerFactory GCPScannerFactory
	// traceIndex — v0.89.77 trace integration slice 1 chunk 4.
	// Optional; see DiscoveryHandlers.traceIndex godoc for posture.
	traceIndex TraceIndexLookup
	logger     *zap.Logger
	// aiProposer — chunk 5 (v0.89.197). The discovery-side AI proposer
	// HandleRecommendationsForGCPScan calls. nil when AI assist is off;
	// the handler 503s in that case.
	aiProposer DiscoveryAIProposer
	// acceptedAssembler — parity follow-up (v0.89.199): feeds the
	// verdict few-shot block + discovery_proposal.created examples.
	// nil = cold-start empty.
	acceptedAssembler DiscoveryAcceptedRecommendationsAssembler
	// scanStore — continuous-discovery slice 2 (v0.89.251). Persists
	// completed scans + backs the history endpoints. Nil = non-persisted.
	scanStore DiscoveryScanStore
}

// NewDiscoveryGCPHandlers builds the handler struct. Optional
// dependencies are wired via the With* methods; mirror the
// NewDiscoveryHandlers + WithAuditService + WithCredstoreKey shape
// used by the AWS surface so the trampoline in server.go can call
// them through.
func NewDiscoveryGCPHandlers(store gcpconnstore.Store, logger *zap.Logger) *DiscoveryGCPHandlers {
	return &DiscoveryGCPHandlers{
		store:  store,
		logger: logger,
	}
}

// WithGCPAuditService wires the audit recorder used by Create /
// Delete / Scan. Nil leaves audit emission as a no-op; the rest of
// the surface stays unaffected.
func (h *DiscoveryGCPHandlers) WithGCPAuditService(a services.AuditService) *DiscoveryGCPHandlers {
	h.auditService = a
	return h
}

// WithGCPCredstoreKey wires the credstore key used to seal SA JSON
// at create time and unseal it at validate/scan time. A nil key
// leaves Create / Validate / Scan 500ing with a humanized error.
func (h *DiscoveryGCPHandlers) WithGCPCredstoreKey(k *credstore.Key) *DiscoveryGCPHandlers {
	h.credstoreKey = k
	return h
}

// WithGCPScannerFactory wires the scanner factory used by Validate
// and Scan. Production wires a factory that builds *gcp.Scanner
// (chunk 2 type, lives in a parallel worktree); tests substitute a
// fake that returns a pre-canned scanner.Scanner. A nil factory
// leaves Validate / Scan 500ing with a humanized error.
// WithGCPTraceIndex wires the v0.89.77 trace integration slice 1
// chunk 4 traceindex lookup. Nil leaves scan responses
// un-annotated; production wires the same Index chunk 3 wired into
// the Discovery dashboard.
func (h *DiscoveryGCPHandlers) WithGCPTraceIndex(idx TraceIndexLookup) *DiscoveryGCPHandlers {
	h.traceIndex = idx
	return h
}

// WithGCPScanStore wires the persisted scan-history store (slice 2).
func (h *DiscoveryGCPHandlers) WithGCPScanStore(s DiscoveryScanStore) *DiscoveryGCPHandlers {
	h.scanStore = s
	return h
}

// HandleGCPListScans — GET /api/v1/discovery/gcp/connections/:id/scans.
func (h *DiscoveryGCPHandlers) HandleGCPListScans(c *gin.Context) {
	writeScanList(c, h.scanStore, h.logger, "gcp", strings.TrimSpace(c.Param("id")))
}

// HandleGCPGetScan — GET /api/v1/discovery/gcp/connections/:id/scans/:scanID.
func (h *DiscoveryGCPHandlers) HandleGCPGetScan(c *gin.Context) {
	writeScanDetail(c, h.scanStore, h.logger, "gcp",
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(c.Param("scanID")))
}

// HandleGCPScanDrift — GET /api/v1/discovery/gcp/connections/:id/drift.
func (h *DiscoveryGCPHandlers) HandleGCPScanDrift(c *gin.Context) {
	writeDrift(c, h.scanStore, h.logger, "gcp", strings.TrimSpace(c.Param("id")))
}

func (h *DiscoveryGCPHandlers) WithGCPScannerFactory(f GCPScannerFactory) *DiscoveryGCPHandlers {
	h.scannerFactory = f
	return h
}

// WithGCPAIProposer wires the discovery-side AI proposer used by
// HandleRecommendationsForGCPScan. Production wires s.discoveryAIService;
// nil leaves recommendations 503-ing.
func (h *DiscoveryGCPHandlers) WithGCPAIProposer(p DiscoveryAIProposer) *DiscoveryGCPHandlers {
	h.aiProposer = p
	return h
}

// WithGCPAcceptedAssembler wires the accepted-recommendations assembler (verdict
// few-shot + discovery_proposal.created examples).
func (h *DiscoveryGCPHandlers) WithGCPAcceptedAssembler(a DiscoveryAcceptedRecommendationsAssembler) *DiscoveryGCPHandlers {
	h.acceptedAssembler = a
	return h
}

// --- Create -------------------------------------------------------------

// gcpCreateConnectionRequest is the JSON wire shape the wizard
// POSTs. SealedSA carries the base64-encoded Service Account JSON —
// base64 over the wire to avoid JSON-in-JSON escape pain per design
// doc §6 (the server base64-decodes then credstore-seals before
// storage).
type gcpCreateConnectionRequest struct {
	DisplayName string `json:"display_name"`
	ProjectID   string `json:"project_id"`
	SealedSA    string `json:"sealed_sa"`
	Region      string `json:"region"`
}

// HandleCreateGCPConnection — POST /api/v1/discovery/gcp/connections.
//
// Per design doc §6: the body carries display_name, project_id,
// base64-encoded SA JSON, and region. The handler:
//  1. Validates the request shape (400 on missing fields, on
//     malformed base64, on invalid project_id format, on a SA JSON
//     whose client_email doesn't end in .iam.gserviceaccount.com).
//  2. Seals the SA JSON via credstore.SealGCPServiceAccount.
//  3. Persists the row via store.Create.
//  4. Emits discovery.gcp.connection_created.
//  5. Returns 201 with the connection JSON (SealedSA is suppressed by
//     the json:"-" tag — never appears in the response).
func (h *DiscoveryGCPHandlers) HandleCreateGCPConnection(c *gin.Context) {
	var req gcpCreateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message:       "Request body could not be parsed as JSON. Check the wizard's payload shape.",
			SuggestedStep: "save",
		}})
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingDisplayName",
			Message:       "Display name is required. Provide a label so operators can identify the connection in the list.",
			SuggestedStep: "save",
		}})
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingProjectID",
			Message:       "GCP project ID is required. Paste the value from Step 1 of the GCP wizard.",
			SuggestedStep: "project-id",
		}})
		return
	}
	if !gcpProjectIDPattern.MatchString(req.ProjectID) {
		// Per design doc §7 step 1 / §13 acceptance test 10 the
		// project ID must conform to GCP's documented rule:
		// lower-case ASCII letter prefix, 4-28 inner [a-z0-9-]
		// characters, terminal letter/digit. UPPERCASE pastes,
		// underscores, and short/long forms all fail this check.
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidProjectID",
			Message:       "GCP project ID does not match the required format ([a-z][-a-z0-9]{4,28}[a-z0-9]). Verify the value in the GCP console.",
			SuggestedStep: "project-id",
		}})
		return
	}
	if strings.TrimSpace(req.SealedSA) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingServiceAccount",
			Message:       "Service Account JSON is required. Paste the JSON contents from Step 3 of the GCP wizard.",
			SuggestedStep: "service-account",
		}})
		return
	}
	saJSON, err := base64.StdEncoding.DecodeString(req.SealedSA)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidServiceAccountBase64",
			Message:       "Service Account payload is not valid base64. The wizard should encode the JSON before submission; check the client-side encoder.",
			SuggestedStep: "service-account",
		}})
		return
	}
	// Optional SA JSON sanity-check per design doc §12 Q1. Catches
	// the operator who pasted a non-SA JSON file. Failures here are
	// 400s because they're operator-recoverable; a malformed JSON
	// blob (e.g. garbled paste) and a wrong-file paste land in the
	// same humanized message.
	if err := validateGCPServiceAccountJSON(saJSON); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidServiceAccountJSON",
			Message:       err.Error(),
			SuggestedStep: "service-account",
		}})
		return
	}

	if h.store == nil {
		// Belt-and-braces: the trampoline already 503s when the store
		// is nil. Surface as 500 for the struct-literal construction
		// path.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "GCPStoreNotWired",
			Message:       "Squadron's GCP connection substrate isn't configured.",
			SuggestedStep: "save",
		}})
		return
	}
	if h.credstoreKey == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "CredKeyNotWired",
			Message:       "Squadron's credential encryption key isn't configured. The Create flow cannot persist without it.",
			SuggestedStep: "save",
		}})
		return
	}

	sealed, err := credstore.SealGCPServiceAccount(h.credstoreKey, saJSON)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("gcp create connection: SA seal failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "SAEncryptFailed",
			Message:       "Squadron could not encrypt the Service Account JSON. Verify SQUADRON_SECRETS_KEY is set and retry.",
			SuggestedStep: "save",
		}})
		return
	}

	conn := &gcpconnstore.GCPConnection{
		DisplayName:                      strings.TrimSpace(req.DisplayName),
		ProjectID:                        strings.TrimSpace(req.ProjectID),
		Region:                           strings.TrimSpace(req.Region),
		SealedSA:                         sealed,
		LearnFromAcceptedRecommendations: true,
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("gcp create connection: store write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "GCPStoreWriteFailed",
			Message:       "Squadron could not persist the GCP connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Audit emit. The SealedSA blob is NEVER in the payload — both
	// the json:"-" tag and the explicit payload shape below enforce
	// this. Mirrors the AWS surface's no-credential-in-payload posture.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryGCPConnectionCreated,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "created",
			Payload: map[string]any{
				"connection_id": conn.ID,
				"display_name":  conn.DisplayName,
				"project_id":    conn.ProjectID,
				"region":        conn.Region,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}

	// The GCPConnection's SealedSA field carries json:"-", so the
	// marshaled response naturally omits the sealed bytes — the
	// design-doc §13 acceptance test 1 invariant. Test asserts
	// against the response body.
	c.JSON(http.StatusCreated, conn)
}

// --- List ---------------------------------------------------------------

// gcpListConnectionsResponse is the wire shape the GCP discovery page
// fetches. Empty array (NOT null) when no rows — matches the AWS
// counterpart's posture so the UI's empty-state branch is a single
// .length check.
type gcpListConnectionsResponse struct {
	Connections []*gcpconnstore.GCPConnection `json:"connections"`
}

// HandleListGCPConnections — GET /api/v1/discovery/gcp/connections.
//
// Returns every stored GCP connection. SealedSA stays out of the
// response by way of the json:"-" tag on the field. Empty store
// returns {"connections": []} with 200, not 404.
func (h *DiscoveryGCPHandlers) HandleListGCPConnections(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("gcp list connections: store read failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the GCP connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	if conns == nil {
		conns = []*gcpconnstore.GCPConnection{}
	}
	c.JSON(http.StatusOK, gcpListConnectionsResponse{Connections: conns})
}

// --- Get ----------------------------------------------------------------

// HandleGetGCPConnection — GET
// /api/v1/discovery/gcp/connections/:id.
//
// Returns the single GCP connection row identified by :id. SealedSA
// stays out of the response by way of the json:"-" tag on the field.
// 404 when no row matches.
func (h *DiscoveryGCPHandlers) HandleGetGCPConnection(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp get connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the GCP connection. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// --- Update -------------------------------------------------------------

// gcpUpdateConnectionRequest is the JSON wire shape the PATCH endpoint
// accepts. All three fields are pointers so the handler can
// distinguish "omitted, preserve existing" from "explicit empty
// value". DisplayName "" via pointer would zero out the row — slice 1
// rejects that case at validation time but the wire shape allows it
// for parity with future slices.
type gcpUpdateConnectionRequest struct {
	DisplayName                      *string `json:"display_name,omitempty"`
	Region                           *string `json:"region,omitempty"`
	LearnFromAcceptedRecommendations *bool   `json:"learn_from_accepted_recommendations,omitempty"`
}

// HandleUpdateGCPConnection — PATCH
// /api/v1/discovery/gcp/connections/:id.
//
// PATCH semantics: only fields explicitly present in the body are
// updated. ProjectID + SealedSA are NEVER mutated by this endpoint —
// rotation is delete + re-create, matching the substrate's
// design-doc §5 posture.
//
// Returns 200 with the updated connection JSON. 404 when no row
// matches; 400 on a malformed body or an explicit empty
// DisplayName.
func (h *DiscoveryGCPHandlers) HandleUpdateGCPConnection(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	var req gcpUpdateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}

	existing, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp update connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the GCP connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Build the update payload by overlaying the patch on existing.
	// PATCH semantics: pointer-nil fields preserve the existing value.
	if req.DisplayName != nil {
		if strings.TrimSpace(*req.DisplayName) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
				Code:    "EmptyDisplayName",
				Message: "Display name must be non-empty when supplied.",
			}})
			return
		}
		existing.DisplayName = strings.TrimSpace(*req.DisplayName)
	}
	if req.Region != nil {
		existing.Region = strings.TrimSpace(*req.Region)
	}
	if req.LearnFromAcceptedRecommendations != nil {
		existing.LearnFromAcceptedRecommendations = *req.LearnFromAcceptedRecommendations
	}
	// SealedSA stays "" so the substrate's Update preserves the
	// stored sealed bytes per its documented contract.
	updatePayload := &gcpconnstore.GCPConnection{
		ID:                               existing.ID,
		DisplayName:                      existing.DisplayName,
		ProjectID:                        existing.ProjectID,
		Region:                           existing.Region,
		LearnFromAcceptedRecommendations: existing.LearnFromAcceptedRecommendations,
	}
	if err := h.store.Update(c.Request.Context(), updatePayload); err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp update connection: store write failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreWriteFailed",
			Message: "Squadron could not persist the GCP connection update. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Re-read to pick up the substrate-stamped UpdatedAt + the
	// preserved SealedSA (which we'll suppress on marshal via the
	// json:"-" tag).
	fresh, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		// Should not happen on the happy path — fall back to the
		// in-memory copy so the caller still sees the update result.
		c.JSON(http.StatusOK, updatePayload)
		return
	}
	c.JSON(http.StatusOK, fresh)
}

// --- Delete -------------------------------------------------------------

// HandleDeleteGCPConnection — DELETE
// /api/v1/discovery/gcp/connections/:id.
//
// Emits discovery.gcp.connection_deleted. The substrate's Delete is
// idempotent (deleting a missing row is not an error) so the handler
// returns 204 in both the "row existed" and "row already gone"
// cases.
func (h *DiscoveryGCPHandlers) HandleDeleteGCPConnection(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}

	// Look up project_id before delete so the audit payload can carry
	// it. A missing row produces an empty project_id — the substrate's
	// idempotent delete still fires.
	projectID := ""
	if existing, err := h.store.Get(c.Request.Context(), id); err == nil && existing != nil {
		projectID = existing.ProjectID
	}

	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		if h.logger != nil {
			h.logger.Error("gcp delete connection: store delete failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreDeleteFailed",
			Message: "Squadron could not delete the GCP connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryGCPConnectionDeleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   id,
			Action:     "deleted",
			Payload: map[string]any{
				"connection_id": id,
				"project_id":    projectID,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}
	c.Status(http.StatusNoContent)
}

// --- Validate -----------------------------------------------------------

// gcpValidateResponse is the wire shape the wizard's "Validate"
// button renders. The success path carries instance_count; failures
// carry error_kind + a humanized message. Mirrors design doc §6.1.
type gcpValidateResponse struct {
	OK            bool   `json:"ok"`
	InstanceCount int    `json:"instance_count,omitempty"`
	ErrorKind     string `json:"error_kind,omitempty"`
	Message       string `json:"message,omitempty"`
}

// HandleValidateGCPConnection — POST
// /api/v1/discovery/gcp/connections/:id/validate.
//
// Per design doc §6.1: unseals the stored SA JSON, cross-checks the
// SA's project_id against the connection row, builds a Scanner via
// the factory, and calls Scan with a short timeout. The endpoint
// produces NO audit signal — validate is the operator's lightweight
// confidence probe, not a real scan. Runbook §11.3 documents this.
func (h *DiscoveryGCPHandlers) HandleValidateGCPConnection(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	if h.credstoreKey == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "CredKeyNotWired",
			Message: "Squadron's credential encryption key isn't configured.",
		}})
		return
	}
	if h.scannerFactory == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPScannerNotWired",
			Message: "Squadron's GCP scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp validate: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the GCP connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	saJSON, err := credstore.UnsealGCPServiceAccount(h.credstoreKey, conn.SealedSA)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("gcp validate: SA unseal failed", zap.Error(err), zap.String("id", id))
		}
		// Surface as an operator-recoverable validate result rather
		// than a 500 — the operator can re-paste the SA via delete +
		// re-create. The error_kind is credentials_invalid because
		// the cipher rejected the blob.
		c.JSON(http.StatusOK, gcpValidateResponse{
			OK:        false,
			ErrorKind: "credentials_invalid",
			Message:   "Squadron could not decrypt the stored Service Account JSON. Delete the connection and re-paste the SA key.",
		})
		return
	}

	// Per design doc §11.3 — cross-check the SA's project against
	// the connection's configured project before any GCP call. This
	// catches the silent-half-empty-scan failure mode where an SA
	// minted in project A is paired with a connection configured
	// for project B (GCP returns "" for that scenario; the operator
	// has no signal anything is wrong).
	saProjectID, _ := extractGCPSAProjectID(saJSON)
	if saProjectID != "" && saProjectID != conn.ProjectID {
		c.JSON(http.StatusOK, gcpValidateResponse{
			OK:        false,
			ErrorKind: "project_mismatch",
			Message:   "The Service Account belongs to project " + saProjectID + " but this connection is configured for project " + conn.ProjectID + ". Delete the connection and re-create it with a Service Account from the right project.",
		})
		return
	}

	scn, err := h.scannerFactory.Build(conn, saJSON)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("gcp validate: scanner build failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusOK, gcpValidateResponse{
			OK:        false,
			ErrorKind: classifyGCPScanError(err),
			Message:   "Squadron could not initialize the GCP scanner: " + err.Error(),
		})
		return
	}

	// Tight timeout to keep validate snappy — the design doc's
	// §6.1 calls validate "the operator's confidence check"; a
	// validate that hangs for minutes defeats the purpose.
	scanCtx, cancel := context.WithTimeout(c.Request.Context(), gcpValidateHandlerTimeout)
	defer cancel()
	// Validate calls Scan with the project-as-region scope; the
	// chunk-2 scanner walks compute.instances.list per zone. Per
	// the scanner.Scanner interface, the regions slice carries
	// "any region the scanner should walk"; an empty slice + the
	// scanner's per-conn region filter is the production wiring.
	regions := []string{}
	if strings.TrimSpace(conn.Region) != "" {
		regions = append(regions, conn.Region)
	}
	// Validate uses the scanner.Scanner interface but does NOT
	// require a *credstore.CloudConnection — chunk-2's GCP scanner
	// satisfies the interface with a wrapper that pulls the project
	// + SA out of its constructor closure. We pass nil here; the
	// chunk-2 scanner ignores the conn arg per its construction
	// (which captured project + region at Build time).
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		c.JSON(http.StatusOK, gcpValidateResponse{
			OK:        false,
			ErrorKind: classifyGCPScanError(err),
			Message:   "GCP scan probe failed: " + err.Error(),
		})
		return
	}
	if result == nil {
		c.JSON(http.StatusOK, gcpValidateResponse{
			OK:            true,
			InstanceCount: 0,
		})
		return
	}
	c.JSON(http.StatusOK, gcpValidateResponse{
		OK:            true,
		InstanceCount: len(result.Compute),
	})
}

// --- Scan ---------------------------------------------------------------

// gcpScanResponse is the wire shape the scan endpoint returns. Wraps
// the scanner.Result with a few connection-level fields so the UI
// doesn't need to round-trip back to the connection row to render
// the inventory panel.
//
// v0.89.66 (#695 Stream 93, database tier slice 2 chunk 5) — adds
// the Databases field carrying the Cloud SQL instance inventory the
// chunk 2 scanner extension populates. The omitempty tag preserves
// the cold-start wire shape for handlers that ran before the chunk
// 2 scanner extension (the Databases slice is nil on those paths).
//
// v0.89.71 (#702 Stream 100, Kubernetes tier slice 2 chunk 5) —
// adds the Clusters field carrying the GKE cluster inventory the
// v0.89.70 chunk 2 GKE scanner populates on result.Clusters. The
// omitempty tag preserves the cold-start wire shape for handlers
// that ran before the v0.89.70 scanner extension (the Clusters
// slice is nil on those paths).
type gcpScanResponse struct {
	ConnectionID        string                             `json:"connection_id"`
	ProjectID           string                             `json:"project_id"`
	Region              string                             `json:"region"`
	Compute             []scanner.ComputeInstanceSnapshot  `json:"compute"`
	Databases           []scanner.DatabaseInstanceSnapshot `json:"databases,omitempty"`
	Clusters            []scanner.ClusterSnapshot          `json:"clusters,omitempty"`
	ObjectStores        []scanner.ObjectStoreSnapshot      `json:"object_stores,omitempty"`
	LoadBalancers       []scanner.LoadBalancerSnapshot     `json:"load_balancers,omitempty"`
	InstrumentedCount   int                                `json:"instrumented_count"`
	UninstrumentedCount int                                `json:"uninstrumented_count"`
	Partial             bool                               `json:"partial"`
	PartialReason       string                             `json:"partial_reason,omitempty"`
	FailedServices      []string                           `json:"failed_services,omitempty"`
	ScanID              string                             `json:"scan_id"`
	EventSources        []eventSourceRow                   `json:"event_sources,omitempty"`
	// Serverless carries the per-function rows (Cloud Run / Cloud
	// Functions) with their cold-start + error-rate detection
	// annotations. The snapshot type is marshaled directly (these
	// clouds don't use a snake_case wire row), so both regression
	// axes round-trip into the recs request DTO — feeding the
	// detection→recommendation flow (parity with AWS).
	Serverless []scanner.ServerlessInstanceSnapshot `json:"serverless,omitempty"`
}

// HandleScanGCPConnection — POST
// /api/v1/discovery/gcp/connections/:id/scan.
//
// Per design doc §6 + §13 acceptance test 5: emits
// discovery.gcp.scan_started, builds a Scanner via the factory, calls
// Scan with a 5-minute timeout, emits discovery.gcp.scan_completed
// (with per-category counts + the partial flag) on success or
// discovery.gcp.scan_failed on a hard error.
func (h *DiscoveryGCPHandlers) HandleScanGCPConnection(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreNotWired",
			Message: "Squadron's GCP connection substrate isn't configured.",
		}})
		return
	}
	if h.credstoreKey == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "CredKeyNotWired",
			Message: "Squadron's credential encryption key isn't configured.",
		}})
		return
	}
	if h.scannerFactory == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPScannerNotWired",
			Message: "Squadron's GCP scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp scan: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPStoreReadFailed",
			Message: "Squadron could not read the GCP connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Demo mode (v0.89.243, first-user onboarding): the reserved demo
	// project serves a canned sample inventory. Short-circuit after the
	// store read (the row is real) but before any SA decrypt or scanner
	// build — no GCP credentials, no cloud calls.
	if demo.IsGCPDemoProject(conn.ProjectID) {
		r := demo.GCPResult()
		instr, uninstr := 0, 0
		for _, ci := range r.Compute {
			if ci.HasOTel {
				instr++
			} else {
				uninstr++
			}
		}
		c.JSON(http.StatusOK, gcpScanResponse{
			ConnectionID:        conn.ID,
			ProjectID:           conn.ProjectID,
			Region:              conn.Region,
			Compute:             r.Compute,
			Databases:           r.Databases,
			Clusters:            r.Clusters,
			ObjectStores:        r.ObjectStores,
			LoadBalancers:       r.LoadBalancers,
			InstrumentedCount:   instr,
			UninstrumentedCount: uninstr,
			Partial:             false,
			ScanID:              r.ScanID,
			Serverless:          r.Serverless,
		})
		return
	}

	// scan_started fires BEFORE any scanner call so a forensic
	// reader can correlate a stranded scan_started (no matching
	// scan_completed / scan_failed) with an unhandled crash. Mirrors
	// the AWS scan handler's invariant.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryGCPScanStarted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_started",
			Payload: map[string]any{
				"connection_id": conn.ID,
				"project_id":    conn.ProjectID,
				"region":        conn.Region,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}

	saJSON, err := credstore.UnsealGCPServiceAccount(h.credstoreKey, conn.SealedSA)
	if err != nil {
		h.emitGCPScanFailed(c.Request.Context(), conn, "", "credentials_invalid",
			"Squadron could not decrypt the stored Service Account JSON. Delete the connection and re-paste the SA key.",
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "SADecryptFailed",
			Message: "Squadron could not decrypt the stored Service Account JSON.",
		}})
		return
	}

	scn, err := h.scannerFactory.Build(conn, saJSON)
	if err != nil {
		kind := classifyGCPScanError(err)
		h.emitGCPScanFailed(c.Request.Context(), conn, "", kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPScannerBuildFailed",
			Message: "Squadron could not initialize the GCP scanner: " + err.Error(),
		}})
		return
	}

	scanCtx, cancel := context.WithTimeout(c.Request.Context(), gcpScanHandlerTimeout)
	defer cancel()
	regions := []string{}
	if strings.TrimSpace(conn.Region) != "" {
		regions = append(regions, conn.Region)
	}
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		kind := classifyGCPScanError(err)
		scanID := ""
		if result != nil {
			scanID = result.ScanID
		}
		h.emitGCPScanFailed(c.Request.Context(), conn, scanID, kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPScanFailed",
			Message: "GCP scan failed: " + err.Error(),
		}})
		return
	}
	if result == nil {
		// Degenerate path — the contract says a non-error return
		// carries a non-nil Result. Surface as a 500 with audit so a
		// future regression is visible.
		h.emitGCPScanFailed(c.Request.Context(), conn, "", "unknown", "scanner returned nil result")
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "GCPScanNilResult",
			Message: "GCP scanner returned an empty result.",
		}})
		return
	}

	instanceCount := len(result.Compute)
	instrumentedCount := result.InstrumentedCount
	uninstrumentedCount := result.UninstrumentedCount
	// Defense in depth — slice 1's scanner derives these from the
	// per-row HasOTel flag; we recompute here in case a future
	// scanner returns the raw rows without the tally.
	if instrumentedCount == 0 && uninstrumentedCount == 0 {
		for _, ci := range result.Compute {
			if ci.HasOTel {
				instrumentedCount++
			} else {
				uninstrumentedCount++
			}
		}
	}

	if h.auditService != nil {
		payload := map[string]any{
			"connection_id":        conn.ID,
			"project_id":           conn.ProjectID,
			"region":               conn.Region,
			"scan_id":              result.ScanID,
			"instance_count":       instanceCount,
			"instrumented_count":   instrumentedCount,
			"uninstrumented_count": uninstrumentedCount,
			"partial":              result.Partial,
			"recorded_at":          time.Now().UTC(),
		}
		if result.PartialReason != "" {
			payload["partial_reason"] = result.PartialReason
		}
		if len(result.FailedServices) > 0 {
			payload["failed_services"] = result.FailedServices
		}
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryGCPScanCompleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_completed",
			Payload:    payload,
		})
	}

	// Trace integration slice 1 chunk 4 (v0.89.77) — annotate the
	// per-resource last_seen_at in-place against the traceindex
	// before the response is serialized. The scope_id projection
	// uses the GCP project_id per design doc §6.
	if h.traceIndex != nil {
		AnnotateComputeWithLastSeen(c.Request.Context(), h.traceIndex, "gcp", conn.ProjectID, result.Compute, h.logger)
		AnnotateDatabaseWithLastSeen(c.Request.Context(), h.traceIndex, "gcp", conn.ProjectID, result.Databases, h.logger)
		AnnotateClusterWithLastSeen(c.Request.Context(), h.traceIndex, "gcp", conn.ProjectID, result.Clusters, h.logger)
	}

	// Event-source tier (v0.89.195) — gated dispatch mirroring the AWS
	// path (HandleAWSRunScan). The optional request body carries the
	// tier list; when it includes event_source (the default set does)
	// and the GCP scanner implements EventSourceDiscoveryScanner, walk
	// Pub/Sub event sources and fold them into the result so the
	// response surfaces them with the slice-2 propagation axis. The GCP
	// Inventory tab's Event-sources sub-tab already renders
	// scan.event_sources; this is the missing producer.
	var scanReq struct {
		Tiers []string `json:"tiers,omitempty"`
	}
	_ = c.ShouldBindJSON(&scanReq)
	if tierListContains(parseTiersOrDefault(scanReq.Tiers), TierEventSource) {
		if esScanner, ok := scn.(EventSourceDiscoveryScanner); ok {
			esOut, esErr := esScanner.ScanEventSources(scanCtx, scanner.ScanScope{
				AccountID: conn.ProjectID,
				Regions:   regions,
			})
			if esErr != nil {
				if h.logger != nil {
					h.logger.Warn("gcp scan: event source scan failed",
						zap.Error(esErr), zap.String("project_id", conn.ProjectID))
				}
			}
			if len(esOut) > 0 {
				result.EventSources = append(result.EventSources, esOut...)
			}
		}
	}

	resp := gcpScanResponse{
		ConnectionID:        conn.ID,
		ProjectID:           conn.ProjectID,
		Region:              conn.Region,
		Compute:             result.Compute,
		Databases:           result.Databases,
		Clusters:            result.Clusters,
		ObjectStores:        result.ObjectStores,
		LoadBalancers:       result.LoadBalancers,
		InstrumentedCount:   instrumentedCount,
		UninstrumentedCount: uninstrumentedCount,
		Partial:             result.Partial,
		PartialReason:       result.PartialReason,
		FailedServices:      result.FailedServices,
		ScanID:              result.ScanID,
		EventSources:        marshalEventSourceRows(result.EventSources),
		Serverless:          result.Serverless,
	}
	// slice 2 (v0.89.251) — persist the completed scan (best-effort). Scope
	// is the route :id (connection ID). Demo path returned earlier, so demo
	// scans are not persisted.
	if h.scanStore != nil {
		if rj, err := json.Marshal(resp); err == nil {
			recordScan(c.Request.Context(), h.scanStore, h.logger, "gcp", conn.ID, result, rj)
		} else if h.logger != nil {
			h.logger.Warn("gcp scan: marshal for persistence failed",
				zap.Error(err), zap.String("scan_id", result.ScanID))
		}
	}
	c.JSON(http.StatusOK, resp)
}

// emitGCPScanFailed records a discovery.gcp.scan_failed audit event
// with the supplied connection + error metadata. Safe to call when
// auditService is nil. Payload NEVER carries the plaintext SA JSON
// or the sealed bytes — the substrate's no-credential-in-audit
// invariant extends to the GCP path.
func (h *DiscoveryGCPHandlers) emitGCPScanFailed(ctx context.Context, conn *gcpconnstore.GCPConnection, scanID, errorKind, message string) {
	if h.auditService == nil || conn == nil {
		return
	}
	payload := map[string]any{
		"connection_id":     conn.ID,
		"project_id":        conn.ProjectID,
		"region":            conn.Region,
		"error_kind":        errorKind,
		"humanized_message": message,
		"recorded_at":       time.Now().UTC(),
	}
	if scanID != "" {
		payload["scan_id"] = scanID
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventDiscoveryGCPScanFailed,
		TargetType: credstore.TargetTypeCloudConnection,
		TargetID:   conn.ID,
		Action:     "scan_failed",
		Payload:    payload,
	})
}

// --- Recommendations stub -----------------------------------------------

// HandleRecommendationsForGCPScan — POST
// /api/v1/discovery/gcp/connections/:id/recommendations.
//
// Chunk 3 of #667 ships this as a 501 NotImplemented stub. Chunk 5
// of #667 (the proposer integration) wires the real path — adding
// the Provider field on DiscoveryScanContext, the gce-otel-label
// recommendation kind, and the system-prompt extension. Until then,
// the route returns a humanized "coming in chunk 5" message so the
// UI can render an empty Recommendations tab without 404ing.
// gcpGenerateRecommendationsRequest is the POST body for the GCP
// generate-recommendations endpoint: the gcpScanResponse the operator
// just received, echoed back so the proposer reasons over the same
// inventory the Inventory tab rendered.
type gcpGenerateRecommendationsRequest struct {
	ScanResult gcpScanResponse `json:"scan_result"`
}

// HandleRecommendationsForGCPScan — POST
// /api/v1/discovery/gcp/connections/:id/recommendations (chunk 5,
// v0.89.197). Builds a Provider="gcp" DiscoveryScanContext from the
// posted scan result and runs the shared proposer, mirroring the AWS
// handler. Event sources flow through mapEventSourceCandidates; the
// plan walks through buildDiscoveryRecommendations.
func (h *DiscoveryGCPHandlers) HandleRecommendationsForGCPScan(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	// Demo mode (v0.89.243): the reserved demo project serves seeded
	// recommendations through the same buildDiscoveryRecommendations walk —
	// no LLM, no API key. Resolve the connection early so we can detect the
	// demo sentinel before the aiProposer wiring check (a keyless first-user
	// has aiProposer == nil).
	if h.store != nil {
		if conn, err := h.store.Get(c.Request.Context(), id); err == nil && conn != nil && demo.IsGCPDemoProject(conn.ProjectID) {
			job := defaultRecommendationJobStore.Create("gcp", conn.ProjectID)
			defaultRecommendationJobStore.Run(job.ID, func(_ context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
				now := time.Now().UTC()
				recs, bErr := buildDiscoveryRecommendations(demo.GCPScanID, demo.GCPRecommendationSteps(), now)
				if bErr != nil {
					if h.logger != nil {
						h.logger.Error("gcp demo recommendations: plan step marshal failed", zap.Error(bErr))
					}
					return nil, &scanner.HumanizedError{
						Code:    "PlanStepMarshalFailed",
						Message: "Squadron could not encode the demo plan step. The error has been logged.",
					}, http.StatusInternalServerError
				}
				return marshalRecResult(awsGenerateRecommendationsResponse{Recommendations: recs})
			})
			c.JSON(http.StatusAccepted, recommendationJobAcceptedResponse{
				JobID:  job.ID,
				Status: string(RecJobPending),
			})
			return
		}
	}

	if h.aiProposer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": &scanner.HumanizedError{
			Code:    "AIProposerNotWired",
			Message: "Squadron's AI assist is not configured. Set ANTHROPIC_API_KEY and ai.enabled=true to enable discovery recommendations.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, gcpconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No GCP connection exists with that ID. Connect the project from the wizard first.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("gcp generate recommendations: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "StoreReadFailed",
			Message: "Squadron could not read the connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	var req gcpGenerateRecommendationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON. Re-run the scan and retry.",
		}})
		return
	}
	if strings.TrimSpace(req.ScanResult.ScanID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanID",
			Message: "scan_result.scan_id is required. Re-run the scan and retry.",
		}})
		return
	}
	if pid := strings.TrimSpace(req.ScanResult.ProjectID); pid != "" && pid != conn.ProjectID {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "ProjectIDMismatch",
			Message: "scan_result.project_id does not match the connection. Re-run the scan against the right connection and retry.",
		}})
		return
	}

	regions := []string{}
	if r := strings.TrimSpace(req.ScanResult.Region); r != "" {
		regions = append(regions, r)
	}
	aiCtx := &ai.DiscoveryScanContext{
		ScanID:              req.ScanResult.ScanID,
		Provider:            "gcp",
		ProjectID:           conn.ProjectID,
		Regions:             regions,
		InstrumentedCount:   req.ScanResult.InstrumentedCount,
		UninstrumentedCount: req.ScanResult.UninstrumentedCount,
	}
	for _, ci := range req.ScanResult.Compute {
		aiCtx.ComputeInstances = append(aiCtx.ComputeInstances, ai.ComputeResourceCandidate{
			ResourceID:   ci.ResourceID,
			InstanceType: ci.InstanceType,
			Region:       ci.Region,
			OSFamily:     ci.OSFamily,
			HasOTel:      ci.HasOTel,
		})
	}
	for _, db := range req.ScanResult.Databases {
		aiCtx.Databases = append(aiCtx.Databases, ai.DatabaseResourceCandidate{
			ResourceID:                 db.ResourceID,
			Engine:                     db.Engine,
			EngineVersion:              db.EngineVersion,
			InstanceClass:              db.InstanceClass,
			PerformanceInsightsEnabled: db.PerformanceInsightsEnabled,
			EnhancedMonitoringEnabled:  db.EnhancedMonitoringEnabled,
			Region:                     db.Region,
			Provider:                   db.Provider,
			QueryInsightsEnabled:       db.QueryInsightsEnabled,
			SQLInsightsDiagEnabled:     db.SQLInsightsDiagEnabled,
			DatabaseManagementEnabled:  db.DatabaseManagementEnabled,
		})
	}
	for _, o := range req.ScanResult.ObjectStores {
		aiCtx.ObjectStores = append(aiCtx.ObjectStores, ai.ObjectStoreCandidate{
			ResourceID:                 o.ResourceID,
			Region:                     o.Region,
			ServerAccessLoggingEnabled: o.ServerAccessLoggingEnabled,
			Provider:                   "gcp",
		})
	}
	for _, l := range req.ScanResult.LoadBalancers {
		aiCtx.LoadBalancers = append(aiCtx.LoadBalancers, ai.LoadBalancerCandidate{
			ResourceID:        l.ResourceID,
			Name:              l.Name,
			Type:              l.Type,
			Scheme:            l.Scheme,
			AccessLogsEnabled: l.AccessLogsEnabled,
			Region:            l.Region,
			Provider:          "gcp",
		})
	}
	for _, cl := range req.ScanResult.Clusters {
		addonNames := make([]string, 0, len(cl.Addons))
		for _, a := range cl.Addons {
			if !strings.EqualFold(a.Status, "ACTIVE") {
				continue
			}
			addonNames = append(addonNames, a.Name)
		}
		aiCtx.Clusters = append(aiCtx.Clusters, ai.ClusterCandidate{
			ResourceID:          cl.ResourceID,
			Name:                cl.Name,
			KubernetesVersion:   cl.KubernetesVersion,
			ControlPlaneLogging: append([]string(nil), cl.ControlPlaneLogging...),
			AddonNames:          addonNames,
			Region:              cl.Region,
		})
	}
	aiCtx.EventSources = mapEventSourceCandidates(req.ScanResult.EventSources)

	verdictBlock, acceptedURLs, acceptedURLsByState := assembleDiscoveryVerdictBlock(
		c.Request.Context(), h.acceptedAssembler, conn.ProjectID, firstRegion(regions), h.logger)
	aiCtx.VerdictBlock = verdictBlock

	// v0.89.210 async: run the proposer in a background job (see
	// docs/proposals/async-recommendations-design.md) and return 202 +
	// a job_id the UI polls; the call can take 30s-120s+.
	job := defaultRecommendationJobStore.Create("gcp", conn.ProjectID)
	defaultRecommendationJobStore.Run(job.ID, func(ctx context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
		result, err := h.aiProposer.ProposeFromDiscoveryScan(ctx, aiCtx)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("gcp generate recommendations: proposer call failed",
					zap.Error(err), zap.String("project_id", conn.ProjectID), zap.String("scan_id", aiCtx.ScanID))
			}
			return nil, &scanner.HumanizedError{
				Code:    "ProposerCallFailed",
				Message: "Squadron's AI proposer failed: " + err.Error(),
			}, http.StatusInternalServerError
		}
		if result.Declined {
			return marshalRecResult(awsGenerateRecommendationsResponse{
				Declined:        true,
				Reason:          result.Reason,
				Recommendations: []recommendations.Recommendation{},
			})
		}

		now := time.Now().UTC()
		recs, err := buildDiscoveryRecommendations(req.ScanResult.ScanID, result.Plan.Steps, now)
		if err != nil {
			if h.logger != nil {
				h.logger.Error("gcp generate recommendations: plan step marshal failed", zap.Error(err))
			}
			return nil, &scanner.HumanizedError{
				Code:    "PlanStepMarshalFailed",
				Message: "Squadron could not encode the plan step. The error has been logged.",
			}, http.StatusInternalServerError
		}

		if h.auditService != nil {
			_ = h.auditService.Record(ctx, services.AuditEntry{
				Actor:      services.AuditActorSystem,
				EventType:  "discovery.gcp.recommendations_generated",
				TargetType: credstore.TargetTypeCloudConnection,
				TargetID:   conn.ID,
				Action:     "recommendations_generated",
				Payload: map[string]any{
					"connection_id": conn.ID,
					"project_id":    conn.ProjectID,
					"scan_id":       req.ScanResult.ScanID,
					"step_count":    len(recs),
					"tokens_in":     result.TokensIn,
					"tokens_out":    result.TokensOut,
					"model":         result.Model,
					"recorded_at":   now,
				},
			})
		}

		emitDiscoveryProposalCreated(ctx, h.auditService,
			conn.ID, conn.ProjectID, firstRegion(regions), req.ScanResult.ScanID,
			len(recs), acceptedURLs, acceptedURLsByState)

		return marshalRecResult(awsGenerateRecommendationsResponse{
			Reasoning:       result.Reasoning,
			Recommendations: recs,
		})
	})

	c.JSON(http.StatusAccepted, recommendationJobAcceptedResponse{
		JobID:  job.ID,
		Status: string(RecJobPending),
	})
}

// --- helpers ------------------------------------------------------------

// validateGCPServiceAccountJSON does a small structural check on the
// pasted SA JSON. Per design doc §12 Q1, the wizard rejects a paste
// whose client_email doesn't end in .iam.gserviceaccount.com — that
// catches the operator who pasted the wrong file (e.g. an OAuth
// client secret JSON has a similar shape but lives in a different
// suffix). The function deliberately tolerates extra fields and
// missing optional ones; the only hard requirements are
//   - the bytes parse as JSON
//   - a non-empty type field that says "service_account"
//   - a client_email ending in the SA suffix
//
// project_id presence is NOT checked here — design doc §11.3
// handles project_id mismatch downstream at validate time.
func validateGCPServiceAccountJSON(saJSON []byte) error {
	var parsed struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(saJSON, &parsed); err != nil {
		return errors.New("Service Account JSON is not valid JSON. Re-download the key file and try again.")
	}
	if strings.TrimSpace(parsed.Type) == "" {
		return errors.New("Service Account JSON is missing the required \"type\" field. Re-download the key file from the GCP console.")
	}
	switch parsed.Type {
	case "service_account":
		// Downloadable SA key: keep the client_email shape checks so a
		// truncated or wrong-project key is caught early.
		if strings.TrimSpace(parsed.ClientEmail) == "" {
			return errors.New("service_account credential is missing the required \"client_email\" field. Re-download the key file from the GCP console.")
		}
		if !strings.HasSuffix(parsed.ClientEmail, gcpSAClientEmailSuffix) {
			return errors.New("service_account client_email does not end in " + gcpSAClientEmailSuffix + ". Verify you exported a key from the correct project.")
		}
	case "external_account", "impersonated_service_account", "authorized_user":
		// Keyless / federated credentials (Workload Identity Federation,
		// SA impersonation, or gcloud ADC). They carry no client_email;
		// the Go credential loader validates the type-specific fields
		// at token-mint time, so accept the shape here. This is the path
		// for tenants where constraints/iam.disableServiceAccountKey
		// Creation forbids downloadable keys.
	default:
		return errors.New("Credential JSON \"type\" is \"" + parsed.Type + "\", which the GCP connector does not support. Use service_account, external_account (Workload Identity Federation), impersonated_service_account, or authorized_user.")
	}
	return nil
}

// extractGCPSAProjectID parses the SA JSON and returns the
// project_id field, or "" + error if the field is absent /
// unparseable. Used by the validate flow's design-doc §11.3
// cross-check.
func extractGCPSAProjectID(saJSON []byte) (string, error) {
	var parsed struct {
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal(saJSON, &parsed); err != nil {
		return "", err
	}
	return strings.TrimSpace(parsed.ProjectID), nil
}

// classifyGCPScanError maps a raw scanner error into one of the
// error_kind strings the validate / scan_failed audit consumers
// pattern-match against. Per design doc §6.1 the kinds are:
// permission_denied, project_not_found, credentials_invalid,
// network, and the catch-all "unknown". The classifier reads the
// stringified error; the chunk-2 GCP scanner is expected to wrap
// google-cloud-go errors with enough surface for these substring
// checks, but the classifier tolerates anything.
func classifyGCPScanError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "403") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "permissiondenied") || strings.Contains(msg, "forbidden"):
		return "permission_denied"
	case strings.Contains(msg, "404") || strings.Contains(msg, "project not found") || strings.Contains(msg, "notfound") || strings.Contains(msg, "not found"):
		return "project_not_found"
	case strings.Contains(msg, "oauth") || strings.Contains(msg, "sign") || strings.Contains(msg, "invalid_grant") || strings.Contains(msg, "credential"):
		return "credentials_invalid"
	case strings.Contains(msg, "dial") || strings.Contains(msg, "timeout") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "network"):
		return "network"
	default:
		return "unknown"
	}
}
