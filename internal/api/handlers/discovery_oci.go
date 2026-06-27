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
	"github.com/devopsmike2/squadron/internal/discovery/ociconnstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/recommendations"
	"github.com/devopsmike2/squadron/internal/services"
)

// OCIScannerFactory builds a scanner.Scanner for a given OCI
// connection. The factory accepts the unsealed API Signing Key
// private key (PEM-encoded RSA bytes) alongside the connection row
// so the production wire (chunk-2 boundary) can construct an
// *oci.Scanner without the handler ever importing the chunk-2 oci
// package directly. Tests substitute a fakeOCIScannerFactory that
// returns a pre-seeded fake scanner.
//
// The factory is per-request because the private key is unsealed
// per-request (it's never held in memory across calls); production
// implementations may close over the credstore.Key the handler does
// not see.
//
// See docs/proposals/oci-discovery-slice1.md §13 contract item 5
// for the boundary rationale: chunks 2 and 3 ship in parallel
// worktrees, so the handler depends on the provider-agnostic
// scanner.Scanner interface and main.go composes the concrete
// *oci.Scanner at startup. Mirrors the AzureScannerFactory
// (v0.89.52) shape one-for-one.
type OCIScannerFactory interface {
	Build(conn ociconnstore.OCIConnection, privateKey []byte) (scanner.Scanner, error)
}

// ociTenancyOCIDPattern matches the OCI tenancy OCID shape
// (ocid1.tenancy.oc1..<unique_id>). Per design doc §8 step 1 the
// wizard validates against this shape; the handler enforces the
// same rule so a wizard-bypassing API caller can't inject a
// malformed value. OCI SDK calls construct signed REST URLs from
// this value, so the server-side check is defense-in-depth against
// URL-path tampering as well as operator paste error.
var ociTenancyOCIDPattern = regexp.MustCompile(`^ocid1\.tenancy\.oc1\..+`)

// ociUserOCIDPattern matches the OCI user OCID shape
// (ocid1.user.oc1..<unique_id>). Same rationale as
// ociTenancyOCIDPattern.
var ociUserOCIDPattern = regexp.MustCompile(`^ocid1\.user\.oc1\..+`)

// ociFingerprintPattern matches the OCI API Signing Key fingerprint
// shape — pairs of hex digits joined by colons (e.g.
// "aa:bb:cc:dd:ee:ff:00:11:22:33:44:55:66:77:88:99"). The OCI
// Console returns a 16-pair MD5 fingerprint when a public key is
// uploaded; the wizard surfaces the colon-pair pattern. The handler
// enforces shape — not the exact 16-pair count — to tolerate
// future fingerprint algorithm changes without breaking the create
// path. Defense-in-depth against a wizard-bypassing API caller
// pasting an arbitrary string into the field.
var ociFingerprintPattern = regexp.MustCompile(`^[0-9a-fA-F]{2}(:[0-9a-fA-F]{2})+$`)

// ociValidateHandlerTimeout caps the OCI validate endpoint at 30s.
// Validate calls ListInstances once against the user's home
// compartment; the happy path round-trips well under a second.
// Same posture as the Azure / GCP validate timeouts — validate's
// purpose is "fast confidence check", not a full inventory walk.
const ociValidateHandlerTimeout = 30 * time.Second

// ociScanHandlerTimeout caps the OCI scan endpoint at 5 minutes —
// same upper bound as the AWS / GCP / Azure scan handlers. A
// tenancy-wide compartment walk can legitimately span minutes on a
// fleet with thousands of instances per design doc §14 Q2.
const ociScanHandlerTimeout = 5 * time.Minute

// DiscoveryOCIHandlers serves the OCI-side connector + scan +
// validate + recommendations surface — the slice-1 mirror of the
// AWS handler in discovery.go, the GCP handler in discovery_gcp.go,
// and the Azure handler in discovery_azure.go. Chunk 3 of #681
// introduces it; chunks 2 (scanner) and 5 (proposer) are in
// parallel worktrees and bolt onto the scanner.Scanner interface /
// OCIScannerFactory respectively.
//
// store is consumed by every endpoint except the recommendations
// stub. credstoreKey is consumed by Validate / Scan to unseal the
// connection's RSA private key. auditService is consumed by
// Create / Delete / Scan; Validate intentionally produces no audit
// signal (per design doc §11 + the GCP / Azure precedent: validate
// is the operator's confidence probe, lighter weight than a real
// scan). logger is required.
type DiscoveryOCIHandlers struct {
	store          ociconnstore.Store
	credstoreKey   *credstore.Key
	auditService   services.AuditService
	scannerFactory OCIScannerFactory
	// traceIndex — v0.89.77 trace integration slice 1 chunk 4.
	// Optional; see DiscoveryHandlers.traceIndex godoc for posture.
	traceIndex TraceIndexLookup
	logger     *zap.Logger
	// aiProposer — chunk 5 (v0.89.198). nil when AI assist is off;
	// HandleRecommendationsForOCIScan 503s in that case.
	aiProposer DiscoveryAIProposer
	// acceptedAssembler — parity follow-up (v0.89.199): feeds the
	// verdict few-shot block + discovery_proposal.created examples.
	// nil = cold-start empty.
	acceptedAssembler DiscoveryAcceptedRecommendationsAssembler
	// scanStore — continuous-discovery slice 2 (v0.89.251). Persists
	// completed scans + backs the history endpoints. Nil = non-persisted.
	scanStore DiscoveryScanStore
}

// NewDiscoveryOCIHandlers builds the handler struct. Optional
// dependencies are wired via the With* methods; mirror the
// NewDiscoveryAzureHandlers / WithAzureAuditService /
// WithAzureCredstoreKey shape used by the Azure surface so the
// trampoline in server.go can call them through.
func NewDiscoveryOCIHandlers(store ociconnstore.Store, logger *zap.Logger) *DiscoveryOCIHandlers {
	return &DiscoveryOCIHandlers{
		store:  store,
		logger: logger,
	}
}

// WithOCIAuditService wires the audit recorder used by Create /
// Delete / Scan. Nil leaves audit emission as a no-op; the rest of
// the surface stays unaffected.
func (h *DiscoveryOCIHandlers) WithOCIAuditService(a services.AuditService) *DiscoveryOCIHandlers {
	h.auditService = a
	return h
}

// WithOCICredstoreKey wires the credstore key used to seal the RSA
// private key at create time and unseal it at validate/scan time.
// A nil key leaves Create / Validate / Scan 500ing with a humanized
// error.
func (h *DiscoveryOCIHandlers) WithOCICredstoreKey(k *credstore.Key) *DiscoveryOCIHandlers {
	h.credstoreKey = k
	return h
}

// WithOCIScannerFactory wires the scanner factory used by Validate
// and Scan. Production wires a factory that builds *oci.Scanner
// (chunk 2 type, lives in a parallel worktree); tests substitute a
// fake that returns a pre-canned scanner.Scanner. A nil factory
// leaves Validate / Scan 500ing with a humanized error.
// WithOCITraceIndex wires the v0.89.77 trace integration slice 1
// chunk 4 traceindex lookup. Nil leaves scan responses
// un-annotated; production wires the same Index chunk 3 wired into
// the Discovery dashboard.
func (h *DiscoveryOCIHandlers) WithOCITraceIndex(idx TraceIndexLookup) *DiscoveryOCIHandlers {
	h.traceIndex = idx
	return h
}

// WithOCIScanStore wires the persisted scan-history store (slice 2).
func (h *DiscoveryOCIHandlers) WithOCIScanStore(s DiscoveryScanStore) *DiscoveryOCIHandlers {
	h.scanStore = s
	return h
}

// HandleOCIListScans — GET /api/v1/discovery/oci/connections/:id/scans.
func (h *DiscoveryOCIHandlers) HandleOCIListScans(c *gin.Context) {
	writeScanList(c, h.scanStore, h.logger, "oci", strings.TrimSpace(c.Param("id")))
}

// HandleOCIGetScan — GET /api/v1/discovery/oci/connections/:id/scans/:scanID.
func (h *DiscoveryOCIHandlers) HandleOCIGetScan(c *gin.Context) {
	writeScanDetail(c, h.scanStore, h.logger, "oci",
		strings.TrimSpace(c.Param("id")), strings.TrimSpace(c.Param("scanID")))
}

// HandleOCIScanDrift — GET /api/v1/discovery/oci/connections/:id/drift.
func (h *DiscoveryOCIHandlers) HandleOCIScanDrift(c *gin.Context) {
	writeDrift(c, h.scanStore, h.logger, "oci", strings.TrimSpace(c.Param("id")))
}

func (h *DiscoveryOCIHandlers) WithOCIScannerFactory(f OCIScannerFactory) *DiscoveryOCIHandlers {
	h.scannerFactory = f
	return h
}

// WithOCIAIProposer wires the discovery-side AI proposer used by
// HandleRecommendationsForOCIScan.
func (h *DiscoveryOCIHandlers) WithOCIAIProposer(p DiscoveryAIProposer) *DiscoveryOCIHandlers {
	h.aiProposer = p
	return h
}

// WithOCIAcceptedAssembler wires the accepted-recommendations assembler (verdict
// few-shot + discovery_proposal.created examples).
func (h *DiscoveryOCIHandlers) WithOCIAcceptedAssembler(a DiscoveryAcceptedRecommendationsAssembler) *DiscoveryOCIHandlers {
	h.acceptedAssembler = a
	return h
}

// --- Create -------------------------------------------------------------

// ociCreateConnectionRequest is the JSON wire shape the wizard
// POSTs. SealedPrivateKey carries the base64-encoded RSA private
// key (PEM) — base64 over the wire to keep the wire shape consistent
// with the Azure SealedSecret / GCP SealedSA patterns (avoids
// JSON-in-JSON escape pain for PEM bytes that include newlines and
// special characters, per design doc §7). The server base64-decodes
// then credstore-seals before storage.
type ociCreateConnectionRequest struct {
	DisplayName      string `json:"display_name"`
	TenancyOCID      string `json:"tenancy_ocid"`
	UserOCID         string `json:"user_ocid"`
	Fingerprint      string `json:"fingerprint"`
	SealedPrivateKey string `json:"sealed_private_key"`
	Region           string `json:"region"`
}

// HandleCreateOCIConnection — POST
// /api/v1/discovery/oci/connections.
//
// Per design doc §7: the body carries display_name, tenancy_ocid,
// user_ocid, fingerprint, base64-encoded sealed_private_key, and
// region. The handler:
//  1. Validates the request shape (400 on missing fields, on
//     malformed base64, on non-OCID tenancy_ocid / user_ocid values,
//     on non-colon-pair fingerprint, on empty region).
//  2. Seals the private key via credstore.SealOCIPrivateKey.
//  3. Persists the row via store.Create.
//  4. Emits discovery.oci.connection_created.
//  5. Returns 201 with the connection JSON (SealedPrivateKey is
//     suppressed by the json:"-" tag — never appears in the
//     response).
func (h *DiscoveryOCIHandlers) HandleCreateOCIConnection(c *gin.Context) {
	var req ociCreateConnectionRequest
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
	if strings.TrimSpace(req.TenancyOCID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingTenancyOCID",
			Message:       "OCI tenancy OCID is required. Paste the value from Step 1 of the OCI wizard.",
			SuggestedStep: "tenancy-ocid",
		}})
		return
	}
	if !ociTenancyOCIDPattern.MatchString(strings.TrimSpace(req.TenancyOCID)) {
		// Per design doc §8 step 1 the tenancy_ocid must conform to
		// the canonical OCI tenancy OCID shape
		// (ocid1.tenancy.oc1..<unique_id>). Pastes that include
		// surrounding whitespace are tolerated; pastes that include
		// unrelated characters fail this check.
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidTenancyOCID",
			Message:       "OCI tenancy OCID does not match the required format (ocid1.tenancy.oc1..<unique_id>). Verify the value in the OCI Console under Tenancy details.",
			SuggestedStep: "tenancy-ocid",
		}})
		return
	}
	if strings.TrimSpace(req.UserOCID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingUserOCID",
			Message:       "OCI user OCID is required. Paste the value from Step 1 of the OCI wizard.",
			SuggestedStep: "user-ocid",
		}})
		return
	}
	if !ociUserOCIDPattern.MatchString(strings.TrimSpace(req.UserOCID)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidUserOCID",
			Message:       "OCI user OCID does not match the required format (ocid1.user.oc1..<unique_id>). Verify the value in the OCI Console under Identity → Users.",
			SuggestedStep: "user-ocid",
		}})
		return
	}
	if strings.TrimSpace(req.Fingerprint) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingFingerprint",
			Message:       "OCI API Signing Key fingerprint is required. Paste the fingerprint the OCI Console returned when you uploaded the public key.",
			SuggestedStep: "fingerprint",
		}})
		return
	}
	if !ociFingerprintPattern.MatchString(strings.TrimSpace(req.Fingerprint)) {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidFingerprint",
			Message:       "OCI API Signing Key fingerprint does not match the required colon-separated hex-pair format (e.g. aa:bb:cc:...). Verify you pasted the value the OCI Console displayed under API Keys.",
			SuggestedStep: "fingerprint",
		}})
		return
	}
	if strings.TrimSpace(req.Region) == "" {
		// Per design doc §5: OCI requires Region always — unlike
		// AWS / GCP / Azure which allow empty Region for "scan all".
		// OCI's API endpoints are regional, so the scanner must know
		// which region to query. Slice 1 ships single-region per
		// connection.
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingRegion",
			Message:       "OCI region is required (e.g. us-phoenix-1). Unlike AWS/GCP/Azure, OCI's API endpoints are regional so the scanner must know which region to query.",
			SuggestedStep: "region",
		}})
		return
	}
	if strings.TrimSpace(req.SealedPrivateKey) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "MissingPrivateKey",
			Message:       "OCI API Signing Key private key is required. Paste the contents of oci_api_key.pem (including BEGIN/END PRIVATE KEY markers) into Step 4 of the OCI wizard.",
			SuggestedStep: "private-key",
		}})
		return
	}
	privateKey, err := base64.StdEncoding.DecodeString(req.SealedPrivateKey)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "InvalidPrivateKeyBase64",
			Message:       "OCI API Signing Key private key payload is not valid base64. The wizard should encode the key before submission; check the client-side encoder.",
			SuggestedStep: "private-key",
		}})
		return
	}
	if len(privateKey) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:          "EmptyPrivateKey",
			Message:       "OCI API Signing Key private key decoded to zero bytes. Verify the wizard's base64 encoder ran on a non-empty input.",
			SuggestedStep: "private-key",
		}})
		return
	}

	if h.store == nil {
		// Belt-and-braces: the trampoline already 503s when the store
		// is nil. Surface as 500 for the struct-literal construction
		// path.
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "OCIStoreNotWired",
			Message:       "Squadron's OCI connection substrate isn't configured.",
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

	sealed, err := credstore.SealOCIPrivateKey(h.credstoreKey, privateKey)
	if err != nil {
		if h.logger != nil {
			h.logger.Error("oci create connection: private_key seal failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "PrivateKeyEncryptFailed",
			Message:       "Squadron could not encrypt the OCI API Signing Key private key. Verify SQUADRON_SECRETS_KEY is set and retry.",
			SuggestedStep: "save",
		}})
		return
	}

	conn := &ociconnstore.OCIConnection{
		DisplayName:                      strings.TrimSpace(req.DisplayName),
		TenancyOCID:                      strings.TrimSpace(req.TenancyOCID),
		UserOCID:                         strings.TrimSpace(req.UserOCID),
		Fingerprint:                      strings.TrimSpace(req.Fingerprint),
		SealedPrivateKey:                 sealed,
		Region:                           strings.TrimSpace(req.Region),
		LearnFromAcceptedRecommendations: true,
	}
	if err := h.store.Create(c.Request.Context(), conn); err != nil {
		if h.logger != nil {
			h.logger.Error("oci create connection: store write failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:          "OCIStoreWriteFailed",
			Message:       "Squadron could not persist the OCI connection. The error has been logged; retry in a moment.",
			SuggestedStep: "save",
		}})
		return
	}

	// Audit emit. The SealedPrivateKey blob is NEVER in the payload
	// — both the json:"-" tag and the explicit payload shape below
	// enforce this. Private key bytes are the strongest credential
	// type Squadron handles; never-log/never-embed-in-audit is a
	// non-negotiable invariant. Mirrors the AWS / GCP / Azure
	// surfaces' no-credential-in-payload posture.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryOCIConnectionCreated,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "created",
			Payload: map[string]any{
				"connection_id": conn.ID,
				"display_name":  conn.DisplayName,
				"tenancy_ocid":  conn.TenancyOCID,
				"user_ocid":     conn.UserOCID,
				"fingerprint":   conn.Fingerprint,
				"region":        conn.Region,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}

	// The OCIConnection's SealedPrivateKey field carries json:"-",
	// so the marshaled response naturally omits the sealed bytes —
	// the design-doc §15 acceptance test 1 invariant. Test asserts
	// against the response body.
	c.JSON(http.StatusCreated, conn)
}

// --- List ---------------------------------------------------------------

// ociListConnectionsResponse is the wire shape the OCI discovery
// page fetches. Empty array (NOT null) when no rows — matches the
// AWS / GCP / Azure counterparts' posture so the UI's empty-state
// branch is a single .length check.
type ociListConnectionsResponse struct {
	Connections []*ociconnstore.OCIConnection `json:"connections"`
}

// HandleListOCIConnections — GET
// /api/v1/discovery/oci/connections.
//
// Returns every stored OCI connection. SealedPrivateKey stays out
// of the response by way of the json:"-" tag on the field. Empty
// store returns {"connections": []} with 200, not 404.
func (h *DiscoveryOCIHandlers) HandleListOCIConnections(c *gin.Context) {
	if h.store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}
	conns, err := h.store.List(c.Request.Context())
	if err != nil {
		if h.logger != nil {
			h.logger.Error("oci list connections: store read failed", zap.Error(err))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection list. The error has been logged; retry in a moment.",
		}})
		return
	}
	if conns == nil {
		conns = []*ociconnstore.OCIConnection{}
	}
	c.JSON(http.StatusOK, ociListConnectionsResponse{Connections: conns})
}

// --- Get ----------------------------------------------------------------

// HandleGetOCIConnection — GET
// /api/v1/discovery/oci/connections/:id.
//
// Returns the single OCI connection row identified by :id.
// SealedPrivateKey stays out of the response by way of the
// json:"-" tag on the field. 404 when no row matches.
func (h *DiscoveryOCIHandlers) HandleGetOCIConnection(c *gin.Context) {
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
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}
	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci get connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection. The error has been logged; retry in a moment.",
		}})
		return
	}
	c.JSON(http.StatusOK, conn)
}

// --- Update -------------------------------------------------------------

// ociUpdateConnectionRequest is the JSON wire shape the PATCH
// endpoint accepts. All fields are pointers so the handler can
// distinguish "omitted, preserve existing" from "explicit empty
// value". DisplayName "" via pointer would zero out the row —
// slice 1 rejects that case at validation time but the wire shape
// allows it for parity with future slices.
type ociUpdateConnectionRequest struct {
	DisplayName                      *string `json:"display_name,omitempty"`
	LearnFromAcceptedRecommendations *bool   `json:"learn_from_accepted_recommendations,omitempty"`
}

// HandleUpdateOCIConnection — PATCH
// /api/v1/discovery/oci/connections/:id.
//
// PATCH semantics: only fields explicitly present in the body are
// updated. TenancyOCID + UserOCID + Fingerprint + SealedPrivateKey
// + Region are NEVER mutated by this endpoint — rotation is delete
// + re-create, matching the substrate's design-doc §5 posture.
//
// Returns 200 with the updated connection JSON. 404 when no row
// matches; 400 on a malformed body or an explicit empty
// DisplayName.
func (h *DiscoveryOCIHandlers) HandleUpdateOCIConnection(c *gin.Context) {
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
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}
	var req ociUpdateConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}

	existing, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci update connection: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection. The error has been logged; retry in a moment.",
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
	if req.LearnFromAcceptedRecommendations != nil {
		existing.LearnFromAcceptedRecommendations = *req.LearnFromAcceptedRecommendations
	}
	// SealedPrivateKey stays nil/empty so the substrate's Update
	// preserves the stored sealed bytes per its documented contract.
	// TenancyOCID / UserOCID / Fingerprint / Region round-trip from
	// the previously read row so the substrate's Update validation
	// passes; their values are NOT mutated.
	updatePayload := &ociconnstore.OCIConnection{
		ID:                               existing.ID,
		DisplayName:                      existing.DisplayName,
		TenancyOCID:                      existing.TenancyOCID,
		UserOCID:                         existing.UserOCID,
		Fingerprint:                      existing.Fingerprint,
		Region:                           existing.Region,
		LearnFromAcceptedRecommendations: existing.LearnFromAcceptedRecommendations,
	}
	if err := h.store.Update(c.Request.Context(), updatePayload); err != nil {
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci update connection: store write failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreWriteFailed",
			Message: "Squadron could not persist the OCI connection update. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Re-read to pick up the substrate-stamped UpdatedAt + the
	// preserved SealedPrivateKey (which we'll suppress on marshal
	// via the json:"-" tag).
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

// HandleDeleteOCIConnection — DELETE
// /api/v1/discovery/oci/connections/:id.
//
// Emits discovery.oci.connection_deleted. The substrate's Delete is
// idempotent (deleting a missing row is not an error) so the handler
// returns 204 in both the "row existed" and "row already gone" cases.
func (h *DiscoveryOCIHandlers) HandleDeleteOCIConnection(c *gin.Context) {
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
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
		}})
		return
	}

	// Look up tenancy_ocid + region before delete so the audit
	// payload can carry them. A missing row produces empty strings
	// — the substrate's idempotent delete still fires.
	tenancyOCID := ""
	region := ""
	if existing, err := h.store.Get(c.Request.Context(), id); err == nil && existing != nil {
		tenancyOCID = existing.TenancyOCID
		region = existing.Region
	}

	if err := h.store.Delete(c.Request.Context(), id); err != nil {
		if h.logger != nil {
			h.logger.Error("oci delete connection: store delete failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreDeleteFailed",
			Message: "Squadron could not delete the OCI connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryOCIConnectionDeleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   id,
			Action:     "deleted",
			Payload: map[string]any{
				"connection_id": id,
				"tenancy_ocid":  tenancyOCID,
				"region":        region,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}
	c.Status(http.StatusNoContent)
}

// --- Validate -----------------------------------------------------------

// ociValidateResponse is the wire shape the wizard's "Validate"
// button renders. The success path carries instance_count; failures
// carry error_kind + a humanized message. Mirrors design doc §7.1.
type ociValidateResponse struct {
	OK            bool   `json:"ok"`
	InstanceCount int    `json:"instance_count,omitempty"`
	ErrorKind     string `json:"error_kind,omitempty"`
	Message       string `json:"message,omitempty"`
}

// HandleValidateOCIConnection — POST
// /api/v1/discovery/oci/connections/:id/validate.
//
// Per design doc §7.1: unseals the stored RSA private key, builds a
// Scanner via the factory, and calls Scan with a short timeout.
// The endpoint produces NO audit signal — validate is the operator's
// lightweight confidence probe, not a real scan. Mirrors the GCP /
// Azure precedent.
func (h *DiscoveryOCIHandlers) HandleValidateOCIConnection(c *gin.Context) {
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
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
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
			Code:    "OCIScannerNotWired",
			Message: "Squadron's OCI scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci validate: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	privateKey, err := credstore.UnsealOCIPrivateKey(h.credstoreKey, conn.SealedPrivateKey)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("oci validate: private_key unseal failed", zap.Error(err), zap.String("id", id))
		}
		// Surface as an operator-recoverable validate result rather
		// than a 500 — the operator can re-paste the key via delete
		// + re-create. The error_kind is private_key_invalid because
		// the cipher rejected the blob.
		c.JSON(http.StatusOK, ociValidateResponse{
			OK:        false,
			ErrorKind: "private_key_invalid",
			Message:   "Squadron could not decrypt the stored API Signing Key private key. Delete the connection and re-paste the credentials.",
		})
		return
	}

	scn, err := h.scannerFactory.Build(*conn, privateKey)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("oci validate: scanner build failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusOK, ociValidateResponse{
			OK:        false,
			ErrorKind: classifyOCIScanError(err),
			Message:   "Squadron could not initialize the OCI scanner: " + err.Error(),
		})
		return
	}

	// Tight timeout to keep validate snappy — the design doc's §7.1
	// calls validate "the operator's confidence check"; a validate
	// that hangs for minutes defeats the purpose.
	scanCtx, cancel := context.WithTimeout(c.Request.Context(), ociValidateHandlerTimeout)
	defer cancel()
	regions := []string{}
	if strings.TrimSpace(conn.Region) != "" {
		regions = append(regions, conn.Region)
	}
	// Validate uses the scanner.Scanner interface but does NOT
	// require a *credstore.CloudConnection — chunk-2's OCI scanner
	// satisfies the interface with a wrapper that pulls the
	// tenancy_ocid + user_ocid + fingerprint + private_key + region
	// out of its constructor closure. We pass nil here; the chunk-2
	// scanner ignores the conn arg per its construction (which
	// captured the OCI credential set at Build time).
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		c.JSON(http.StatusOK, ociValidateResponse{
			OK:        false,
			ErrorKind: classifyOCIScanError(err),
			Message:   "OCI scan probe failed: " + err.Error(),
		})
		return
	}
	if result == nil {
		c.JSON(http.StatusOK, ociValidateResponse{
			OK:            true,
			InstanceCount: 0,
		})
		return
	}
	c.JSON(http.StatusOK, ociValidateResponse{
		OK:            true,
		InstanceCount: len(result.Compute),
	})
}

// --- Scan ---------------------------------------------------------------

// ociScanResponse is the wire shape the scan endpoint returns. Wraps
// the scanner.Result with a few connection-level fields so the UI
// doesn't need to round-trip back to the connection row to render
// the inventory panel.
//
// v0.89.66 (#695 Stream 93, database tier slice 2 chunk 5) — adds
// the Databases field carrying the OCI DB System + Autonomous
// Database inventory the chunk 4 scanner extension populates. The
// omitempty tag preserves the cold-start wire shape for handlers
// that ran before the chunk 4 scanner extension (the Databases
// slice is nil on those paths).
//
// v0.89.71 (#702 Stream 100, Kubernetes tier slice 2 chunk 5) —
// adds the Clusters field carrying the OKE cluster inventory the
// v0.89.70 chunk 4 OKE scanner populates on result.Clusters. The
// omitempty tag preserves the cold-start wire shape for handlers
// that ran before the v0.89.70 scanner extension (the Clusters
// slice is nil on those paths).
type ociScanResponse struct {
	ConnectionID        string                             `json:"connection_id"`
	TenancyOCID         string                             `json:"tenancy_ocid"`
	Region              string                             `json:"region"`
	Compute             []scanner.ComputeInstanceSnapshot  `json:"compute"`
	Databases           []scanner.DatabaseInstanceSnapshot `json:"databases,omitempty"`
	Clusters            []scanner.ClusterSnapshot          `json:"clusters,omitempty"`
	InstrumentedCount   int                                `json:"instrumented_count"`
	UninstrumentedCount int                                `json:"uninstrumented_count"`
	Partial             bool                               `json:"partial"`
	PartialReason       string                             `json:"partial_reason,omitempty"`
	FailedServices      []string                           `json:"failed_services,omitempty"`
	ScanID              string                             `json:"scan_id"`
	EventSources        []eventSourceRow                   `json:"event_sources,omitempty"`
}

// HandleScanOCIConnection — POST
// /api/v1/discovery/oci/connections/:id/scan.
//
// Per design doc §7 + §15 acceptance test 8: emits
// discovery.oci.scan_started, builds a Scanner via the factory,
// calls Scan with a 5-minute timeout, emits
// discovery.oci.scan_completed (with per-category counts + the
// partial flag) on success or discovery.oci.scan_failed on a hard
// error.
func (h *DiscoveryOCIHandlers) HandleScanOCIConnection(c *gin.Context) {
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
			Code:    "OCIStoreNotWired",
			Message: "Squadron's OCI connection substrate isn't configured.",
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
			Code:    "OCIScannerNotWired",
			Message: "Squadron's OCI scanner factory isn't configured.",
		}})
		return
	}

	conn, err := h.store.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci scan: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	// Demo mode (v0.89.245, first-user onboarding): the reserved demo tenancy
	// serves a canned sample inventory. Short-circuit after the store read (the
	// row is real) but before any private-key decrypt or scanner build — no OCI
	// credentials, no cloud calls.
	if demo.IsOCIDemoTenancy(conn.TenancyOCID) {
		r := demo.OCIResult()
		instr, uninstr := 0, 0
		for _, ci := range r.Compute {
			if ci.HasOTel {
				instr++
			} else {
				uninstr++
			}
		}
		c.JSON(http.StatusOK, ociScanResponse{
			ConnectionID:        conn.ID,
			TenancyOCID:         conn.TenancyOCID,
			Region:              conn.Region,
			Compute:             r.Compute,
			Databases:           r.Databases,
			Clusters:            r.Clusters,
			InstrumentedCount:   instr,
			UninstrumentedCount: uninstr,
			Partial:             false,
			ScanID:              r.ScanID,
		})
		return
	}

	// scan_started fires BEFORE any scanner call so a forensic
	// reader can correlate a stranded scan_started (no matching
	// scan_completed / scan_failed) with an unhandled crash.
	// Mirrors the AWS / GCP / Azure scan handler invariant.
	if h.auditService != nil {
		_ = h.auditService.Record(c.Request.Context(), services.AuditEntry{
			Actor:      services.AuditActorSystem,
			EventType:  services.AuditEventDiscoveryOCIScanStarted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_started",
			Payload: map[string]any{
				"connection_id": conn.ID,
				"tenancy_ocid":  conn.TenancyOCID,
				"user_ocid":     conn.UserOCID,
				"region":        conn.Region,
				"recorded_at":   time.Now().UTC(),
			},
		})
	}

	privateKey, err := credstore.UnsealOCIPrivateKey(h.credstoreKey, conn.SealedPrivateKey)
	if err != nil {
		h.emitOCIScanFailed(c.Request.Context(), conn, "", "private_key_invalid",
			"Squadron could not decrypt the stored API Signing Key private key. Delete the connection and re-paste the credentials.",
		)
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "PrivateKeyDecryptFailed",
			Message: "Squadron could not decrypt the stored API Signing Key private key.",
		}})
		return
	}

	scn, err := h.scannerFactory.Build(*conn, privateKey)
	if err != nil {
		kind := classifyOCIScanError(err)
		h.emitOCIScanFailed(c.Request.Context(), conn, "", kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIScannerBuildFailed",
			Message: "Squadron could not initialize the OCI scanner: " + err.Error(),
		}})
		return
	}

	scanCtx, cancel := context.WithTimeout(c.Request.Context(), ociScanHandlerTimeout)
	defer cancel()
	regions := []string{}
	if strings.TrimSpace(conn.Region) != "" {
		regions = append(regions, conn.Region)
	}
	result, err := scn.Scan(scanCtx, nil, regions)
	if err != nil {
		kind := classifyOCIScanError(err)
		scanID := ""
		if result != nil {
			scanID = result.ScanID
		}
		h.emitOCIScanFailed(c.Request.Context(), conn, scanID, kind, err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIScanFailed",
			Message: "OCI scan failed: " + err.Error(),
		}})
		return
	}
	if result == nil {
		// Degenerate path — the contract says a non-error return
		// carries a non-nil Result. Surface as a 500 with audit so a
		// future regression is visible.
		h.emitOCIScanFailed(c.Request.Context(), conn, "", "unknown", "scanner returned nil result")
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIScanNilResult",
			Message: "OCI scanner returned an empty result.",
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
			"tenancy_ocid":         conn.TenancyOCID,
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
			EventType:  services.AuditEventDiscoveryOCIScanCompleted,
			TargetType: credstore.TargetTypeCloudConnection,
			TargetID:   conn.ID,
			Action:     "scan_completed",
			Payload:    payload,
		})
	}

	// Trace integration slice 1 chunk 4 (v0.89.77) — annotate the
	// per-resource last_seen_at in-place against the traceindex
	// before the response is serialized. The scope_id projection
	// uses the OCI tenancy_ocid per design doc §6.
	if h.traceIndex != nil {
		AnnotateComputeWithLastSeen(c.Request.Context(), h.traceIndex, "oci", conn.TenancyOCID, result.Compute, h.logger)
		AnnotateDatabaseWithLastSeen(c.Request.Context(), h.traceIndex, "oci", conn.TenancyOCID, result.Databases, h.logger)
		AnnotateClusterWithLastSeen(c.Request.Context(), h.traceIndex, "oci", conn.TenancyOCID, result.Clusters, h.logger)
	}

	// Event-source tier (v0.89.195) — gated dispatch mirroring the AWS
	// + GCP + Azure paths. When the tier list includes event_source
	// (the default set does) and the OCI scanner implements
	// EventSourceDiscoveryScanner, walk Streaming event sources and fold
	// them into the result so the response surfaces them with the
	// slice-2 propagation axis. The OCI Inventory tab's Event-sources
	// sub-tab already renders scan.event_sources.
	var scanReq struct {
		Tiers []string `json:"tiers,omitempty"`
	}
	_ = c.ShouldBindJSON(&scanReq)
	if tierListContains(parseTiersOrDefault(scanReq.Tiers), TierEventSource) {
		if esScanner, ok := scn.(EventSourceDiscoveryScanner); ok {
			esOut, esErr := esScanner.ScanEventSources(scanCtx, scanner.ScanScope{
				AccountID: conn.TenancyOCID,
				Regions:   regions,
			})
			if esErr != nil {
				if h.logger != nil {
					h.logger.Warn("oci scan: event source scan failed",
						zap.Error(esErr), zap.String("tenancy_ocid", conn.TenancyOCID))
				}
			}
			if len(esOut) > 0 {
				result.EventSources = append(result.EventSources, esOut...)
			}
		}
	}

	resp := ociScanResponse{
		ConnectionID:        conn.ID,
		TenancyOCID:         conn.TenancyOCID,
		Region:              conn.Region,
		Compute:             result.Compute,
		Databases:           result.Databases,
		Clusters:            result.Clusters,
		InstrumentedCount:   instrumentedCount,
		UninstrumentedCount: uninstrumentedCount,
		Partial:             result.Partial,
		PartialReason:       result.PartialReason,
		FailedServices:      result.FailedServices,
		ScanID:              result.ScanID,
		EventSources:        marshalEventSourceRows(result.EventSources),
	}
	// slice 2 (v0.89.251) — persist the completed scan (best-effort). Scope
	// is the route :id (connection ID). Demo path returned earlier.
	if h.scanStore != nil {
		if rj, err := json.Marshal(resp); err == nil {
			recordScan(c.Request.Context(), h.scanStore, h.logger, "oci", conn.ID, result, rj)
		} else if h.logger != nil {
			h.logger.Warn("oci scan: marshal for persistence failed",
				zap.Error(err), zap.String("scan_id", result.ScanID))
		}
	}
	c.JSON(http.StatusOK, resp)
}

// emitOCIScanFailed records a discovery.oci.scan_failed audit event
// with the supplied connection + error metadata. Safe to call when
// auditService is nil. Payload NEVER carries the plaintext RSA
// private key or the sealed bytes — the substrate's
// no-credential-in-audit invariant extends to the OCI path.
func (h *DiscoveryOCIHandlers) emitOCIScanFailed(ctx context.Context, conn *ociconnstore.OCIConnection, scanID, errorKind, message string) {
	if h.auditService == nil || conn == nil {
		return
	}
	payload := map[string]any{
		"connection_id":     conn.ID,
		"tenancy_ocid":      conn.TenancyOCID,
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
		EventType:  services.AuditEventDiscoveryOCIScanFailed,
		TargetType: credstore.TargetTypeCloudConnection,
		TargetID:   conn.ID,
		Action:     "scan_failed",
		Payload:    payload,
	})
}

// --- Recommendations stub -----------------------------------------------

// HandleRecommendationsForOCIScan — POST
// /api/v1/discovery/oci/connections/:id/recommendations.
//
// Chunk 3 of #681 ships this as a 501 NotImplemented stub. Chunk 5
// of #681 (the proposer integration) wires the real path — adding
// the Provider="oci" path on DiscoveryScanContext, the
// compute-otel-tag recommendation kind, branch encoding compute-
// prefix detection, and the ListDiscoveryVerdicts fourth OR-match.
// Until then, the route returns a humanized "coming in chunk 5"
// message so the UI can render an empty Recommendations tab without
// 404ing.
// ociGenerateRecommendationsRequest is the POST body: the
// ociScanResponse echoed back so the proposer reasons over the same
// inventory the Inventory tab rendered.
type ociGenerateRecommendationsRequest struct {
	ScanResult ociScanResponse `json:"scan_result"`
}

// HandleRecommendationsForOCIScan — POST
// /api/v1/discovery/oci/connections/:id/recommendations (chunk 5,
// v0.89.198). Builds a Provider="oci" DiscoveryScanContext from the
// posted scan result and runs the shared proposer, mirroring AWS / GCP
// / Azure.
func (h *DiscoveryOCIHandlers) HandleRecommendationsForOCIScan(c *gin.Context) {
	id := strings.TrimSpace(c.Param("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingConnectionID",
			Message: "Connection ID path parameter is required.",
		}})
		return
	}
	// Demo mode (v0.89.245): the reserved demo tenancy serves seeded
	// recommendations through the same buildDiscoveryRecommendations walk —
	// no LLM, no API key. Resolve the connection early so the demo is detected
	// before the aiProposer wiring check (a keyless first-user has nil).
	if h.store != nil {
		if conn, err := h.store.Get(c.Request.Context(), id); err == nil && conn != nil && demo.IsOCIDemoTenancy(conn.TenancyOCID) {
			job := defaultRecommendationJobStore.Create("oci", conn.TenancyOCID)
			defaultRecommendationJobStore.Run(job.ID, func(_ context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
				now := time.Now().UTC()
				recs, bErr := buildDiscoveryRecommendations(demo.OCIScanID, demo.OCIRecommendationSteps(), now)
				if bErr != nil {
					if h.logger != nil {
						h.logger.Error("oci demo recommendations: plan step marshal failed", zap.Error(bErr))
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
		if errors.Is(err, ociconnstore.ErrConnectionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": &scanner.HumanizedError{
				Code:    "ConnectionNotFound",
				Message: "No OCI connection exists with that ID. Connect the tenancy from the wizard first.",
			}})
			return
		}
		if h.logger != nil {
			h.logger.Error("oci generate recommendations: store read failed", zap.Error(err), zap.String("id", id))
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": &scanner.HumanizedError{
			Code:    "OCIStoreReadFailed",
			Message: "Squadron could not read the OCI connection. The error has been logged; retry in a moment.",
		}})
		return
	}

	var req ociGenerateRecommendationsRequest
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
	if tid := strings.TrimSpace(req.ScanResult.TenancyOCID); tid != "" && tid != conn.TenancyOCID {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "TenancyOCIDMismatch",
			Message: "scan_result.tenancy_ocid does not match the connection. Re-run the scan against the right connection and retry.",
		}})
		return
	}

	regions := []string{}
	if r := strings.TrimSpace(req.ScanResult.Region); r != "" {
		regions = append(regions, r)
	}
	aiCtx := &ai.DiscoveryScanContext{
		ScanID:              req.ScanResult.ScanID,
		Provider:            "oci",
		TenancyOCID:         conn.TenancyOCID,
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
		c.Request.Context(), h.acceptedAssembler, conn.TenancyOCID, firstRegion(regions), h.logger)
	aiCtx.VerdictBlock = verdictBlock

	// v0.89.210 async: run the proposer in a background job (see
	// docs/proposals/async-recommendations-design.md) and return 202 +
	// a job_id the UI polls; the call can take 30s-120s+.
	job := defaultRecommendationJobStore.Create("oci", conn.TenancyOCID)
	defaultRecommendationJobStore.Run(job.ID, func(ctx context.Context) (json.RawMessage, *scanner.HumanizedError, int) {
		result, err := h.aiProposer.ProposeFromDiscoveryScan(ctx, aiCtx)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("oci generate recommendations: proposer call failed",
					zap.Error(err), zap.String("tenancy_ocid", conn.TenancyOCID), zap.String("scan_id", aiCtx.ScanID))
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
				h.logger.Error("oci generate recommendations: plan step marshal failed", zap.Error(err))
			}
			return nil, &scanner.HumanizedError{
				Code:    "PlanStepMarshalFailed",
				Message: "Squadron could not encode the plan step. The error has been logged.",
			}, http.StatusInternalServerError
		}

		if h.auditService != nil {
			_ = h.auditService.Record(ctx, services.AuditEntry{
				Actor:      services.AuditActorSystem,
				EventType:  "discovery.oci.recommendations_generated",
				TargetType: credstore.TargetTypeCloudConnection,
				TargetID:   conn.ID,
				Action:     "recommendations_generated",
				Payload: map[string]any{
					"connection_id": conn.ID,
					"tenancy_ocid":  conn.TenancyOCID,
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
			conn.ID, conn.TenancyOCID, firstRegion(regions), req.ScanResult.ScanID,
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

// classifyOCIScanError maps a raw scanner error into one of the
// error_kind strings the validate / scan_failed audit consumers
// pattern-match against. Per design doc §7.1 the kinds are:
// permission_denied (403, NotAuthorizedOrNotFound),
// tenancy_not_found (404 on the tenancy lookup),
// fingerprint_mismatch (401 with key/fingerprint/signature wording —
// OCI's auth layer surfaces the fingerprint as the discriminating
// signal), private_key_invalid (local PEM/RSA parse error before
// the request), network (transport-level), and the catch-all
// "unknown".
//
// The ordering of substring checks matters. OCI's
// "NotAuthorizedOrNotFound" response body is the canonical
// permission-denied marker and contains "not found"; we want
// permission_denied for that case, not tenancy_not_found. So the
// permission_denied check runs BEFORE the 404 / not-found bucket.
// Likewise fingerprint_mismatch (a 401-shaped auth failure)
// classifies before the generic 404 path so an auth failure that
// mentions both "fingerprint" and a 404-shaped tail still lands as
// fingerprint_mismatch.
func classifyOCIScanError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "notauthorizedornotfound") || strings.Contains(msg, "403") || strings.Contains(msg, "permission denied") || strings.Contains(msg, "permission_denied") || strings.Contains(msg, "forbidden"):
		return "permission_denied"
	case strings.Contains(msg, "fingerprint") || strings.Contains(msg, "signature") || strings.Contains(msg, "invalidsignature") || strings.Contains(msg, "401") || strings.Contains(msg, "unauthorized"):
		return "fingerprint_mismatch"
	case strings.Contains(msg, "pem") || strings.Contains(msg, "rsa") || strings.Contains(msg, "private key") || strings.Contains(msg, "private_key") || strings.Contains(msg, "parse"):
		return "private_key_invalid"
	case strings.Contains(msg, "tenancy") && (strings.Contains(msg, "not found") || strings.Contains(msg, "tenancy_not_found") || strings.Contains(msg, "invalid")):
		return "tenancy_not_found"
	case strings.Contains(msg, "404") || strings.Contains(msg, "not found"):
		return "tenancy_not_found"
	case strings.Contains(msg, "dial") || strings.Contains(msg, "timeout") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") || strings.Contains(msg, "network"):
		return "network"
	default:
		return "unknown"
	}
}
