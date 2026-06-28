// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

// Object-store tier — Cloud Storage (GCS) bucket walk. Coverage-parity
// arc slice 1 (docs/proposals/cloud-coverage-parity.md). Mirrors the
// AWS S3 surface: a bucket counts as instrumented when usage/access
// logging is configured (Logging.LogBucket set), matching the S3
// ServerAccessLogging axis on scanner.ObjectStoreSnapshot.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	storage "google.golang.org/api/storage/v1"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// StorageReadOnlyScope is the narrowest OAuth scope that read-lists GCS
// buckets + their logging config. The runbook documents
// roles/storage.legacyBucketReader (or roles/viewer) as the IAM grant.
const StorageReadOnlyScope = "https://www.googleapis.com/auth/devstorage.read_only"

// ServiceIDGCS is the object-store-tier service identifier reported
// against Result.FailedServices on a non-fatal GCS error.
const ServiceIDGCS = "gcs"

// buildStorageClient constructs the GCS API client. Production passes
// the oauth client; tests pass nil and the function reads s.httpClient
// with WithoutAuthentication (mirrors buildRunClient).
func (s *Scanner) buildStorageClient(ctx context.Context, oauthClient *http.Client) (*storage.Service, error) {
	if s.httpClient != nil {
		opts := []option.ClientOption{
			option.WithHTTPClient(s.httpClient),
			option.WithoutAuthentication(),
		}
		if s.endpoint != "" {
			opts = append(opts, option.WithEndpoint(s.endpoint))
		}
		return storage.NewService(ctx, opts...)
	}
	return storage.NewService(ctx, option.WithHTTPClient(oauthClient))
}

// walkGCS lists Cloud Storage buckets in the project, projects each
// into a scanner.ObjectStoreSnapshot, and appends to
// result.ObjectStores. List failures are returned for the caller to
// record as a partial-failure entry against ServiceIDGCS.
func (s *Scanner) walkGCS(ctx context.Context, client *storage.Service, result *scanner.Result) error {
	call := client.Buckets.List(s.ProjectID)
	return call.Pages(ctx, func(page *storage.Buckets) error {
		for _, b := range page.Items {
			if b == nil {
				continue
			}
			snap := scanner.ObjectStoreSnapshot{
				ResourceID: b.Name,
				Region:     strings.ToLower(b.Location),
				Tags:       b.Labels,
			}
			// GCS usage/storage logging → the S3 ServerAccessLogging
			// equivalent. Logging.LogBucket non-empty means logs are
			// delivered to a target bucket.
			if b.Logging != nil && b.Logging.LogBucket != "" {
				snap.ServerAccessLoggingEnabled = true
			}
			result.ObjectStores = append(result.ObjectStores, snap)
		}
		return nil
	})
}

// classifyGCSListError maps a GCS list error into the operator-visible
// PartialReason string (mirrors classifyCloudFunctionsListError).
func classifyGCSListError(err error) string {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		switch ge.Code {
		case http.StatusForbidden, http.StatusUnauthorized:
			return fmt.Sprintf("%s: permission denied (grant the service account roles/storage.legacyBucketReader or roles/viewer)", ServiceIDGCS)
		case http.StatusNotFound:
			return fmt.Sprintf("%s: project not found (verify project_id is correct)", ServiceIDGCS)
		case http.StatusTooManyRequests:
			return fmt.Sprintf("%s: rate limit exceeded mid-scan", ServiceIDGCS)
		}
		return fmt.Sprintf("%s: bucket list failed (HTTP %d): %s", ServiceIDGCS, ge.Code, truncate(ge.Message, 200))
	}
	return fmt.Sprintf("%s: network error: %s", ServiceIDGCS, truncate(err.Error(), 200))
}
