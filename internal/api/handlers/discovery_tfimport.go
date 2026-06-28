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

	blocks, skipped := tfimport.Generate(resources)
	c.JSON(http.StatusOK, awsTerraformImportResponse{
		Terraform:  tfimport.Render(blocks, skipped),
		BlockCount: len(blocks),
		Skipped:    skipped,
	})
}
