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
