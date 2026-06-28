// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func doTFImportRequest(h *DiscoveryHandlers, accountID, body string) *httptest.ResponseRecorder {
	r := gin.New()
	r.POST("/api/v1/discovery/aws/connections/:id/terraform-import", h.HandleAWSGenerateTerraformImport)
	req := httptest.NewRequest(http.MethodPost,
		"/api/v1/discovery/aws/connections/"+accountID+"/terraform-import",
		bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleAWSGenerateTerraformImport_RendersImportBlocks(t *testing.T) {
	h := NewDiscoveryHandlers(&spyStore{}, zap.NewNop())
	body := `{"scan_result":{
		"account_id":"111111111111",
		"provider":"aws",
		"regions":["us-east-1"],
		"compute":[{"resource_id":"i-0abc","region":"us-east-1"}],
		"object_stores":[{"resource_id":"my-logs-bucket","region":"us-east-1"}],
		"functions":[],"databases":[],"load_balancers":[]
	}}`
	w := doTFImportRequest(h, "111111111111", body)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp awsTerraformImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.BlockCount != 2 {
		t.Errorf("block_count = %d, want 2", resp.BlockCount)
	}
	for _, want := range []string{
		"terraform plan -generate-config-out=generated.tf",
		`to = aws_instance.imported_i_0abc`,
		`id = "i-0abc"`,
		`to = aws_s3_bucket.imported_my_logs_bucket`,
		`id = "my-logs-bucket"`,
	} {
		if !strings.Contains(resp.Terraform, want) {
			t.Errorf("terraform output missing %q:\n%s", want, resp.Terraform)
		}
	}
}

func TestHandleAWSGenerateTerraformImport_MissingAccountID_400(t *testing.T) {
	h := NewDiscoveryHandlers(&spyStore{}, zap.NewNop())
	w := doTFImportRequest(h, "x", `{"scan_result":{"provider":"aws"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

func doCloudTFImport(h *DiscoveryHandlers, provider, path, body string) *httptest.ResponseRecorder {
	r := gin.New()
	switch provider {
	case "azure":
		r.POST("/api/v1/discovery/azure/connections/:id/terraform-import", h.HandleAzureGenerateTerraformImport)
	case "gcp":
		r.POST("/api/v1/discovery/gcp/connections/:id/terraform-import", h.HandleGCPGenerateTerraformImport)
	case "oci":
		r.POST("/api/v1/discovery/oci/connections/:id/terraform-import", h.HandleOCIGenerateTerraformImport)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestHandleMultiCloudTerraformImport_Preview(t *testing.T) {
	h := NewDiscoveryHandlers(&spyStore{}, zap.NewNop())

	// Azure: ARM id -> azurerm_linux_virtual_machine.
	wa := doCloudTFImport(h, "azure", "/api/v1/discovery/azure/connections/x/terraform-import",
		`{"scan_result":{"subscription_id":"ccfabdfa-0000-0000-0000-000000000000","compute":[{"resource_id":"vm-linux","import_id":"/subscriptions/s/resourceGroups/rg/providers/Microsoft.Compute/virtualMachines/vm-linux","os_family":"linux","region":"eastus"}]}}`)
	if wa.Code != http.StatusOK || !strings.Contains(wa.Body.String(), "azurerm_linux_virtual_machine.imported_vm_linux") {
		t.Errorf("azure preview wrong: %d %s", wa.Code, wa.Body.String())
	}

	// GCP: project/zone/name -> google_compute_instance.
	wg := doCloudTFImport(h, "gcp", "/api/v1/discovery/gcp/connections/x/terraform-import",
		`{"scan_result":{"project_id":"proj-abc","compute":[{"resource_id":"gce-1","import_id":"proj-abc/us-central1-a/gce-1","region":"us-central1"}]}}`)
	if wg.Code != http.StatusOK || !strings.Contains(wg.Body.String(), `proj-abc/us-central1-a/gce-1`) || !strings.Contains(wg.Body.String(), "google_compute_instance") {
		t.Errorf("gcp preview wrong: %d %s", wg.Code, wg.Body.String())
	}

	// OCI: OCID -> oci_core_instance.
	wo := doCloudTFImport(h, "oci", "/api/v1/discovery/oci/connections/x/terraform-import",
		`{"scan_result":{"tenancy_ocid":"ocid1.tenancy.oc1..aaaa","compute":[{"resource_id":"vm","import_id":"ocid1.instance.oc1..xyz","region":"us-ashburn-1"}]}}`)
	if wo.Code != http.StatusOK || !strings.Contains(wo.Body.String(), "oci_core_instance") || !strings.Contains(wo.Body.String(), "ocid1.instance.oc1..xyz") {
		t.Errorf("oci preview wrong: %d %s", wo.Code, wo.Body.String())
	}
}

func TestHandleMultiCloudTerraformImport_MissingScopeID(t *testing.T) {
	h := NewDiscoveryHandlers(&spyStore{}, zap.NewNop())
	w := doCloudTFImport(h, "gcp", "/api/v1/discovery/gcp/connections/x/terraform-import", `{"scan_result":{"compute":[]}}`)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing project_id, got %d", w.Code)
	}
}
