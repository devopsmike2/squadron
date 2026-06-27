// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
)

// invokeScanHandler is the slice-3b synthetic-context dispatch the GCP/Azure/OCI
// schedulers use to drive their existing scan handlers off the HTTP path. These
// cover its contract: the :id param reaches the handler, a 2xx is success, and a
// >=400 status surfaces as an error the scheduler counts.

func TestInvokeScanHandler_PlumbsParamAndSuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var gotID string
	err := invokeScanHandler(context.Background(), "conn-42", func(c *gin.Context) {
		gotID = c.Param("id")
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	if err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
	if gotID != "conn-42" {
		t.Errorf("handler saw id=%q, want conn-42", gotID)
	}
}

func TestInvokeScanHandler_ErrorStatusSurfaces(t *testing.T) {
	gin.SetMode(gin.TestMode)
	err := invokeScanHandler(context.Background(), "conn-x", func(c *gin.Context) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})
	})
	if err == nil {
		t.Fatal("expected an error for a 500 status")
	}
}

func TestInvokeScanHandler_PropagatesContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type ctxKey string
	k := ctxKey("k")
	ctx := context.WithValue(context.Background(), k, "v")
	var saw any
	_ = invokeScanHandler(ctx, "c1", func(c *gin.Context) {
		saw = c.Request.Context().Value(k)
		c.Status(http.StatusOK)
	})
	if saw != "v" {
		t.Errorf("handler context did not carry the scheduler ctx value, got %v", saw)
	}
}
