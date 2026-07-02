// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
	"github.com/devopsmike2/squadron/internal/iac/tfimport"
)

// awsTerraformImportResponse is the wire shape of the preview endpoint:
// the rendered import-block .tf plus the count and any skipped resources
// (categories/types without a safe import mapping yet).
type awsTerraformImportResponse struct {
	Terraform  string             `json:"terraform"`
	BlockCount int                `json:"block_count"`
	Skipped    []tfimport.Skipped `json:"skipped,omitempty"`
}

// HandleAWSGenerateTerraformImport renders Terraform import{} blocks for
// the resources in a scan result (env -> Terraform arc, slice 1). It is
// synchronous + deterministic — no LLM, no cloud calls — so it returns
// the rendered HCL directly. Slice 2 adds dedup + PR delivery.
func (h *DiscoveryHandlers) HandleAWSGenerateTerraformImport(c *gin.Context) {
	var req awsGenerateRecommendationsRequest // reuses {scan_result}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Message: "Request body could not be parsed as JSON.",
		}})
		return
	}
	sr := req.ScanResult
	if sr.AccountID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{
			Code:    "MissingScanAccountID",
			Message: "scan_result.account_id is required.",
		}})
		return
	}

	blocks, skipped := tfimport.Generate(awsScanToImportResources(sr))
	c.JSON(http.StatusOK, awsTerraformImportResponse{
		Terraform:  tfimport.Render(blocks, skipped),
		BlockCount: len(blocks),
		Skipped:    skipped,
	})
}

// awsScanToImportResources maps an AWS scan response onto the
// provider-agnostic tfimport.Resource shape (shared by the preview
// endpoint and the import-PR endpoint).
func awsScanToImportResources(sr awsScanResponse) []tfimport.Resource {
	var resources []tfimport.Resource
	for _, r := range sr.Compute {
		resources = append(resources, tfimport.Resource{Provider: "aws", Category: "compute", ResourceID: r.ResourceID, Region: r.Region})
	}
	for _, r := range sr.ObjectStores {
		resources = append(resources, tfimport.Resource{Provider: "aws", Category: "object_store", ResourceID: r.ResourceID, Region: r.Region})
	}
	for _, r := range sr.Functions {
		resources = append(resources, tfimport.Resource{Provider: "aws", Category: "function", ResourceID: r.ResourceID, Name: r.Name, Region: r.Region})
	}
	for _, r := range sr.Databases {
		resources = append(resources, tfimport.Resource{Provider: "aws", Category: "database", ResourceID: r.ResourceID, Region: r.Region})
	}
	for _, r := range sr.LoadBalancers {
		resources = append(resources, tfimport.Resource{Provider: "aws", Category: "load_balancer", ResourceID: r.ResourceID, Name: r.Name, Region: r.Region})
	}
	return resources
}

// computeSnapshotsToImportResources maps the canonical ComputeInstanceSnapshot
// list (shared by the Azure/GCP/OCI scan responses) onto tfimport.Resource,
// carrying the scanner-captured ImportID + OSFamily so the multi-cloud
// mappers can emit correct import blocks.
func computeSnapshotsToImportResources(provider string, comp []scanner.ComputeInstanceSnapshot) []tfimport.Resource {
	out := make([]tfimport.Resource, 0, len(comp))
	for _, c := range comp {
		out = append(out, tfimport.Resource{
			Provider:   provider,
			Category:   "compute",
			ResourceID: c.ResourceID,
			// Name drives the generated TF address label; use the
			// operator-readable name so addresses read imported_<name>
			// rather than the long canonical ImportID.
			Name:     c.ResourceID,
			ImportID: c.ImportID,
			OSFamily: c.OSFamily,
			Region:   c.Region,
		})
	}
	return out
}

// nonComputeToImportResources maps the database / object-store / load-balancer
// snapshots (shared by the Azure/GCP/OCI scan responses) onto tfimport.Resource.
// Like compute, the snapshot ResourceID is an operator-readable NAME (not a
// valid terraform import id), so the canonical id rides on the separate
// scanner-enriched ImportID; the mapper skips when ImportID is empty rather than
// guess. Categories/clouds without an ImportID or a mapper surface as a visible
// Skipped instead of being silently dropped (which is what happened when these
// were omitted from the handler entirely).
func nonComputeToImportResources(
	provider string,
	dbs []scanner.DatabaseInstanceSnapshot,
	objs []scanner.ObjectStoreSnapshot,
	lbs []scanner.LoadBalancerSnapshot,
) []tfimport.Resource {
	out := make([]tfimport.Resource, 0, len(dbs)+len(objs)+len(lbs))
	for _, d := range dbs {
		out = append(out, tfimport.Resource{
			Provider: provider, Category: "database",
			// Name drives the generated TF address label; the snapshot
			// ResourceID is the operator-readable instance name, so the
			// block reads imported_<name> rather than the raw import id.
			ResourceID: d.ResourceID, Name: d.ResourceID, ImportID: d.ImportID, Region: d.Region,
		})
	}
	for _, o := range objs {
		// Object stores carry no canonical terraform import id yet (blob/GCS/S3
		// import formats differ and aren't ARM ids); ImportID stays empty so
		// they surface as a visible Skipped until a mapper + enrichment land.
		out = append(out, tfimport.Resource{
			Provider: provider, Category: "object_store",
			ResourceID: o.ResourceID, Region: o.Region,
		})
	}
	for _, l := range lbs {
		out = append(out, tfimport.Resource{
			Provider: provider, Category: "load_balancer",
			ResourceID: l.ResourceID, ImportID: l.ImportID, Name: l.Name, Region: l.Region,
		})
	}
	return out
}

// renderImportResponse runs the generator + writes the standard response.
func renderImportResponse(c *gin.Context, resources []tfimport.Resource) {
	blocks, skipped := tfimport.Generate(resources)
	c.JSON(http.StatusOK, awsTerraformImportResponse{
		Terraform:  tfimport.Render(blocks, skipped),
		BlockCount: len(blocks),
		Skipped:    skipped,
	})
}

// HandleAzureGenerateTerraformImport — env->TF slice 3c preview for Azure.
func (h *DiscoveryHandlers) HandleAzureGenerateTerraformImport(c *gin.Context) {
	var req azureGenerateRecommendationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Message: "Request body could not be parsed as JSON."}})
		return
	}
	if req.ScanResult.SubscriptionID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Code: "MissingSubscriptionID", Message: "scan_result.subscription_id is required."}})
		return
	}
	res := computeSnapshotsToImportResources("azure", req.ScanResult.Compute)
	res = append(res, nonComputeToImportResources("azure", req.ScanResult.Databases, req.ScanResult.ObjectStores, req.ScanResult.LoadBalancers)...)
	renderImportResponse(c, res)
}

// HandleGCPGenerateTerraformImport — env->TF slice 3c preview for GCP.
func (h *DiscoveryHandlers) HandleGCPGenerateTerraformImport(c *gin.Context) {
	var req gcpGenerateRecommendationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Message: "Request body could not be parsed as JSON."}})
		return
	}
	if req.ScanResult.ProjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Code: "MissingProjectID", Message: "scan_result.project_id is required."}})
		return
	}
	res := computeSnapshotsToImportResources("gcp", req.ScanResult.Compute)
	res = append(res, nonComputeToImportResources("gcp", req.ScanResult.Databases, req.ScanResult.ObjectStores, req.ScanResult.LoadBalancers)...)
	renderImportResponse(c, res)
}

// HandleOCIGenerateTerraformImport — env->TF slice 3c preview for OCI.
func (h *DiscoveryHandlers) HandleOCIGenerateTerraformImport(c *gin.Context) {
	var req ociGenerateRecommendationsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Message: "Request body could not be parsed as JSON."}})
		return
	}
	if req.ScanResult.TenancyOCID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": &scanner.HumanizedError{Code: "MissingTenancyOCID", Message: "scan_result.tenancy_ocid is required."}})
		return
	}
	res := computeSnapshotsToImportResources("oci", req.ScanResult.Compute)
	res = append(res, nonComputeToImportResources("oci", req.ScanResult.Databases, req.ScanResult.ObjectStores, req.ScanResult.LoadBalancers)...)
	renderImportResponse(c, res)
}
