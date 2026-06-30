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
	"github.com/devopsmike2/squadron/internal/discovery/azureconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/demo"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
)

// AzureScannerFactory builds a scanner.Scanner for a given Azure
// connection. The factory accepts the unsealed Service Principal
// client_secret bytes alongside the connection row so the production
// wire (chunk-2 boundary) can construct an *azure.Scanner without the
// handler ever importing the chunk-2 azure package directly. Tests
// substitute a fakeAzureScannerFactory that returns a pre-seeded fake
// scanner.
//
// The factory is per-request because the client_secret is unsealed
// per-request (it's never held in memory across calls); production
// implementations may close over the credstore.Key the handler does
// not see.
//
// See docs/proposals/azure-discovery-slice1.md §13 contract item 7
// for the boundary rationale: chunks 2 and 3 ship in parallel
// worktrees, so the handler depends on the provider-agnostic
// scanner.Scanner interface and main.go composes the concrete
// *azure.Scanner at startup. Mirrors the GCPScannerFactory
// (v0.89.47) shape one-for-one.
type AzureScannerFactory interface {
	Build(conn azureconnstore.AzureConnection, clientSecret []byte) (scanner.Scanner, error)
}

// azureUUIDPattern matches the canonical Azure UUID shape with
// hyphens (lower or upper case). Per design doc §8 the wizard
// validates tenant_id, subscription_id, and client_id as UUIDs; the
// handler enforces the same rule so a wizard-bypassing API caller
// can't inject malformed values. Azure SDK calls construct ARM URLs
// from these values, so the server-side check is defense-in-depth
// against URL-path tampering as well as operator paste error.
var azureUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// azureValidateHandlerTimeout caps the Azure validate endpoint at
// 30s. Validate calls VirtualMachinesClient.NewListAllPager once
// against the configured subscription; the happy path round-trips
// well under a second. The shorter-than-AWS budget mirrors the GCP
// validate posture — validate's purpose is "fast confidence check",
// not a full inventory walk.
const azureValidateHandlerTimeout = 30 * time.Second

// azureScanHandlerTimeout caps the Azure scan endpoint at 5 minutes
// — same upper bound as the AWS / GCP scan handlers. A
// subscription-wide VM walk can legitimately span minutes on a
// fleet with thousands of instances per design doc §14 Q3.
const azureScanHandlerTimeout = 5 * time.Minute

// DiscoveryAzureHandlers serves the Azure-side connector + scan +
// validate + recommendations surface — the slice-1 mirror of the
// AWS handler in discovery.go and the GCP handler in
// discovery_gcp.go. Chunk 3 of #674 introduces it; chunks 2
// (scanner) and 5 (proposer) are in parallel worktrees and bolt
// onto the scanner.Scanner interface / AzureScannerFactory
// respectively.
//
// store is consumed by every endpoint except the recommendations
// stub. credstoreKey is consumed by Validate / Scan to unseal the
// connection's SP client_secret. auditService is consumed by
// Create / Delete / Scan; Validate intentionally produces no audit
// signal (per design doc §11 + the GCP precedent: validate is the
// operator's confidence probe, lighter weight than a real scan).
// logger is required.
type DiscoveryAzureHandlers struct {
	store          azureconnstore.Store
	credstoreKey   *credstore.Key
	auditService   services.AuditService
	scannerFactory AzureScannerFactory
	// traceIndex — v0.89.77 trace integration slice 1 chunk 4.
	// Optional; see DiscoveryHandlers.traceIndex godoc for posture.
	traceIndex TraceIndexLookup
	logger     *zap.Logger
	// aiProposer — chunk 5 (v0.89.198). nil when AI assist is off;
	// HandleRecommendationsForAzureScan 503s in that case.
	aiProposer DiscoveryAIProposer
	// acceptedAssembler — parity follow-up (v0.89.199): feeds the
	// verdict few-shot block + discovery_proposal.created examples.
	// nil = cold-start empty.
	acceptedAssembler DiscoveryAcceptedRecommendationsAssembler
	// scanStore — continuous-discovery slice 2 (v0.89.251). Persists
	// completed scans + backs the history endpoints. Nil = non-persisted.
	scanStore DiscoveryScanStore
	// Regression-recommendation stores (detection→proposal). Optional;
	// nil short-circuits the corresponding pass. Azure Functions
	// cold-start + error-rate are commercial-tier (App Insights); the
	// recs only fire once those detectors annotated a prior scan.
	coldStartStore ColdStartObservationReader
	errorRateStore ErrorRateObservationStore
	exclusionStore DiscoveryExclusionStore

	// coldStartConstants pins the cold-start annotation thresholds so the
	// Azure scan handler can run AnnotateServerlessWithColdStart on the
	// serverless rows (parity with AWS). Azure cold-start/error-rate data
	// is App Insights (commercial)-sourced, so the rows populate only when
	// that add-on is on; nil-store-safe → "—" otherwise.
	coldStartConstants ColdStartAnnotationThresholds
}

// WithAzureRegressionStores wires the regression-recommendation stores (any may
// be nil). Returns the receiver for chaining.
func (h *DiscoveryAzureHandlers) WithAzureRegressionStores(
	coldStart ColdStartObservationReader, errorRate ErrorRateObservationStore, exclusions DiscoveryExclusionStore,
) *DiscoveryAzureHandlers {
	h.coldStartStore = coldStart
	h.errorRateStore = errorRate
	h.exclusionStore = exclusions
	return h
}

// WithAzureColdStartConstants pins the cold-start annotation thresholds so
// the Azure scan handler populates cold_start_p95_ms +
// cold_start_exceeds_threshold on the serverless rows (parity with AWS).
// Nil leaves the cold-start annotation a no-op ("—").
func (h *DiscoveryAzureHandlers) WithAzureColdStartConstants(thresholds ColdStartAnnotationThresholds) *DiscoveryAzureHandlers {
	h.coldStartConstants = thresholds
	return h
}

// NewDiscoveryAzureHandlers builds the handler struct. Optional
// dependencies are wired via the With* methods; mirror the
// NewDiscoveryGCPHandlers / WithGCPAuditService / WithGCPCredstoreKey
// shape used by the GCP surface so the trampoline in server.go can
// call them through.
func NewDiscoveryAzureHandlers(store azureconnstore.Store, logger *zap.Logger) *DiscoveryAzureHandlers {
	return &DiscoveryAzureHandlers{
		store:  store,
		logger: logger,
	}
}

// WithAzureAuditService wires the audit recorder used by Create /
// Delete / Scan. Nil leaves audit emission as a no-op; the rest of
// the surface stays unaffected.
func (h *DiscoveryAzureHandlers) WithAzureAuditService(a services.AuditService) *DiscoveryAzureHandlers {
	h.auditService = a
	return h
}

// WithAzureCredstoreKey wires the credstore key used to seal the SP
// client_secret at create time and unseal it at validate/scan time.
// A nil key leaves Create / Validate / Scan 500ing with a humanized
// error.
func (h *DiscoveryAzureHandlers) WithAzureCredstoreKey(k *credstore.Key) *DiscoveryAzureHandlers {
	h.credstoreKey = k
	return h
}

// WithAzureScannerFactory wires the scanner factory used by Validate
// and Scan. Production wires a factory that builds *azure.Scanner
// (chunk 2 type, lives in a parallel worktree); tests substitute a
// fake that returns a pre-canned scanner.Scanner. A nil factory
// leaves Validate / Scan 500ing with a humanized error.
// WithAzureTraceIndex wires the v0.89.77 trace integration slice 1
// chunk 4 traceindex lookup. Nil leaves scan responses
// un-annotated; production wires the same Index chunk 3 wired into
// the Discovery dashboard.
func (h *DiscoveryAzureHandlers) WithAzureTraceIndex(idx TraceIndexLookup) *DiscoveryAzureHandlers {
	h.traceIndex = idx
	return h
}

// WithAzureScanStore wires the persisted scan-history store (slice 2).
func (h *DiscoveryAzureHandlers) WithAzureScanStore(s DiscoveryScanStore) *DiscoveryAzureHandlers {
	h.scanStore = s
	return h
}

// HandleAzureListScans — GET /api/v1/discovery/azure/connections/:id/scans.
func (h *DiscoveryAzureHandlers) HandleAzureListScans(c *gin.Context) {
	writeScanList(c, h.scanStore, h.logger, "azure", strings.TrimSpace(c.Param("id")))
}

// HandleAzureGetScan — GET /api/v1/discovery/azure/connections/:id/scans/:scanID.
func (h *DiscoveryAzureHandlers) HandleAzureGetScan(c *gin.Context) {
	writeScanDetail(c, h.scanStore, h.logger, "azure",
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(c.Param("scanID")))
}

// HandleAzureScanDrift — GET /api/v1/discovery/azure/connections/:id/drift.
func (h *DiscoveryAzureHandlers) HandleAzureScanDrift(c *gin.Context) {
	writeDrift(c, h.scanStore, h.logger, "azure", strings.TrimSpace(c.Param("id")))
}

func (h *DiscoveryAzureHandlers) WithAzureScannerFactory(f AzureScannerFactory) *DiscoveryAzureHandlers {
	h.scannerFactory = f
	return h
}

// WithAzureAIProposer wires the discovery-side AI proposer used by
// HandleRecommendationsForAzureScan.
func (h *DiscoveryAzureHandlers) WithAzureAIProposer(p DiscoveryAIProposer) *DiscoveryAzureHandlers {
	h.aiProposer = p
	return h
}

// WithAzureAcceptedAssembler wires the accepted-recommendations assembler (verdict
// few-shot + discovery_proposal.created examples).
func (h *DiscoveryAzureHandlers) WithAzureAcceptedAssembler(a DiscoveryAcceptedRecommendationsAssembler) *DiscoveryAzureHandlers {
	h.acceptedAssembler = a
	return h
}

// --- Create -------------------------------------------------------------

// azureCreateConnectionRequest is the JSON wire shape the wizard
// POSTs. SealedSecret carries the base64-encoded SP client_secret —
// base64 over the wire to keep the wizard's wire shape consistent
// with the GCP SealedSA pattern (avoids JSON-in-JSON escape pain
// for any secret bytes that happen to include special characters,
// per design doc §7). The server base64-decodes then credstore-seals
// before storage.
type azureCreateConnectionRequest struct {
	DisplayName    string `json:"display_name"`
	TenantID       string `json:"tenant_id"`
	SubscriptionID string `json:"subscription_id"`
	ClientID       string `json:"client_id"`
	SealedSecret   string `json:"sealed_secret"`
	Location       string `json:"location"`
}

// HandleCreateAzureConnection — POST
// /api/v1/discovery/azure/connections.
//
// Per design doc §7: the body carries display_name, tenant_id,
// subscription_id, client_id, base64-encoded sealed_secret, and
// location. The handler:
//  1. Validates the request shape (400 on missing fields, on
//     malformed base64, on non-UUID tenant_id / subscription_id /
//     client_id values).
//  2. Seals the client_secret via credstore.SealAzureClientSecret.
//  3. Persists the row via store.Create.
//  4. Emits discovery.azure.connection_created.
//  5. Returns 201 with the connection JSON (SealedSecret is
//     suppressed by the json:"-" tag — never appears in the
//     response).
func (h *DiscoveryAzureHandlers) HandleCreateAzureConnection(c *gin.Context) {
	var req azureCreateConnectionRequest
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
	if strings.TrimSpace(req.TenantID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingTenantID",
			Message:       "Azure tenant ID is required. Paste the value from Step 1 of the Azure wizard.",
			SuggestedStep: "tenant-id",
		}})
		return
	}
	if !azureUUIDPattern.MatchString(strings.TrimSpace(req.TenantID)) {
		// Per design doc §8 step 1 the tenant_id must conform to the
		// canonical Azure UUID shape (8-4-4-4-12 hex with hyphens).
		// Pastes that include surrounding whitespace are tolerated;
		// pastes that include unrelated characters fail this check.
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidTenantID",
			Message:       "Azure tenant ID does not match the required UUID format (8-4-4-4-12 hex with hyphens). Verify the value in the Azure portal.",
			SuggestedStep: "tenant-id",
		}})
		return
	}
	if strings.TrimSpace(req.SubscriptionID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingSubscriptionID",
			Message:       "Azure subscription ID is required. Paste the value from Step 1 of the Azure wizard.",
			SuggestedStep: "subscription-id",
		}})
		return
	}
	if !azureUUIDPattern.MatchString(strings.TrimSpace(req.SubscriptionID)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidSubscriptionID",
			Message:       "Azure subscription ID does not match the required UUID format (8-4-4-4-12 hex with hyphens). Verify the value in the Azure portal.",
			SuggestedStep: "subscription-id",
		}})
		return
	}
	if strings.TrimSpace(req.ClientID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingClientID",
			Message:       "Service Principal client ID is required. Paste the value from Step 3 of the Azure wizard (the appId field).",
			SuggestedStep: "client-id",
		}})
		return
	}
	if !azureUUIDPattern.MatchString(strings.TrimSpace(req.ClientID)) {
		// Azure SP appIds are documented UUIDs; reject non-UUID pastes
		// at the handler so the operator who pasted the displayName
		// instead of the appId gets a clean error rather than a
		// failed-auth response from the cloud later.
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidClientID",
			Message:       "Service Principal client ID does not match the required UUID format. Verify you pasted the appId from `az ad sp create-for-rbac`, not the display name.",
			SuggestedStep: "client-id",
		}})
		return
	}
	if strings.TrimSpace(req.SealedSecret) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingClientSecret",
			Message:       "Service Principal client secret is required. Paste the password value from `az ad sp create-for-rbac` into Step 3 of the Azure wizard.",
			SuggestedStep: "client-secret",
		}})
		return
	}
	clientSecret, err := base64.StdEncoding.DecodeString(req.SealedSecret)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidClientSecretBase64",
			Message:       "Service Principal client secret payload is not valid base64. The wizard should encode the secret before submission; check the client-side encoder.",
			SuggestedStep: "client-secret",
		}})
		return
	}
	if len(clientSecret) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "EmptyClientSecret",
			Message:       "Service Principal client secret decoded to zero bytes. Verify the wizard's base64 encoder ran on a non-empty input.",
			SuggestedStep: "client-secret",
		}})
		return
	}

	if h.store == nil {
		// Belt-and-braces: the trampoline already 503s when the store
		// is nil. Surface as 500 for the struct-literal construction
		// path.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "AzureStoreNotWired",
			Message:       "Squadron's Azure connection substrate isn't configured.",
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

	sealed, err := credstore.SealAzureClientSecret(h.credstoreKey, clientSecret)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("azure create connection: client_secret seal failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "ClientSecretEncryptFailed",
			Message:       "Squadron could not encrypt the Service Principal client secret. Verify SQUADRON_SECRETS_KEY is set and retry.",
			SuggestedStep: "save",
		}})
		return
	}

	conn := &azureconnstore.AzureConnection{
		DisplayName:                      strings.TrimSpace(req.DisplayName),
		TenantID:                         strings.TrimSpace(req.TenantID),
		SubscriptionID:                   strings.TrimSpace(req.SubscriptionID),
		ClientID:                         strings.TrimSpace(req.ClientID),
		SealedSecret:                     sealed,
		Location:                         strings.TrimSpace(req.Location),
		LearnFromAcceptedRecommendations: true,
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("azure create connection: store write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "AzureStoreWriteFailed",
			Message:       "Squadron could not persist the Azure connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Audit emit. The SealedSecret blob is NEVER in the payload —
	// both the json:"-" tag and the explicit payload shape below
	// enforce this. Mirrors the AWS / GCP surface's
	// no-credential-in-payload posture.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryAzureConnectionCreated,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "created",
			Payload: map[string]any{
				"connection_id":   conn.ID,
				"display_name":    conn.DisplayName,
				"tenant_id":       conn.TenantID,
				"subscription_id": conn.SubscriptionID,
				"client_id":       conn.ClientID,
				"location":        conn.Location,
				"recorded_at":     time.Now().UTC(),
			},
		})
	}

	// The AzureConnection's SealedSecret field carries json:"-", so
	// the marshaled response naturally omits the sealed bytes — the
	// design-doc §15 acceptance test 1 invariant. Test asserts
	// against the response body.
	c.JSON(http.StatusCreated, conn)
}

// --- List ---------------------------------------------------------------

// azureListConnectionsResponse is the wire shape the Azure
// discovery page fetches. Empty array (NOT null) when no rows —
// matches the AWS / GCP counterpart's posture so the UI's empty-state
// branch is a single .length check.
type azureListConnectionsResponse struct {
	Connections []*azureconnstore.AzureConnection `json:"connections"`
}

// HandleListAzureConnections — GET
// /api/v1/discovery/azure/connections.
//
// Returns every stored Azure connection. SealedSecret stays out of
// the response by way of the json:"-" tag on the field. Empty store
// returns {"connections": []} with 200, not 404.
func (h *DiscoveryAzureHandlers) HandleListAzureConnections(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("azure list connections: store read failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	if conns == nil {
		conns = []*azureconnstore.AzureConnection{}
	}
	c.JSON(http.StatusOK, azureListConnectionsResponse{Connections: conns})
}

// --- Get ----------------------------------------------------------------

// HandleGetAzureConnection — GET
// /api/v1/discovery/azure/connections/:id.
//
// Returns the single Azure connection row identified by :id.
// SealedSecret stays out of the response by way of the json:"-" tag
// on the field. 404 when no row matches.
func (h *DiscoveryAzureHandlers) HandleGetAzureConnection(c *gin.Context) {
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
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}
	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure get connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// --- Update -------------------------------------------------------------

// azureUpdateConnectionRequest is the JSON wire shape the PATCH
// endpoint accepts. All fields are pointers so the handler can
// distinguish "omitted, preserve existing" from "explicit empty
// value". DisplayName "" via pointer would zero out the row — slice
// 1 rejects that case at validation time but the wire shape allows
// it for parity with future slices.
type azureUpdateConnectionRequest struct {
	DisplayName                      *string `json:"display_name,omitempty"`
	Location                         *string `json:"location,omitempty"`
	LearnFromAcceptedRecommendations *bool   `json:"learn_from_accepted_recommendations,omitempty"`
}

// HandleUpdateAzureConnection — PATCH
// /api/v1/discovery/azure/connections/:id.
//
// PATCH semantics: only fields explicitly present in the body are
// updated. TenantID + SubscriptionID + ClientID + SealedSecret are
// NEVER mutated by this endpoint — rotation is delete + re-create,
// matching the substrate's design-doc §5 posture.
//
// Returns 200 with the updated connection JSON. 404 when no row
// matches; 400 on a malformed body or an explicit empty
// DisplayName.
func (h *DiscoveryAzureHandlers) HandleUpdateAzureConnection(c *gin.Context) {
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
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}
	var req azureUpdateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}

	existing, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure update connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Build the update payload by overlaying the patch on existing.
	// PATCH semantics: pointer-nil fields preserve the existing
	// value.
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
	if req.Location != nil {
		existing.Location = strings.TrimSpace(*req.Location)
	}
	if req.LearnFromAcceptedRecommendations != nil {
		existing.LearnFromAcceptedRecommendations = *req.LearnFromAcceptedRecommendations
	}
	// SealedSecret stays nil/empty so the substrate's Update
	// preserves the stored sealed bytes per its documented contract.
	// TenantID / SubscriptionID / ClientID round-trip from the
	// previously read row so the substrate's Update validation
	// passes; their values are NOT mutated.
	updatePayload := &azureconnstore.AzureConnection{
		ID:                               existing.ID,
		DisplayName:                      existing.DisplayName,
		TenantID:                         existing.TenantID,
		SubscriptionID:                   existing.SubscriptionID,
		ClientID:                         existing.ClientID,
		Location:                         existing.Location,
		LearnFromAcceptedRecommendations: existing.LearnFromAcceptedRecommendations,
	}
	if err := h.store.Update(c.Request.Context(), updatePayload); err != nil {
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure update connection: store write failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreWriteFailed",
			Message: "Squadron could not persist the Azure connection update. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Re-read to pick up the substrate-stamped UpdatedAt + the
	// preserved SealedSecret (which we'll suppress on marshal via
	// the json:"-" tag).
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

// HandleDeleteAzureConnection — DELETE
// /api/v1/discovery/azure/connections/:id.
//
// Emits discovery.azure.connection_deleted. The substrate's Delete
// is idempotent (deleting a missing row is not an error) so the
// handler returns 204 in both the "row existed" and "row already
// gone" cases.
func (h *DiscoveryAzureHandlers) HandleDeleteAzureConnection(c *gin.Context) {
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
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
		}})
		return
	}

	// Look up subscription_id before delete so the audit payload can
	// carry it. A missing row produces an empty subscription_id —
	// the substrate's idempotent delete still fires.
	subscriptionID := ""
	tenantID := ""
	if existing, err := h.store.Get(c.Request.Context(), id); err == nil && existing != nil {
		subscriptionID = existing.SubscriptionID
		tenantID = existing.TenantID
	}

	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		if h.logger != nil {
			h.logger.Error("azure delete connection: store delete failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreDeleteFailed",
			Message: "Squadron could not delete the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryAzureConnectionDeleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   id,
			Action:     "deleted",
			Payload: map[string]any{
				"connection_id":   id,
				"tenant_id":       tenantID,
				"subscription_id": subscriptionID,
				"recorded_at":     time.Now().UTC(),
			},
		})
	}
	c.Status(http.StatusNoContent)
}

// --- Validate -----------------------------------------------------------

// azureValidateResponse is the wire shape the wizard's "Validate"
// button renders. The success path carries instance_count; failures
// carry error_kind + a humanized message. Mirrors design doc §7.1.
type azureValidateResponse struct {
	OK            bool   `json:"ok"`
	InstanceCount int    `json:"instance_count,omitempty"`
	ErrorKind     string `json:"error_kind,omitempty"`
	Message       string `json:"message,omitempty"`
}

// HandleValidateAzureConnection — POST
// /api/v1/discovery/azure/connections/:id/validate.
//
// Per design doc §7.1: unseals the stored client_secret, builds a
// Scanner via the factory, and calls Scan with a short timeout.
// The endpoint produces NO audit signal — validate is the
// operator's lightweight confidence probe, not a real scan. Mirrors
// the GCP precedent.
//
// Slice 1 does NOT cross-check the SP's subscription_id against the
// connection row (Azure SP access tokens do not carry the
// subscription_id in their claims the way GCP SA JSON carries
// project_id); the scan's 404 response from the cloud is what
// surfaces the subscription_not_found error_kind.
func (h *DiscoveryAzureHandlers) HandleValidateAzureConnection(c *gin.Context) {
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
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
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
			Code:    "AzureScannerNotWired",
			Message: "Squadron's Azure scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure validate: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	clientSecret, err := credstore.UnsealAzureClientSecret(h.credstoreKey, conn.SealedSecret)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("azure validate: client_secret unseal failed", zap.Error(err), zap.String("id", id))
		}
		// Surface as an operator-recoverable validate result rather
		// than a 500 — the operator can re-paste the secret via
		// delete + re-create. The error_kind is credentials_invalid
		// because the cipher rejected the blob.
		c.JSON(http.StatusOK, azureValidateResponse{
			OK:        false,
			ErrorKind: "credentials_invalid",
			Message:   "Squadron could not decrypt the stored Service Principal client secret. Delete the connection and re-paste the credentials.",
		})
		return
	}

	scn, err := h.scannerFactory.Build(*conn, clientSecret)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("azure validate: scanner build failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusOK, azureValidateResponse{
			OK:        false,
			ErrorKind: classifyAzureScanError(err),
			Message:   "Squadron could not initialize the Azure scanner: " + err.Error(),
		})
		return
	}

	// Tight timeout to keep validate snappy — the design doc's
	// §7.1 calls validate "the operator's confidence check"; a
	// validate that hangs for minutes defeats the purpose.
	scanCtx, cancel := context.WithTimeout(c.Request.Context(), azureValidateHandlerTimeout)
	defer cancel()
	regions := []string{}
	if strings.TrimSpace(conn.Location) != "" {
		regions = append(regions, conn.Location)
	}
	// Validate uses the scanner.Scanner interface but does NOT
	// require a *credstore.CloudConnection — chunk-2's Azure
	// scanner satisfies the interface with a wrapper that pulls the
	// subscription_id + tenant_id + client_id + secret out of its
	// constructor closure. We pass nil here; the chunk-2 scanner
	// ignores the conn arg per its construction (which captured the
	// SP triple + location at Build time).
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		c.JSON(http.StatusOK, azureValidateResponse{
			OK:        false,
			ErrorKind: classifyAzureScanError(err),
			Message:   "Azure scan probe failed: " + err.Error(),
		})
		return
	}
	if result == nil {
		c.JSON(http.StatusOK, azureValidateResponse{
			OK:            true,
			InstanceCount: 0,
		})
		return
	}
	c.JSON(http.StatusOK, azureValidateResponse{
		OK:            true,
		InstanceCount: len(result.Compute),
	})
}

// --- Scan ---------------------------------------------------------------

// azureScanResponse is the wire shape the scan endpoint returns.
// Wraps the scanner.Result with a few connection-level fields so
// the UI doesn't need to round-trip back to the connection row to
// render the inventory panel.
//
// v0.89.66 (#695 Stream 93, database tier slice 2 chunk 5) — adds
// the Databases field carrying the Azure SQL database inventory the
// chunk 3 scanner extension populates. The omitempty tag preserves
// the cold-start wire shape for handlers that ran before the chunk
// 3 scanner extension (the Databases slice is nil on those paths).
//
// v0.89.71 (#702 Stream 100, Kubernetes tier slice 2 chunk 5) —
// adds the Clusters field carrying the AKS cluster inventory the
// v0.89.70 chunk 3 AKS scanner populates on result.Clusters. The
// omitempty tag preserves the cold-start wire shape for handlers
// that ran before the v0.89.70 scanner extension (the Clusters
// slice is nil on those paths).
type azureScanResponse struct {
	ConnectionID        string                             `json:"connection_id"`
	SubscriptionID      string                             `json:"subscription_id"`
	Location            string                             `json:"location"`
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
	Orchestrations      []awsOrchestrationRow              `json:"orchestrations,omitempty"`
	// Serverless carries the per-function rows (Azure Functions) with
	// their cold-start + error-rate detection annotations (the
	// snapshot type is marshaled directly), so both regression axes
	// round-trip into the recs request DTO — feeding the
	// detection→recommendation flow (parity with AWS).
	Serverless []scanner.ServerlessInstanceSnapshot `json:"serverless,omitempty"`
}

// HandleScanAzureConnection — POST
// /api/v1/discovery/azure/connections/:id/scan.
//
// Per design doc §7 + §15 acceptance test 7: emits
// discovery.azure.scan_started, builds a Scanner via the factory,
// calls Scan with a 5-minute timeout, emits
// discovery.azure.scan_completed (with per-category counts + the
// partial flag) on success or discovery.azure.scan_failed on a
// hard error.
func (h *DiscoveryAzureHandlers) HandleScanAzureConnection(c *gin.Context) {
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
			Code:    "AzureStoreNotWired",
			Message: "Squadron's Azure connection substrate isn't configured.",
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
			Code:    "AzureScannerNotWired",
			Message: "Squadron's Azure scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure scan: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Demo mode (v0.89.244, first-user onboarding): the reserved demo
	// subscription serves a canned sample inventory. Short-circuit after the
	// store read (the row is real) but before any client-secret decrypt or
	// scanner build — no Azure credentials, no cloud calls.
	if demo.IsAzureDemoSubscription(conn.SubscriptionID) {
		r := demo.AzureResult()
		instr, uninstr := 0, 0
		for _, ci := range r.Compute {
			if ci.HasOTel {
				instr++
			} else {
				uninstr++
			}
		}
		c.JSON(http.StatusOK, azureScanResponse{
			ConnectionID:        conn.ID,
			SubscriptionID:      conn.SubscriptionID,
			Location:            conn.Location,
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
	// scan_completed / scan_failed) with an unhandled crash.
	// Mirrors the AWS / GCP scan handler invariant.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryAzureScanStarted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_started",
			Payload: map[string]any{
				"connection_id":   conn.ID,
				"tenant_id":       conn.TenantID,
				"subscription_id": conn.SubscriptionID,
				"location":        conn.Location,
				"recorded_at":     time.Now().UTC(),
			},
		})
	}

	clientSecret, err := credstore.UnsealAzureClientSecret(h.credstoreKey, conn.SealedSecret)
	if err != nil {
		h.emitAzureScanFailed(c.Request.Context(), conn, "", "credentials_invalid",
			"Squadron could not decrypt the stored Service Principal client secret. Delete the connection and re-paste the credentials.",
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "ClientSecretDecryptFailed",
			Message: "Squadron could not decrypt the stored Service Principal client secret.",
		}})
		return
	}

	scn, err := h.scannerFactory.Build(*conn, clientSecret)
	if err != nil {
		kind := classifyAzureScanError(err)
		h.emitAzureScanFailed(c.Request.Context(), conn, "", kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureScannerBuildFailed",
			Message: "Squadron could not initialize the Azure scanner: " + err.Error(),
		}})
		return
	}

	scanCtx, cancel := context.WithTimeout(c.Request.Context(), azureScanHandlerTimeout)
	defer cancel()
	regions := []string{}
	if strings.TrimSpace(conn.Location) != "" {
		regions = append(regions, conn.Location)
	}
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		kind := classifyAzureScanError(err)
		scanID := ""
		if result != nil {
			scanID = result.ScanID
		}
		h.emitAzureScanFailed(c.Request.Context(), conn, scanID, kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureScanFailed",
			Message: "Azure scan failed: " + err.Error(),
		}})
		return
	}
	if result == nil {
		// Degenerate path — the contract says a non-error return
		// carries a non-nil Result. Surface as a 500 with audit so a
		// future regression is visible.
		h.emitAzureScanFailed(c.Request.Context(), conn, "", "unknown", "scanner returned nil result")
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureScanNilResult",
			Message: "Azure scanner returned an empty result.",
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
			"tenant_id":            conn.TenantID,
			"subscription_id":      conn.SubscriptionID,
			"location":             conn.Location,
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
			EventType:  services.AuditEventDiscoveryAzureScanCompleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_completed",
			Payload:    payload,
		})
	}

	// Trace integration slice 1 chunk 4 (v0.89.77) — annotate the
	// per-resource last_seen_at in-place against the traceindex
	// before the response is serialized. The scope_id projection
	// uses the Azure subscription_id per design doc §6.
	if h.traceIndex != nil {
		AnnotateComputeWithLastSeen(c.Request.Context(), h.traceIndex, "azure", conn.SubscriptionID, result.Compute, h.logger)
		AnnotateDatabaseWithLastSeen(c.Request.Context(), h.traceIndex, "azure", conn.SubscriptionID, result.Databases, h.logger)
		AnnotateClusterWithLastSeen(c.Request.Context(), h.traceIndex, "azure", conn.SubscriptionID, result.Clusters, h.logger)
	}

	// Serverless cold-start + error-rate annotation (parity with AWS;
	// serverless-annotation-parity arc). Project the persisted
	// cold_start_observation + error_rate_observation rows onto the Azure
	// Functions inventory rows so the UI shows cold-start latency + error
	// rate, not "—". Azure's source is App Insights (commercial), so these
	// populate only when that add-on is on; both nil-store-safe.
	if h.coldStartStore != nil && h.coldStartConstants != nil {
		AnnotateServerlessWithColdStart(c.Request.Context(), h.coldStartStore, h.coldStartConstants, result.Serverless, h.logger)
	}
	if h.errorRateStore != nil {
		AnnotateServerlessWithErrorRate(c.Request.Context(), h.errorRateStore, result.Serverless, h.logger)
	}

	// Event-source tier (v0.89.195) — gated dispatch mirroring the AWS
	// + GCP paths. When the tier list includes event_source (the default
	// set does) and the Azure scanner implements
	// EventSourceDiscoveryScanner, walk Service Bus event sources and
	// fold them into the result so the response surfaces them with the
	// slice-2 propagation axis. The Azure Inventory tab's Event-sources
	// sub-tab already renders scan.event_sources.
	var scanReq struct {
		Tiers []string `json:"tiers,omitempty"`
	}
	_ = c.ShouldBindJSON(&scanReq)
	if tierListContains(parseTiersOrDefault(scanReq.Tiers), TierEventSource) {
		if esScanner, ok := scn.(EventSourceDiscoveryScanner); ok {
			esOut, esErr := esScanner.ScanEventSources(scanCtx, scanner.ScanScope{
				AccountID: conn.SubscriptionID,
				Regions:   regions,
			})
			if esErr != nil {
				if h.logger != nil {
					h.logger.Warn("azure scan: event source scan failed",
						zap.Error(esErr), zap.String("subscription_id", conn.SubscriptionID))
				}
			}
			if len(esOut) > 0 {
				result.EventSources = append(result.EventSources, esOut...)
			}
		}
	}

	// Orchestration-tier (Logic Apps): the Azure scanner satisfies
	// OrchestrationDiscoveryScanner via the embedded chunk-2 scanner, but
	// unlike the AWS handler this one never invoked it, so Logic Apps were
	// silently dropped from every Azure scan (orchestrations always empty).
	// Mirror the event-source fold above so the Inventory tab's
	// Orchestration sub-tab populates.
	if tierListContains(parseTiersOrDefault(scanReq.Tiers), TierOrchestration) {
		if orchScanner, ok := scn.(OrchestrationDiscoveryScanner); ok {
			orchOut, orchErr := orchScanner.ScanOrchestrations(scanCtx, scanner.ScanScope{
				AccountID: conn.SubscriptionID,
				Regions:   regions,
			})
			if orchErr != nil {
				if h.logger != nil {
					h.logger.Warn("azure scan: orchestration scan failed",
						zap.Error(orchErr), zap.String("subscription_id", conn.SubscriptionID))
				}
			}
			if len(orchOut) > 0 {
				result.Orchestrations = append(result.Orchestrations, orchOut...)
			}
		}
	}

	resp := azureScanResponse{
		ConnectionID:        conn.ID,
		SubscriptionID:      conn.SubscriptionID,
		Location:            conn.Location,
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
		Orchestrations:      marshalOrchestrationRows(result.Orchestrations),
		Serverless:          result.Serverless,
	}
	// slice 2 (v0.89.251) — persist the completed scan (best-effort). Scope
	// is the route :id (connection ID). Demo path returned earlier.
	if h.scanStore != nil {
		if rj, err := json.Marshal(resp); err == nil {
			recordScan(c.Request.Context(), h.scanStore, h.logger, "azure", conn.ID, result, rj)
		} else if h.logger != nil {
			h.logger.Warn("azure scan: marshal for persistence failed",
				zap.Error(err), zap.String("scan_id", result.ScanID))
		}
	}
	c.JSON(http.StatusOK, resp)
}

// emitAzureScanFailed records a discovery.azure.scan_failed audit
// event with the supplied connection + error metadata. Safe to call
// when auditService is nil. Payload NEVER carries the plaintext SP
// client_secret or the sealed bytes — the substrate's
// no-credential-in-audit invariant extends to the Azure path.
func (h *DiscoveryAzureHandlers) emitAzureScanFailed(ctx context.Context, conn *azureconnstore.AzureConnection, scanID, errorKind, message string) {
	if h.auditService == nil || conn == nil {
		return
	}
	payload := map[string]any{
		"connection_id":     conn.ID,
		"tenant_id":         conn.TenantID,
		"subscription_id":   conn.SubscriptionID,
		"location":          conn.Location,
		"error_kind":        errorKind,
		"humanized_message": message,
		"recorded_at":       time.Now().UTC(),
	}
	if scanID != "" {
		payload["scan_id"] = scanID
	}
	_ = h.auditService.Record(ctx, services.AuditEntry{
		Actor:      services.AuditActorSystem,
		EventType:  services.AuditEventDiscoveryAzureScanFailed,
		TargetType: credstore.TargetTypeCloudConnection,
		TargetID:   conn.ID,
		Action:     "scan_failed",
		Payload:    payload,
	})
}

// --- Recommendations stub -----------------------------------------------

// HandleRecommendationsForAzureScan — POST
// /api/v1/discovery/azure/connections/:id/recommendations.
//
// Chunk 3 of #674 ships this as a 501 NotImplemented stub. Chunk 5
// of #674 (the proposer integration) wires the real path — adding
// the Provider="azure" path on DiscoveryScanContext, the
// vm-otel-tag recommendation kind, and the system-prompt
// extension. Until then, the route returns a humanized "coming in
// chunk 5" message so the UI can render an empty Recommendations
// tab without 404ing.
// azureGenerateRecommendationsRequest is the POST body: the
// azureScanResponse echoed back so the proposer reasons over the same
// inventory the Inventory tab rendered.
type azureGenerateRecommendationsRequest struct {
	ScanResult azureScanResponse `json:"scan_result"`
}

// HandleRecommendationsForAzureScan — POST
// /api/v1/discovery/azure/connections/:id/recommendations (chunk 5,
// v0.89.198). Builds a Provider="azure" DiscoveryScanContext from the
// posted scan result and runs the shared proposer, mirroring AWS + GCP.
func (h *DiscoveryAzureHandlers) HandleRecommendationsForAzureScan(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	// Demo mode (v0.89.244): the reserved demo subscription serves seeded
	// recommendations through the same buildDiscoveryRecommendations walk —
	// no LLM, no API key. Resolve the connection early so the demo is detected
	// before the aiProposer wiring check (a keyless first-user has nil).
	if h.store != nil {
		if conn, err := h.store.Get(c.Request.Context(), id); err == nil && conn != nil && demo.IsAzureDemoSubscription(conn.SubscriptionID) {
			job := defaultRecommendationJobStore.Create("azure", conn.SubscriptionID)
			defaultRecommendationJobStore.Run(job.ID, func(_ context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
				now := time.Now().UTC()
				recs, bErr := buildDiscoveryRecommendations(demo.AzureScanID, demo.AzureRecommendationSteps(), now)
				if bErr != nil {
					if h.logger != nil {
						h.logger.Error("azure demo recommendations: plan step marshal failed", zap.Error(bErr))
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
		if errors.Is(err, azureconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No Azure connection exists with that ID. Connect the subscription from the wizard first.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("azure generate recommendations: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "AzureStoreReadFailed",
			Message: "Squadron could not read the Azure connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	var req azureGenerateRecommendationsRequest
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
	if sid := strings.TrimSpace(req.ScanResult.SubscriptionID); sid != "" && sid != conn.SubscriptionID {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "SubscriptionIDMismatch",
			Message: "scan_result.subscription_id does not match the connection. Re-run the scan against the right connection and retry.",
		}})
		return
	}

	regions := []string{}
	if r := strings.TrimSpace(req.ScanResult.Location); r != "" {
		regions = append(regions, r)
	}
	aiCtx := &ai.DiscoveryScanContext{
		ScanID:              req.ScanResult.ScanID,
		Provider:            "azure",
		TenantID:            conn.TenantID,
		SubscriptionID:      conn.SubscriptionID,
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
			Provider:                   "azure",
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
			Provider:          "azure",
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
		c.Request.Context(), h.acceptedAssembler, conn.SubscriptionID, firstRegion(regions), h.logger)
	aiCtx.VerdictBlock = verdictBlock

	// v0.89.210 async: run the proposer in a background job (see
	// docs/proposals/async-recommendations-design.md) and return 202 +
	// a job_id the UI polls; the call can take 30s-120s+.
	job := defaultRecommendationJobStore.Create("azure", conn.SubscriptionID)
	defaultRecommendationJobStore.Run(job.ID, func(ctx context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
		result, err := h.aiProposer.ProposeFromDiscoveryScan(ctx, aiCtx)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("azure generate recommendations: proposer call failed",
					zap.Error(err), zap.String("subscription_id", conn.SubscriptionID), zap.String("scan_id", aiCtx.ScanID))
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
				h.logger.Error("azure generate recommendations: plan step marshal failed", zap.Error(err))
			}
			return nil, &scanner.HumanizedError{
				Code:    "PlanStepMarshalFailed",
				Message: "Squadron could not encode the plan step. The error has been logged.",
			}, http.StatusInternalServerError
		}

		// Detection → proposal: append cold-start + error-rate regression recs
		// for any Azure Functions row whose detector fired on this scan
		// (commercial-tier — App Insights). Additive + best-effort.
		appendRegressionRecs(ctx, &recs, req.ScanResult.Serverless,
			h.coldStartStore, h.errorRateStore, h.exclusionStore,
			conn.ID, conn.SubscriptionID, conn.Location, req.ScanResult.ScanID, now, h.logger)

		if h.auditService != nil {
			_ = h.auditService.Record(ctx, services.AuditEntry{
				Actor:      services.AuditActorSystem,
				EventType:  "discovery.azure.recommendations_generated",
				TargetType: credstore.TargetTypeCloudConnection,
				TargetID:   conn.ID,
				Action:     "recommendations_generated",
				Payload: map[string]any{
					"connection_id":   conn.ID,
					"subscription_id": conn.SubscriptionID,
					"scan_id":         req.ScanResult.ScanID,
					"step_count":      len(recs),
					"tokens_in":       result.TokensIn,
					"tokens_out":      result.TokensOut,
					"model":           result.Model,
					"recorded_at":     now,
				},
			})
		}

		emitDiscoveryProposalCreated(ctx, h.auditService,
			conn.ID, conn.SubscriptionID, firstRegion(regions), req.ScanResult.ScanID,
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

// classifyAzureScanError maps a raw scanner error into one of the
// error_kind strings the validate / scan_failed audit consumers
// pattern-match against. Per design doc §7.1 the kinds are:
// permission_denied (403, AuthorizationFailed),
// subscription_not_found (404), tenant_invalid (auth failure with
// tenant-related message), credentials_invalid (auth failure with
// secret/client mismatch), network, and the catch-all "unknown".
// The classifier reads the stringified error; the chunk-2 Azure
// scanner is expected to wrap azidentity / armcompute errors with
// enough surface for these substring checks, but the classifier
// tolerates anything.
//
// The ordering of substring checks matters. A "Tenant not found"
// message contains both "tenant" and "not found" — we want
// tenant_invalid, not subscription_not_found. So the tenant check
// runs BEFORE the 404 / not-found bucket. Likewise credentials_invalid
// runs after tenant_invalid so an "AADSTS Tenant not found"
// classifies as tenant_invalid rather than credentials_invalid.
func classifyAzureScanError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "authorizationfailed") || strings.Contains(msg, "403") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "forbidden"):
		return "permission_denied"
	case strings.Contains(msg, "tenant") && (strings.Contains(msg, "invalid") || strings.Contains(msg, "not found") || strings.Contains(msg, "aadsts")):
		return "tenant_invalid"
	case strings.Contains(msg, "subscriptionnotfound") || strings.Contains(msg, "subscription not found") || strings.Contains(msg, "404") || strings.Contains(msg, "not found"):
		return "subscription_not_found"
	case strings.Contains(msg, "aadsts") || strings.Contains(msg, "invalid_client") || strings.Contains(msg, "invalid client") || strings.Contains(msg, "client_secret") || strings.Contains(msg, "client secret") || strings.Contains(msg, "client assertion") || strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "credential"):
		return "credentials_invalid"
	case strings.Contains(msg, "dial") || strings.Contains(msg, "timeout") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "network"):
		return "network"
	default:
		return "unknown"
	}
}

// marshalOrchestrationRows converts scanner orchestration snapshots into
// the snake_case wire rows (shared awsOrchestrationRow shape). Added with
// the v0.89.222 fix that taught HandleScanAzureConnection to invoke
// ScanOrchestrations — the orchestration fold it had been missing, which
// left Logic Apps undiscoverable on every Azure scan.
func marshalOrchestrationRows(snaps []scanner.OrchestrationInstanceSnapshot) []awsOrchestrationRow {
	rows := make([]awsOrchestrationRow, 0, len(snaps))
	for _, oc := range snaps {
		rows = append(rows, awsOrchestrationRow{
			Provider:     oc.Provider,
			Surface:      oc.Surface,
			AccountID:    oc.AccountID,
			Region:       oc.Region,
			ResourceName: oc.ResourceName,
			ResourceARN:  oc.ResourceARN,
			WorkflowType: oc.WorkflowType,
			HasTraceAxis: oc.HasTraceAxis,
			HasLogAxis:   oc.HasLogAxis,
			LastSeenAt:   oc.LastSeenAt,
			Detail:       oc.Detail,
		})
	}
	return rows
}
