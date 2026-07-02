// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package oci

// Object-store tier — OCI Object Storage buckets. Coverage-parity arc
// slice 4. Mirrors the AWS S3 surface. OCI has no per-bucket "access
// logging" boolean (object access logs are delivered via the Logging
// service), so the instrumented axis maps to the closest bucket-native
// observability flag: objectEventsEnabled (the bucket emits object
// lifecycle events to the Events service). Honest framing: this is the
// per-bucket observability signal OCI exposes inline; full access-log
// detection via the Logging service is a future enhancement.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// ServiceIDObjectStorage is the object-store-tier service identifier.
const ServiceIDObjectStorage = "ociobjectstorage"

// objectStorageListAPIVersion is unused in the path (the Object
// Storage API is unversioned in the URL) but kept for symmetry/docs.

type ociBucketSummary struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type ociBucket struct {
	Name                string                            `json:"name"`
	ObjectEventsEnabled bool                              `json:"objectEventsEnabled"`
	FreeformTags        map[string]string                 `json:"freeformTags"`
	DefinedTags         map[string]map[string]interface{} `json:"definedTags"`
}

func (s *Scanner) objectStorageEndpoint() string {
	if s.ociEndpoint != "" {
		return s.ociEndpoint
	}
	return fmt.Sprintf("https://objectstorage.%s.oraclecloud.com", s.Region)
}

// scanObjectStorage resolves the tenancy namespace, lists buckets per
// compartment, and appends an ObjectStoreSnapshot per bucket. The
// coverage axis (ServerAccessLoggingEnabled) is resolved from the OCI
// Logging service (slice 6): OCI has no inline per-bucket logging flag,
// so a bucket is covered when an OCI service log references it (the same
// listLogsForOCIResource detection the streaming/topic/queue tiers use).
// A Logging-call failure dims the axis to false and is recorded once as
// a partial failure rather than aborting the bucket walk.
// ociBucketImportID composes the terraform import id for an OCI Object
// Storage bucket: "n/<namespace>/b/<bucket>". Empty when either component is
// missing so the mapper skips rather than emit a malformed id.
func ociBucketImportID(namespace, bucket string) string {
	if namespace == "" || bucket == "" {
		return ""
	}
	return "n/" + namespace + "/b/" + bucket
}

func (s *Scanner) scanObjectStorage(ctx context.Context, sk *SigningKey, comps []ociCompartment, result *scanner.Result) {
	ns, nsErr := s.getNamespace(ctx, sk)
	if nsErr != nil {
		recordPartialFailure(result, ServiceIDObjectStorage, classifyOCITierError(ServiceIDObjectStorage, "namespace", nsErr))
		return
	}
	if ns == "" {
		return
	}
	loggingFailed := false
	for _, comp := range comps {
		buckets, listErr := s.listBuckets(ctx, sk, ns, comp.ID)
		if listErr != nil {
			reason := classifyOCITierError(ServiceIDObjectStorage, "bucket", listErr)
			if reason == "" {
				// A 404 on the per-compartment bucket LIST is meaningful
				// (we already discovered this compartment) — surface it
				// rather than silently reporting zero buckets.
				reason = fmt.Sprintf("%s: bucket list returned HTTP 404 (NotAuthorizedOrNotFound) for compartment %s — verify the discovery policy grants read buckets", ServiceIDObjectStorage, comp.ID)
			}
			recordPartialFailure(result, ServiceIDObjectStorage, reason)
			continue
		}
		for _, b := range buckets {
			covered := false
			hasLog, logErr := s.listLogsForOCIResource(ctx, sk, comp.ID, b.Name)
			if logErr != nil {
				if !loggingFailed {
					loggingFailed = true
					recordPartialFailure(result, ServiceIDObjectStorage, classifyOCITierError(ServiceIDObjectStorage, "logging", logErr))
				}
			} else {
				covered = hasLog
			}
			detail, detErr := s.getBucket(ctx, sk, ns, b.Name)
			if detErr != nil {
				recordPartialFailure(result, ServiceIDObjectStorage, classifyOCITierError(ServiceIDObjectStorage, "bucket", detErr))
				result.ObjectStores = append(result.ObjectStores, scanner.ObjectStoreSnapshot{ResourceID: b.Name, ImportID: ociBucketImportID(ns, b.Name), Region: s.Region, ServerAccessLoggingEnabled: covered})
				continue
			}
			result.ObjectStores = append(result.ObjectStores, scanner.ObjectStoreSnapshot{
				ResourceID: detail.Name,
				// ImportID: oci_objectstorage_bucket imports by "n/<namespace>/b/<bucket>".
				ImportID:                   ociBucketImportID(ns, detail.Name),
				Region:                     s.Region,
				ServerAccessLoggingEnabled: covered,
				Tags:                       flattenTags(detail.FreeformTags, detail.DefinedTags),
			})
		}
	}
}

func (s *Scanner) getNamespace(ctx context.Context, sk *SigningKey) (string, error) {
	url := fmt.Sprintf("%s/n", strings.TrimRight(s.objectStorageEndpoint(), "/"))
	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return "", callErr
	}
	var ns string
	if jerr := json.Unmarshal(body, &ns); jerr != nil {
		return "", &ociCallError{Wrapped: fmt.Errorf("namespace parse: %w", jerr)}
	}
	return ns, nil
}

func (s *Scanner) listBuckets(ctx context.Context, sk *SigningKey, namespace, compartmentID string) ([]ociBucketSummary, error) {
	url := fmt.Sprintf("%s/n/%s/b?compartmentId=%s",
		strings.TrimRight(s.objectStorageEndpoint(), "/"), namespace, compartmentID)
	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return nil, callErr
	}
	var out []ociBucketSummary
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return nil, &ociCallError{Wrapped: fmt.Errorf("bucket list parse: %w", jerr)}
	}
	return out, nil
}

func (s *Scanner) getBucket(ctx context.Context, sk *SigningKey, namespace, name string) (ociBucket, error) {
	url := fmt.Sprintf("%s/n/%s/b/%s",
		strings.TrimRight(s.objectStorageEndpoint(), "/"), namespace, name)
	body, callErr := s.doSignedGET(ctx, sk, url)
	if callErr != nil {
		return ociBucket{}, callErr
	}
	var out ociBucket
	if jerr := json.Unmarshal(body, &out); jerr != nil {
		return ociBucket{}, &ociCallError{Wrapped: fmt.Errorf("bucket parse: %w", jerr)}
	}
	return out, nil
}
