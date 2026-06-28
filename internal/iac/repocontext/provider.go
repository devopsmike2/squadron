// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

// Package repocontext builds the "EXISTING TERRAFORM CONTEXT" prompt
// block for the discovery proposer by fetching the operator's actual
// placement-map files from their connected IaC repo and summarising
// them deterministically (internal/iac/hclsummary) — no AI tokens
// spent on parsing, only the compact summary is fed to the model.
//
// Scope selection (slice 2b): the recommendations endpoint is keyed by
// cloud account, and there is no stored account->IaC-connection link.
// So the provider summarises the placement files of the SINGLE IaC
// connection that targets the scan's cloud provider. When zero or more
// than one connection matches, it returns "" (no context) rather than
// risk feeding the model addresses from the wrong repo — an honest,
// safe default. A future slice can add an explicit account->connection
// binding to disambiguate the multi-connection case.
package repocontext

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/iacconnstore"
	iacgithub "github.com/devopsmike2/squadron/internal/iac/github"
	"github.com/devopsmike2/squadron/internal/iac/hclsummary"
)

// FileClient is the narrow slice of the GitHub client the provider
// needs: read a file's content at a ref. *iacgithub.PATClient
// satisfies it; tests substitute a fake.
type FileClient interface {
	GetFileContent(ctx context.Context, owner, repo, path, ref string) (*iacgithub.FileContent, error)
}

// defaultByteBudget caps the rendered prompt block (~1.5K tokens).
const defaultByteBudget = 6000

// defaultMaxFiles caps how many distinct placement files we fetch +
// summarise per recommendation, bounding both latency and token cost.
const defaultMaxFiles = 6

// Provider builds repo-context prompt blocks. The zero value is not
// usable; use New. All GitHub + credstore dependencies mirror the
// open-PR handler's wiring so the wiring layer passes the same values.
type Provider struct {
	store      iacconnstore.Store
	clientFor  func(token string) FileClient
	credKey    *credstore.Key
	logger     *zap.Logger
	byteBudget int
	maxFiles   int
}

// New constructs a Provider. Returns nil when any hard dependency is
// missing, so the wiring layer can leave the handler's provider unset
// (and the cold-start prompt unchanged) on deployments that haven't
// wired the IaC substrate or the credstore key.
func New(store iacconnstore.Store, clientFor func(token string) FileClient, credKey *credstore.Key, logger *zap.Logger) *Provider {
	if store == nil || clientFor == nil || credKey == nil {
		return nil
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Provider{
		store:      store,
		clientFor:  clientFor,
		credKey:    credKey,
		logger:     logger,
		byteBudget: defaultByteBudget,
		maxFiles:   defaultMaxFiles,
	}
}

// RepoContextForScope returns the rendered EXISTING TERRAFORM CONTEXT
// block for the given cloud provider, or "" when there is nothing
// usable. Never errors: any failure (no connection, ambiguous match,
// PAT decrypt failure, GitHub fetch failure) degrades to "" so
// recommendation generation always proceeds. accountID is currently
// used only for logging (see package doc on scope selection).
func (p *Provider) RepoContextForScope(ctx context.Context, cloudProvider, accountID string) string {
	if p == nil {
		return ""
	}
	cloudProvider = strings.ToLower(strings.TrimSpace(cloudProvider))
	if cloudProvider == "" {
		cloudProvider = "aws"
	}

	conns, err := p.store.List(ctx)
	if err != nil {
		p.logger.Warn("repocontext: list connections failed; proceeding without repo context", zap.Error(err))
		return ""
	}

	// Find connections whose placement map targets this cloud provider.
	var matched []*iacconnstore.IaCConnection
	for _, c := range conns {
		for _, e := range c.PlacementMap {
			if strings.EqualFold(strings.TrimSpace(e.Provider), cloudProvider) {
				matched = append(matched, c)
				break
			}
		}
	}
	switch len(matched) {
	case 0:
		return ""
	case 1:
		// good — unambiguous
	default:
		p.logger.Info("repocontext: multiple IaC connections match provider; skipping repo context to avoid wrong-repo addresses",
			zap.String("provider", cloudProvider), zap.String("account_id", accountID), zap.Int("matches", len(matched)))
		return ""
	}
	conn := matched[0]

	owner, repo, ok := splitRepo(conn.RepoFullName)
	if !ok {
		return ""
	}
	creds, err := iacconnstore.UnmarshalGitHubPATCreds(conn.CredCiphertext, p.credKey)
	if err != nil {
		p.logger.Warn("repocontext: PAT decrypt failed; proceeding without repo context", zap.Error(err))
		return ""
	}
	client := p.clientFor(creds.Token)
	ref := conn.DefaultBranch

	// Distinct file paths for this provider, capped.
	seen := map[string]struct{}{}
	var paths []string
	for _, e := range conn.PlacementMap {
		if !strings.EqualFold(strings.TrimSpace(e.Provider), cloudProvider) {
			continue
		}
		fp := strings.TrimSpace(e.FilePath)
		if fp == "" {
			continue
		}
		if _, dup := seen[fp]; dup {
			continue
		}
		seen[fp] = struct{}{}
		paths = append(paths, fp)
		if len(paths) >= p.maxFiles {
			break
		}
	}

	var summaries []hclsummary.FileSummary
	for _, fp := range paths {
		fc, ferr := client.GetFileContent(ctx, owner, repo, fp, ref)
		if ferr != nil || fc == nil {
			// File may not exist yet (placement points at a to-be-created
			// path). Skip silently — absence is fine.
			continue
		}
		summaries = append(summaries, hclsummary.SummarizeFile(fp, fc.DecodedContent))
	}
	return hclsummary.RenderForPrompt(summaries, p.byteBudget)
}

func splitRepo(full string) (owner, repo string, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(full), "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
