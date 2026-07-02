// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package azure

// Object-store tier — Azure Storage Accounts (Blob). Coverage-parity
// arc slice 3. Mirrors the AWS S3 surface: a storage account counts as
// instrumented when blob-service diagnostic logging is configured (any
// enabled log category on {account}/blobServices/default), matching the
// S3 ServerAccessLogging axis on scanner.ObjectStoreSnapshot.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

type armStorageAccount struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Location string            `json:"location"`
	Tags     map[string]string `json:"tags"`
}

type armStorageAccountListResponse struct {
	Value    []armStorageAccount `json:"value"`
	NextLink string              `json:"nextLink"`
}

// scanAzureStorage lists storage accounts and appends an
// ObjectStoreSnapshot per account, flipping ServerAccessLoggingEnabled
// when blob diagnostic logging is configured. Partial-failure semantics
// mirror scanAzureSQL.
func (s *Scanner) scanAzureStorage(ctx context.Context, accessToken string, result *scanner.Result) {
	accounts, listErr := s.listStorageAccounts(ctx, accessToken)
	if listErr != nil {
		recordPartialFailure(result, ServiceIDAzureStorage, classifyAzureStorageError(listErr))
		return
	}
	for _, a := range accounts {
		logging, diagErr := s.probeStorageBlobLogging(ctx, accessToken, a.ID)
		if diagErr != nil {
			recordPartialFailure(result, ServiceIDAzureStorage, classifyAzureStorageError(diagErr))
			result.ObjectStores = append(result.ObjectStores, projectStorageAccount(a, false))
			continue
		}
		result.ObjectStores = append(result.ObjectStores, projectStorageAccount(a, logging))
	}
}

func (s *Scanner) listStorageAccounts(ctx context.Context, accessToken string) ([]armStorageAccount, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	pageURL := fmt.Sprintf(
		"%s/subscriptions/%s/providers/Microsoft.Storage/storageAccounts?api-version=%s",
		strings.TrimRight(endpoint, "/"), s.SubscriptionID, armStorageAPIVersion,
	)
	var out []armStorageAccount
	for pageURL != "" {
		body, callErr := s.doARMGet(ctx, accessToken, pageURL)
		if callErr != nil {
			return nil, callErr
		}
		var page armStorageAccountListResponse
		if jerr := json.Unmarshal(body, &page); jerr != nil {
			return nil, &armCallError{Wrapped: fmt.Errorf("storage account list parse: %w", jerr)}
		}
		out = append(out, page.Value...)
		pageURL = page.NextLink
	}
	return out, nil
}

// probeStorageBlobLogging returns true iff the account's blob service
// has any enabled diagnostic log category. A 404 means "no diagnostic
// settings configured" (false, no error), mirroring probeSQLInsights.
func (s *Scanner) probeStorageBlobLogging(ctx context.Context, accessToken, accountARMID string) (bool, error) {
	endpoint := s.armEndpoint
	if endpoint == "" {
		endpoint = armManagementEndpoint
	}
	resourceID := strings.TrimPrefix(accountARMID, "/")
	diagURL := fmt.Sprintf(
		"%s/%s/blobServices/default/providers/microsoft.insights/diagnosticSettings?api-version=%s",
		strings.TrimRight(endpoint, "/"), resourceID, armDiagSettingsAPIVersion,
	)
	body, callErr := s.doARMGet(ctx, accessToken, diagURL)
	if callErr != nil {
		var ace *armCallError
		if errors.As(callErr, &ace) && ace.StatusCode == http.StatusNotFound {
			return false, nil
		}
		return false, callErr
	}
	var resp armDiagnosticSettingsResponse
	if jerr := json.Unmarshal(body, &resp); jerr != nil {
		return false, &armCallError{Wrapped: fmt.Errorf("diagnostic settings parse: %w", jerr)}
	}
	for _, ds := range resp.Value {
		for _, log := range ds.Properties.Logs {
			if log.Enabled {
				return true, nil
			}
		}
	}
	return false, nil
}

func projectStorageAccount(a armStorageAccount, logging bool) scanner.ObjectStoreSnapshot {
	return scanner.ObjectStoreSnapshot{
		ResourceID: a.Name,
		// ImportID: the scanner inventories storage accounts (not containers);
		// azurerm_storage_account imports by the full ARM resource id, which
		// the ARM list returns verbatim as a.ID.
		ImportID:                   a.ID,
		Region:                     a.Location,
		ServerAccessLoggingEnabled: logging,
		Tags:                       copyTags(a.Tags),
	}
}

func classifyAzureStorageError(err error) string {
	return classifyAzureTierError(ServiceIDAzureStorage, "storage", err)
}
