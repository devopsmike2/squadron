// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// v0.89.8 (#617, Stream 22) — squadronctl iac subcommand. Closes the
// CLI-side gap on the connect-IaC-repo surface that shipped in
// v0.89.3 → v0.89.5 (Stream 19). Mirrors the squadronctl plans
// pattern from v0.77 / v0.89.2: cobra subcommand tree, the cliapi
// client.Do helper, the table / json output split, the same error
// envelope handling.
//
// Slice 1 (v0.89.8): list / get / connect / delete / validate.
//
// v0.89.15 (#631, Stream 32) — adds open-pr + update-placement, the
// two subcommands the v0.89.8 slice deferred. The envelope shape is
// stable now that v0.89.11 (slice 1.5 hybrid PR), v0.89.12 (HCL-aware
// merging) and v0.89.14 (action runner steps) have shipped, so the
// CLI can consume the disposition_actual / lifecycle_ignored /
// hcl_patch_failure_reason fields without churn risk.

// iacGitHubPlacementKind is one of the nine canonical
// (provider, resource_kind) rows the in-product wizard pre-populates
// (ui/src/data/iacGithubWizard.ts → IAC_GITHUB_PLACEMENT_KINDS). The
// CLI wizard mirrors them row-for-row so the two surfaces stay in
// lockstep — any change here should land on both sides in the same
// commit. Slice 4 (v0.89.6) added the eighth row
// (dynamodb-contributor-insights); slice 5 (v0.89.10) added the ninth
// (ecs-container-insights); v0.89.15 (#631, Stream 32) backfilled it
// onto the CLI side alongside the open-pr + update-placement
// subcommands.
type iacGitHubPlacementKind struct {
	Provider     string
	ResourceKind string
	DisplayName  string
	Description  string
}

var iacGitHubPlacementKinds = []iacGitHubPlacementKind{
	{
		Provider:     "aws",
		ResourceKind: "ec2-otel-layer",
		DisplayName:  "EC2 OTel layer",
		Description:  "Installs an OpenTelemetry collector or agent on EC2 instances that lack one.",
	},
	{
		Provider:     "aws",
		ResourceKind: "lambda-otel-layer",
		DisplayName:  "Lambda OTel layer",
		Description:  "Attaches the AWS-managed OTel Lambda layer to functions missing instrumentation.",
	},
	{
		Provider:     "aws",
		ResourceKind: "rds-pi-em",
		DisplayName:  "RDS Performance Insights + Enhanced Monitoring",
		Description:  "Enables PI and EM on RDS instances missing either lever.",
	},
	{
		Provider:     "aws",
		ResourceKind: "s3-access-logging",
		DisplayName:  "S3 access logging",
		Description:  "Turns on server-access logging for buckets without it.",
	},
	{
		Provider:     "aws",
		ResourceKind: "alb-access-logs",
		DisplayName:  "ALB / NLB access logs",
		Description:  "Enables access logs on load balancers.",
	},
	{
		Provider:     "aws",
		ResourceKind: "eks-cluster-logging",
		DisplayName:  "EKS control-plane logging",
		Description:  "Turns on api + audit control-plane log types on EKS clusters.",
	},
	{
		Provider:     "aws",
		ResourceKind: "eks-observability-addon",
		DisplayName:  "EKS observability addon",
		Description:  "Installs adot or amazon-cloudwatch-observability on clusters without one ACTIVE.",
	},
	{
		Provider:     "aws",
		ResourceKind: "dynamodb-contributor-insights",
		DisplayName:  "DynamoDB Contributor Insights",
		Description:  "Enables CloudWatch Contributor Insights on DynamoDB tables.",
	},
	{
		Provider:     "aws",
		ResourceKind: "ecs-container-insights",
		DisplayName:  "ECS / Fargate Container Insights",
		Description:  "Enables CloudWatch Container Insights on ECS clusters.",
	},
}

// iacGitHubRepoFullNameRe is the same regex the in-product wizard
// uses (ui/src/data/iacGithubWizard.ts → REPO_FULL_NAME_RE) and the
// server-side handler validates against. Kept identical so an
// operator who passes the CLI check passes the server check too.
var iacGitHubRepoFullNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func newIaCCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "iac",
		Short: "Connect and inspect IaC repos for the proposer",
		Long: `Manage Squadron's connection to a GitHub-hosted Terraform repo so the
proposer can open PRs against your infrastructure. This is the CLI
alternative to the in-product wizard at /discovery/iac/github — same
shape, same nine canonical resource_kind rows, same validation, same
audit trail. Use whichever fits the moment; the runbook at
docs/discovery-iac-first-time-setup.md still applies.

Subcommands wrap these endpoints:

  list              GET    /api/v1/iac/github/connections
  get               (list + filter; no per-id GET endpoint today)
  connect           POST   /api/v1/iac/github/validate + /connections
  delete            DELETE /api/v1/iac/github/connections/:id
  validate          POST   /api/v1/iac/github/validate (dry-run, no DB write)
  open-pr           POST   /api/v1/iac/github/connections/:id/open-pr
  update-placement  PATCH  /api/v1/iac/github/connections/:id/placement-map

The PAT never leaves the connect / validate request body. The CLI
reads it via terminal ReadPassword (no echo), never logs it, never
writes it to the squadronctl config file. The server seals it with
the same AES-GCM substrate as the AWS credentials and unseals it
only when a PR-open call needs it.`,
	}
	cmd.AddCommand(
		newIaCListCommand(),
		newIaCGetCommand(),
		newIaCConnectCommand(),
		newIaCDeleteCommand(),
		newIaCValidateCommand(),
		newIaCOpenPRCommand(),
		newIaCUpdatePlacementCommand(),
	)
	return cmd
}

// newIaCListCommand wraps GET /api/v1/iac/github/connections.
func newIaCListCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List IaC repo connections",
		Long: `List every IaC GitHub connection registered against this Squadron
deployment. One row per connection.

The default output is a column table with the connection_id, repo,
default branch, layout, and the count of placement rows configured.
-o json prints the full API response so a CI script can pipe it
into jq.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient()
			var resp cliapi.ListIaCGitHubConnectionsResponse
			if err := c.Do(cmd.Context(), http.MethodGet,
				"/api/v1/iac/github/connections", nil, nil, &resp); err != nil {
				return renderAPIError(err)
			}
			if flags.Output == "json" {
				out, err := asJSON(resp)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			w := cmd.OutOrStdout()
			if len(resp.Connections) == 0 {
				fmt.Fprintln(w, "no connections")
				return nil
			}
			rows := make([][]string, 0, len(resp.Connections))
			for _, conn := range resp.Connections {
				rows = append(rows, []string{
					truncate(conn.ConnectionID, 12),
					conn.RepoFullName,
					conn.DefaultBranch,
					conn.RepoLayout,
					fmt.Sprintf("%d", placementSetCount(conn.PlacementMap)),
					conn.CreatedAt.Format("2006-01-02 15:04:05 MST"),
				})
			}
			table(w, []string{"CONNECTION_ID", "REPO", "BRANCH", "LAYOUT", "PLACEMENTS", "CREATED"}, rows)
			return nil
		},
	}
	return cmd
}

// newIaCGetCommand fetches the list endpoint and filters client-side
// by connection_id. Slice 1 ships without a per-id GET endpoint —
// the list response is small (cardinality matches the number of
// connected GitHub repos, which is single-digit in every Squadron
// deployment seen so far) and a per-id endpoint hasn't been worth
// the round trip yet. If/when the list response grows past one page,
// add the per-id endpoint and this command keeps the same shape.
func newIaCGetCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <connection-id>",
		Short: "Show one IaC repo connection",
		Long: `Show full detail for a single connection — repo, branch, layout,
branch prefix, reviewer team, and the placement map (one row per
canonical resource_kind that has a file path set).

Implementation note: slice 1 fetches the list endpoint and filters
client-side. A per-id GET endpoint isn't shipping until the list
response is bigger than one page; for now the list payload is small
enough that the extra round trip isn't worth it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			c := newClient()
			var resp cliapi.ListIaCGitHubConnectionsResponse
			if err := c.Do(cmd.Context(), http.MethodGet,
				"/api/v1/iac/github/connections", nil, nil, &resp); err != nil {
				return renderAPIError(err)
			}
			var found *cliapi.IaCGitHubConnection
			for i := range resp.Connections {
				if resp.Connections[i].ConnectionID == id {
					found = &resp.Connections[i]
					break
				}
			}
			if found == nil {
				return fmt.Errorf("connection %s not found", id)
			}
			if flags.Output == "json" {
				out, err := asJSON(found)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "connection_id:        %s\n", found.ConnectionID)
			fmt.Fprintf(w, "provider:             %s\n", found.Provider)
			fmt.Fprintf(w, "auth_kind:            %s\n", found.AuthKind)
			fmt.Fprintf(w, "repo:                 %s\n", found.RepoFullName)
			fmt.Fprintf(w, "default_branch:       %s\n", found.DefaultBranch)
			fmt.Fprintf(w, "repo_layout:          %s\n", found.RepoLayout)
			if found.BranchPrefix != "" {
				fmt.Fprintf(w, "branch_prefix:        %s\n", found.BranchPrefix)
			}
			if found.ReviewerTeamHandle != "" {
				fmt.Fprintf(w, "reviewer_team_handle: %s\n", found.ReviewerTeamHandle)
			}
			fmt.Fprintf(w, "created:              %s\n",
				found.CreatedAt.Format("2006-01-02 15:04:05 MST"))
			fmt.Fprintln(w, strings.Repeat("-", 60))
			fmt.Fprintln(w, "Placement map:")
			rows := make([][]string, 0, len(found.PlacementMap))
			for _, p := range found.PlacementMap {
				path := p.FilePath
				if path == "" {
					path = "<skipped>"
				}
				rows = append(rows, []string{p.Provider, p.ResourceKind, path})
			}
			table(w, []string{"PROVIDER", "RESOURCE_KIND", "FILE_PATH"}, rows)
			return nil
		},
	}
	return cmd
}

// newIaCConnectCommand drives the wizard or reads --file and posts
// to /validate then /connections. The interactive path is the
// default; --file is the CI / scripted path.
func newIaCConnectCommand() *cobra.Command {
	var (
		filePath  string
		skipCheck bool
	)
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect a GitHub Terraform repo (interactive wizard)",
		Long: `Walk the same steps the in-product wizard at /discovery/iac/github
walks: PAT → repo → layout → default branch → branch prefix →
reviewer team → per-row placement map → validate → save.

The PAT is read via golang.org/x/term ReadPassword (no echo to
terminal, no scrollback). It travels only in the validate and
save request bodies, is never written to the squadronctl config
file, and is never echoed back in any CLI output.

Non-interactive form (CI / scripts):

  squadronctl iac connect --file ./connection.yaml

YAML or JSON, auto-detected by extension. Same field names as the
wire shape:

  token: ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxx
  repo_full_name: my-org/infra-terraform
  default_branch: main
  repo_layout: multi        # "mono" or "multi"
  branch_prefix: ""         # optional, server default is squadron/rec
  reviewer_team_handle: ""  # optional, "my-org/platform-reviewers"
  placement_map:
    - { provider: aws, resource_kind: ec2-otel-layer,           file_path: modules/ec2/main.tf }
    - { provider: aws, resource_kind: lambda-otel-layer,        file_path: modules/lambda/main.tf }
    - { provider: aws, resource_kind: rds-pi-em,                file_path: modules/rds/main.tf }
    - { provider: aws, resource_kind: s3-access-logging,        file_path: modules/s3/main.tf }
    - { provider: aws, resource_kind: alb-access-logs,          file_path: modules/elb/main.tf }
    - { provider: aws, resource_kind: eks-cluster-logging,      file_path: modules/eks/main.tf }
    - { provider: aws, resource_kind: eks-observability-addon,  file_path: modules/eks/main.tf }
    - { provider: aws, resource_kind: dynamodb-contributor-insights, file_path: modules/dynamodb/main.tf }

Each placement_map entry maps to one row of the server's wizard.
Omit an entry (or set file_path to "") to skip that kind.

For first-time setup, walk through docs/discovery-iac-first-time-
setup.md alongside this command — it covers the GitHub-side PAT
bootstrap and the trust thesis.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				req       cliapi.IaCGitHubSaveConnectionRequest
				bufReader *bufio.Reader // populated only on the interactive path
				err       error
			)
			if filePath != "" {
				req, err = readConnectFile(filePath)
				if err != nil {
					return err
				}
			} else {
				// One bufio.Reader for the whole interactive
				// session — the wizard, the post-validate confirm
				// prompt, every prompt the session asks. Allocating
				// a second reader on the same underlying stdin
				// would silently lose bytes.
				bufReader = bufio.NewReader(cmd.InOrStdin())
				req, err = runConnectWizardWithReader(cmd.InOrStdin(), bufReader, cmd.OutOrStdout())
				if err != nil {
					return err
				}
			}

			c := newClient()
			// Validate first so the operator sees preflight rows
			// before the connection is persisted. The --file path
			// runs the same validate (defense-in-depth: a stale
			// file with a wrong path should fail loud here, not
			// silently save a broken connection).
			if !skipCheck {
				validateReq := cliapi.IaCGitHubValidateRequest{
					Token:         req.Token,
					RepoFullName:  req.RepoFullName,
					DefaultBranch: req.DefaultBranch,
					PlacementMap:  req.PlacementMap,
				}
				var vresp cliapi.IaCGitHubValidateResponse
				if err := c.Do(cmd.Context(), http.MethodPost,
					"/api/v1/iac/github/validate", nil, validateReq, &vresp); err != nil {
					return renderAPIError(err)
				}
				renderValidateResponse(cmd.OutOrStdout(), &vresp)
				if vresp.RepoErr != nil {
					return fmt.Errorf("validate failed: %s", vresp.RepoErr.Message)
				}
			}

			// Confirm before save when running interactively. Uses
			// the same shared bufio.Reader the wizard used so no
			// bytes are silently lost between prompts.
			if filePath == "" {
				ok, err := promptConfirmShared(bufReader, cmd.OutOrStdout(),
					"Save this connection?", false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted; nothing saved.")
					return nil
				}
			}

			var sresp cliapi.IaCGitHubSaveConnectionResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/iac/github/connections", nil, req, &sresp); err != nil {
				return renderAPIError(err)
			}
			if flags.Output == "json" {
				out, err := asJSON(sresp)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "connection saved\n")
			fmt.Fprintf(w, "connection_id: %s\n", sresp.ConnectionID)
			fmt.Fprintf(w, "repo:          %s\n", sresp.RepoFullName)
			fmt.Fprintf(w, "status:        %s\n", sresp.Status)
			return nil
		},
	}
	cmd.Flags().StringVarP(&filePath, "file", "f", "",
		`Read connection params from a YAML or JSON file (skips interactive prompts).`)
	cmd.Flags().BoolVar(&skipCheck, "skip-validate", false,
		`Skip the preflight validate POST; only save. Useful when the operator already validated.`)
	return cmd
}

// newIaCDeleteCommand wraps DELETE /api/v1/iac/github/connections/:id.
func newIaCDeleteCommand() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <connection-id>",
		Short: "Delete an IaC repo connection",
		Long: `Delete one IaC GitHub connection by id. The server tombstones the
connection and zeroes its sealed PAT. Open PRs already in flight are
unaffected; the proposer's open-pr surface goes silent for resource
kinds that referenced this connection until another connection
covers them.

Confirmation prompts default to no. Pass --yes to skip the prompt
(useful in CI cleanup scripts).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			if !yes {
				ok, err := promptConfirm(cmd.InOrStdin(), cmd.OutOrStdout(),
					fmt.Sprintf("Delete connection %s?", id), false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted; nothing deleted.")
					return nil
				}
			}
			c := newClient()
			if err := c.Do(cmd.Context(), http.MethodDelete,
				"/api/v1/iac/github/connections/"+url.PathEscape(id),
				nil, nil, nil); err != nil {
				return renderAPIError(err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "deleted %s\n", id)
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt.")
	return cmd
}

// newIaCValidateCommand wraps POST /api/v1/iac/github/validate. No
// DB write — pure preflight against the server's GitHub client.
func newIaCValidateCommand() *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Preflight a repo + placement map against GitHub",
		Long: `Run the same validate the connect wizard runs — repo reachability,
default-branch fetch, per-row placement-map preflight — without
persisting anything. Use this to debug a placement map before
committing to the save, or to re-check an existing connection's
repo state after a PAT rotation.

Interactive prompts mirror connect's first five steps (PAT, repo,
default branch, placement map). Pass --file <path> to skip the
prompts and read from YAML or JSON; the file format is the same as
` + "`iac connect --file`" + ` minus the persistence-only fields
(repo_layout / branch_prefix / reviewer_team_handle).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			var (
				req cliapi.IaCGitHubValidateRequest
				err error
			)
			if filePath != "" {
				save, err2 := readConnectFile(filePath)
				if err2 != nil {
					return err2
				}
				req = cliapi.IaCGitHubValidateRequest{
					Token:         save.Token,
					RepoFullName:  save.RepoFullName,
					DefaultBranch: save.DefaultBranch,
					PlacementMap:  save.PlacementMap,
				}
			} else {
				req, err = runValidateWizard(cmd.InOrStdin(), cmd.OutOrStdout())
				if err != nil {
					return err
				}
			}
			c := newClient()
			var resp cliapi.IaCGitHubValidateResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/iac/github/validate", nil, req, &resp); err != nil {
				return renderAPIError(err)
			}
			if flags.Output == "json" {
				out, err := asJSON(resp)
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			renderValidateResponse(cmd.OutOrStdout(), &resp)
			if resp.RepoErr != nil {
				return fmt.Errorf("validate failed: %s", resp.RepoErr.Message)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&filePath, "file", "f", "",
		`Read validate params from a YAML or JSON file (skips interactive prompts).`)
	return cmd
}

// newIaCOpenPRCommand wraps POST
// /api/v1/iac/github/connections/:id/open-pr. v0.89.15 (#631, Stream
// 32) — the v0.89.8 slice deferred this surface until the
// recommendation envelope stabilised; v0.89.11 (slice 1.5) +
// v0.89.12 (HCL-aware merging) landed the disposition_actual +
// lifecycle_ignored + hcl_patch_failure_reason fields the CLI now
// renders.
//
// The recommendation envelope (scan_id, step_idx, resource_kind,
// snippet, proposer_reasoning, affected_resources, account_id, and
// the optional hcl_patch) comes from --from-file or stdin. The CLI
// relays it verbatim — hcl_patch in particular is opaque to the CLI
// (the server owns the parse via internal/iac/hclpatch.Patch). PAT
// bytes never appear in the request or output: the server unseals
// the stored PAT keyed off the connection ID.
func newIaCOpenPRCommand() *cobra.Command {
	var (
		connectionID string
		fromFile     string
		dryRun       bool
	)
	cmd := &cobra.Command{
		Use:   "open-pr",
		Short: "Open a Squadron PR against a connected IaC repo",
		Long: `Hand the proposer's recommendation envelope to the IaC connection
and open a PR (or --dry-run preflight the placement against the
configured map). Calls POST /api/v1/iac/github/connections/:id/open-pr
under the hood (or /validate when --dry-run is set).

The envelope is read from --from-file (a JSON file) or, if not
given, stdin. Shape:

  {
    "scan_id":             "<scan-uuid>",
    "step_idx":            0,
    "resource_kind":       "lambda-otel-layer",
    "snippet":             "<terraform>",
    "proposer_reasoning":  "<prose>",
    "affected_resources": ["arn:aws:lambda:...", ...],
    "account_id":          "111111111111",
    "hcl_patch":           { ... }
  }

hcl_patch is optional. Omit it (or pre-v0.89.12 envelopes) and the
server falls back to the slice-1.5 append-only path with a
"[needs manual merge]" PR title prefix + a squadron/needs-manual-merge
label. Include it on patch_existing kinds and the server attempts
HCL-aware merging; clean merges drop the manual-merge banner.

Use this when the in-product Recommendations tab isn't reachable —
CI scripts that pipe a recommendation through jq, or an operator
debugging a placement-map issue from a terminal. The UI's Open-PR
button is the equivalent surface for the non-CLI path.

Worked example (future, when scan + generate-recommendations
subcommands exist):

  squadronctl discovery aws scan-all -o json | \
    jq '.recommendations[0]' | \
    squadronctl iac open-pr --connection-id <id>

For now, the recommendation envelope comes from the API or a hand-
edited JSON file:

  curl -sH "Authorization: Bearer $SQUADRON_TOKEN" \
    "$SQUADRON_URL/api/v1/recommendations?scan_id=$SCAN" | \
    jq '.recommendations[0]' | \
    squadronctl iac open-pr --connection-id $CID

--dry-run hits the validate endpoint instead and prints the
placement preflight without creating a PR.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(connectionID) == "" {
				return fmt.Errorf("--connection-id is required")
			}
			env, err := readOpenPREnvelope(fromFile, cmd.InOrStdin())
			if err != nil {
				return err
			}
			c := newClient()

			// --dry-run: hit /validate instead of /open-pr. The
			// validate endpoint takes (token, repo, default_branch,
			// placement_map), none of which the operator has from
			// the open-pr envelope — so we first fetch the
			// connection's placement_map via the list endpoint,
			// then preview which row the proposer would land on.
			// No PAT round trip happens here: --dry-run never
			// reaches GitHub, it just answers "is there a row for
			// this resource_kind?".
			if dryRun {
				return runOpenPRDryRun(cmd.Context(), cmd.OutOrStdout(), c, connectionID, env)
			}

			resp, err := c.OpenIaCGitHubPullRequest(cmd.Context(), connectionID, env)
			if err != nil {
				return renderOpenPRError(err)
			}
			if flags.Output == "json" {
				out, jerr := asJSON(resp)
				if jerr != nil {
					return jerr
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			renderOpenPRResponse(cmd.OutOrStdout(), resp)
			return nil
		},
	}
	cmd.Flags().StringVar(&connectionID, "connection-id", "",
		"IaC connection ID to open the PR against (required).")
	cmd.Flags().StringVar(&fromFile, "from-file", "",
		`Path to a JSON file containing the recommendation envelope. If empty, the envelope is read from stdin.`)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false,
		"Preview placement without creating a PR. Looks up the connection's placement_map row for the envelope's resource_kind and renders what the open-pr would target.")
	_ = cmd.MarkFlagRequired("connection-id")
	return cmd
}

// readOpenPREnvelope parses the recommendation envelope from a path
// or stdin. Validates the four required fields (scan_id,
// resource_kind, snippet) before returning — a tight client-side
// error beats the server's 400.
func readOpenPREnvelope(fromFile string, stdin io.Reader) (cliapi.IaCGitHubOpenPRRequest, error) {
	var (
		raw []byte
		err error
		req cliapi.IaCGitHubOpenPRRequest
	)
	switch {
	case fromFile != "":
		raw, err = os.ReadFile(fromFile)
		if err != nil {
			return req, fmt.Errorf("read %s: %w", fromFile, err)
		}
	default:
		raw, err = io.ReadAll(stdin)
		if err != nil {
			return req, fmt.Errorf("read stdin: %w", err)
		}
		if len(strings.TrimSpace(string(raw))) == 0 {
			return req, fmt.Errorf("no envelope on stdin; pass --from-file or pipe a JSON object")
		}
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return req, fmt.Errorf("parse recommendation envelope as JSON: %w", err)
	}
	if strings.TrimSpace(req.ScanID) == "" {
		return req, fmt.Errorf("envelope missing required field scan_id")
	}
	if strings.TrimSpace(req.ResourceKind) == "" {
		return req, fmt.Errorf("envelope missing required field resource_kind")
	}
	if strings.TrimSpace(req.Snippet) == "" {
		return req, fmt.Errorf("envelope missing required field snippet")
	}
	return req, nil
}

// runOpenPRDryRun previews the open-pr placement without a PR
// round-trip. Loads the connection's placement_map via the list
// endpoint (no per-id GET yet, same as `iac get`), finds the row
// matching the envelope's resource_kind, and prints the preview.
// 422 (NoPlacementMapping) on miss — matches what the server would
// have returned.
func runOpenPRDryRun(
	ctx context.Context,
	out io.Writer,
	c *cliapi.Client,
	connectionID string,
	env cliapi.IaCGitHubOpenPRRequest,
) error {
	var resp cliapi.ListIaCGitHubConnectionsResponse
	if err := c.Do(ctx, http.MethodGet,
		"/api/v1/iac/github/connections", nil, nil, &resp); err != nil {
		return renderAPIError(err)
	}
	var conn *cliapi.IaCGitHubConnection
	for i := range resp.Connections {
		if resp.Connections[i].ConnectionID == connectionID {
			conn = &resp.Connections[i]
			break
		}
	}
	if conn == nil {
		return fmt.Errorf("connection %s not found. List with 'squadronctl iac list'.", connectionID)
	}
	var match *cliapi.IaCGitHubPlacementEntry
	for i := range conn.PlacementMap {
		if conn.PlacementMap[i].ResourceKind == env.ResourceKind &&
			conn.PlacementMap[i].FilePath != "" {
			match = &conn.PlacementMap[i]
			break
		}
	}
	if match == nil {
		return fmt.Errorf(
			"No placement row configured for resource_kind %s. Configure via 'squadronctl iac update-placement --connection-id %s'.",
			env.ResourceKind, connectionID)
	}
	if flags.Output == "json" {
		preview := map[string]any{
			"dry_run":       true,
			"connection_id": connectionID,
			"repo":          conn.RepoFullName,
			"resource_kind": env.ResourceKind,
			"file_path":     match.FilePath,
		}
		s, err := asJSON(preview)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, s)
		return nil
	}
	fmt.Fprintln(out, "Dry run — no PR would be opened.")
	fmt.Fprintf(out, "Connection:   %s\n", connectionID)
	fmt.Fprintf(out, "Repo:         %s\n", conn.RepoFullName)
	fmt.Fprintf(out, "Kind:         %s\n", env.ResourceKind)
	fmt.Fprintf(out, "Target file:  %s\n", match.FilePath)
	return nil
}

// renderOpenPRResponse formats the success body for human output.
// Mirrors the in-product success card row-for-row: PR URL,
// disposition_actual, branch, file, commit. The patch_existing
// fallback path gets a loud warning line citing the failure reason;
// the lifecycle.ignore_changes signal gets a separate warning. The
// tone is engineer-to-engineer terse (no green/amber emoji, no
// marketing language) — the UI's badges translate the same signals
// into visual cues.
func renderOpenPRResponse(out io.Writer, resp *cliapi.IaCGitHubOpenPRResponse) {
	disposition := resp.DispositionActual
	if disposition == "" {
		disposition = resp.Disposition
	}
	fmt.Fprintf(out, "PR opened:    %s\n", resp.PRURL)
	if disposition != "" {
		fmt.Fprintf(out, "Disposition:  %s\n", disposition)
	}
	fmt.Fprintf(out, "Branch:       %s\n", resp.Branch)
	fmt.Fprintf(out, "File:         %s\n", resp.FilePath)
	if resp.CommitSHA != "" {
		fmt.Fprintf(out, "Commit:       %s\n", resp.CommitSHA)
	}
	if disposition == "patch_existing_fell_back_to_append" {
		reason := resp.HCLPatchFailureReason
		if reason == "" {
			reason = "unknown"
		}
		fmt.Fprintf(out,
			"WARNING:      HCL-aware merge fell back to append-only (%s). The PR carries a manual-merge label; review the placement file before merging.\n",
			reason)
	}
	if resp.LifecycleIgnored {
		fmt.Fprintln(out,
			"WARNING:      Target resource has lifecycle.ignore_changes covering a patched attribute. Terraform may not apply the change until the ignore is lifted.")
	}
}

// renderOpenPRError humanises the open-pr error codes per the design
// doc §6 + §7. Unrecognised codes fall through to renderAPIError so
// the existing suggested_step + doc_link rendering still fires.
//
// PAT bytes never appear in these messages — by design, the open-pr
// handler unseals the stored token server-side and the failure
// modes here only carry connection-level metadata.
func renderOpenPRError(err error) error {
	var apiErr *cliapi.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "NoPlacementMapping":
		// The handler's message already mentions the resource_kind; we
		// add the CLI-specific suggested step that points at the new
		// update-placement subcommand rather than the wizard URL.
		return fmt.Errorf(
			"%s\nsuggested: Configure a placement row via 'squadronctl iac update-placement --connection-id <id>'.",
			apiErr.Detail)
	case "RepoNotFound":
		return fmt.Errorf(
			"%s\nsuggested: Re-run 'squadronctl iac connect' with a fresh PAT.",
			apiErr.Detail)
	case "AuthFailed":
		return fmt.Errorf(
			"PAT no longer valid. Re-run 'squadronctl iac connect' with a fresh PAT.")
	case "DefaultBranchWriteRefused":
		return fmt.Errorf(
			"[internal error] Squadron tried to write the default branch. Please file an issue: %s",
			apiErr.Detail)
	case "SquadronFileAlreadyExists":
		return fmt.Errorf(
			"%s\nsuggested: Close or merge the existing Squadron PR before re-opening.",
			apiErr.Detail)
	}
	if strings.HasPrefix(apiErr.Code, "HCLPatchFailed") {
		return fmt.Errorf(
			"HCL-aware merge failed (%s: %s). Squadron would fall back to append-only with a manual-merge label.",
			apiErr.Code, apiErr.Detail)
	}
	return renderAPIError(err)
}

// newIaCUpdatePlacementCommand wraps PATCH
// /api/v1/iac/github/connections/:id/placement-map. v0.89.15 (#631,
// Stream 32) — the v0.89.8 slice deferred this surface until an
// interactive map editor was worth a separate review pass.
//
// Two paths: --file for non-interactive use (CI scripts, scripted
// re-configuration), and an interactive prompt that walks each of
// the nine canonical resource_kind rows. Either way the CLI shows
// a diff preview ("EC2 OTel layer: <old> → <new>") before confirming.
//
// Token bytes never appear in this flow — the handler preserves the
// stored cred_ciphertext untouched (#610 design doc invariant) and
// only the placement_map column is mutated.
func newIaCUpdatePlacementCommand() *cobra.Command {
	var (
		connectionID string
		filePath     string
		yes          bool
	)
	cmd := &cobra.Command{
		Use:   "update-placement",
		Short: "Update an IaC connection's placement map",
		Long: `Replace an IaC connection's placement_map. The placement_map is
the (provider, resource_kind) → file_path lookup the proposer uses
to decide where to write the recommendation snippet on PR open.
Calls PATCH /api/v1/iac/github/connections/:id/placement-map.

Two input forms:

  squadronctl iac update-placement --connection-id <id>
      Interactive — walks each of the nine canonical resource_kind
      rows showing the current value (or "<skipped>"); Enter keeps,
      a new path replaces, "skip" clears the row.

  squadronctl iac update-placement --connection-id <id> --file map.yaml
      Non-interactive — reads a YAML or JSON file with a
      placement_map: [...] array. Same shape as 'iac connect --file':

        placement_map:
          - { provider: aws, resource_kind: ec2-otel-layer, file_path: modules/ec2/main.tf }
          - { provider: aws, resource_kind: lambda-otel-layer, file_path: modules/lambda/main.tf }
          - ...

A diff preview ("EC2 OTel layer: <old> → <new>") prints before the
confirm prompt. Pass --yes to skip the prompt (CI cleanup).

Only the placement_map mutates; token, repo, default_branch,
branch_prefix and reviewer_team_handle stay owned by the connect
wizard's create path. Re-run 'iac connect' to rotate any of those.

Use this from the CLI when the in-product wizard deep-link is
unreachable — re-keying placement after a repo refactor, or
scripted re-configuration from a config-as-code substrate.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(connectionID) == "" {
				return fmt.Errorf("--connection-id is required")
			}
			c := newClient()
			// Load the existing connection (list + filter, same as
			// `iac get` — no per-id GET endpoint yet, design-doc
			// rationale stands).
			conn, err := loadIaCConnection(cmd.Context(), c, connectionID)
			if err != nil {
				return err
			}

			var newMap []cliapi.IaCGitHubPlacementEntry
			if filePath != "" {
				save, err := readConnectFile(filePath)
				if err != nil {
					return err
				}
				newMap, err = normalizePlacementMap(save.PlacementMap)
				if err != nil {
					return err
				}
			} else {
				newMap, err = runUpdatePlacementWizard(
					cmd.InOrStdin(), cmd.OutOrStdout(), conn.PlacementMap)
				if err != nil {
					return err
				}
			}

			// Diff preview — always rendered before any confirm so
			// the operator sees the delta in the same format the
			// interactive prompt used.
			renderPlacementDiff(cmd.OutOrStdout(), conn.PlacementMap, newMap)

			if !yes {
				ok, err := promptConfirm(cmd.InOrStdin(), cmd.OutOrStdout(),
					fmt.Sprintf("Update placement map on %s?", conn.RepoFullName), false)
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.OutOrStdout(), "aborted; placement map unchanged.")
					return nil
				}
			}

			resp, err := c.UpdateIaCGitHubPlacementMap(cmd.Context(), connectionID,
				cliapi.IaCGitHubUpdatePlacementMapRequest{PlacementMap: newMap})
			if err != nil {
				return renderUpdatePlacementError(err)
			}
			if flags.Output == "json" {
				out, jerr := asJSON(resp)
				if jerr != nil {
					return jerr
				}
				fmt.Fprintln(cmd.OutOrStdout(), out)
				return nil
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "placement map updated on %s\n", resp.RepoFullName)
			rows := make([][]string, 0, len(resp.PlacementMap))
			for _, p := range resp.PlacementMap {
				path := p.FilePath
				if path == "" {
					path = "<skipped>"
				}
				rows = append(rows, []string{p.Provider, p.ResourceKind, path})
			}
			table(w, []string{"PROVIDER", "RESOURCE_KIND", "FILE_PATH"}, rows)
			return nil
		},
	}
	cmd.Flags().StringVar(&connectionID, "connection-id", "",
		"IaC connection ID whose placement map to update (required).")
	cmd.Flags().StringVarP(&filePath, "file", "f", "",
		"Read the new placement_map from a YAML or JSON file (skips the interactive prompts).")
	cmd.Flags().BoolVar(&yes, "yes", false,
		"Skip the confirm prompt after the diff preview.")
	_ = cmd.MarkFlagRequired("connection-id")
	return cmd
}

// loadIaCConnection looks up one connection by ID via the list
// endpoint. Mirrors `iac get` — single-digit connection cardinality
// in every Squadron deployment makes the per-id round trip
// unnecessary. Returns a 4xx-shaped error on miss so the command
// layer can humanise it cleanly.
func loadIaCConnection(
	ctx context.Context,
	c *cliapi.Client,
	connectionID string,
) (*cliapi.IaCGitHubConnection, error) {
	var resp cliapi.ListIaCGitHubConnectionsResponse
	if err := c.Do(ctx, http.MethodGet,
		"/api/v1/iac/github/connections", nil, nil, &resp); err != nil {
		return nil, renderAPIError(err)
	}
	for i := range resp.Connections {
		if resp.Connections[i].ConnectionID == connectionID {
			return &resp.Connections[i], nil
		}
	}
	return nil, fmt.Errorf(
		"Connection %s not found. List with 'squadronctl iac list'.",
		connectionID)
}

// runUpdatePlacementWizard drives the per-row interactive editor.
// For each of the nine canonical kinds it shows the current value
// (or "<skipped>") and accepts Enter-to-keep, a new path, or the
// "skip" keyword to clear the row.
//
// Reuses the same shared-bufio-reader discipline as the connect
// wizard — no fresh bufio.Reader per prompt or bytes get dropped.
func runUpdatePlacementWizard(
	in io.Reader,
	out io.Writer,
	current []cliapi.IaCGitHubPlacementEntry,
) ([]cliapi.IaCGitHubPlacementEntry, error) {
	currentByKind := make(map[string]string, len(current))
	for _, p := range current {
		currentByKind[p.ResourceKind] = p.FilePath
	}
	r := bufio.NewReader(in)
	fmt.Fprintln(out, "Placement map editor — Enter to keep, new path to replace, \"skip\" to clear.")
	fmt.Fprintln(out, "")
	out2 := make([]cliapi.IaCGitHubPlacementEntry, 0, len(iacGitHubPlacementKinds))
	for _, k := range iacGitHubPlacementKinds {
		cur := currentByKind[k.ResourceKind]
		display := cur
		if display == "" {
			display = "<skipped>"
		}
		fmt.Fprintf(out, "  %s — %s\n", k.DisplayName, k.Description)
		val, err := promptString(r, out,
			fmt.Sprintf("    file_path [%s]: ", display), cur)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(val), "skip") {
			val = ""
		}
		out2 = append(out2, cliapi.IaCGitHubPlacementEntry{
			Provider:     k.Provider,
			ResourceKind: k.ResourceKind,
			FilePath:     val,
		})
	}
	fmt.Fprintln(out, "")
	return out2, nil
}

// normalizePlacementMap validates the placement_map from a --file.
// Rejects unknown resource_kind values, empty provider, and rows
// where file_path is whitespace-only. Empty file_path is allowed —
// that's the "skip" signal the substrate persists.
func normalizePlacementMap(rows []cliapi.IaCGitHubPlacementEntry) ([]cliapi.IaCGitHubPlacementEntry, error) {
	known := make(map[string]struct{}, len(iacGitHubPlacementKinds))
	for _, k := range iacGitHubPlacementKinds {
		known[k.ResourceKind] = struct{}{}
	}
	out := make([]cliapi.IaCGitHubPlacementEntry, 0, len(rows))
	for _, r := range rows {
		if strings.TrimSpace(r.Provider) == "" {
			return nil, fmt.Errorf("placement map entry missing provider (resource_kind=%q)", r.ResourceKind)
		}
		if strings.TrimSpace(r.ResourceKind) == "" {
			return nil, fmt.Errorf("placement map entry missing resource_kind")
		}
		if _, ok := known[r.ResourceKind]; !ok {
			return nil, fmt.Errorf("unknown resource_kind %q; expected one of the nine canonical kinds (see 'iac connect --help')", r.ResourceKind)
		}
		// file_path may be empty (the "skip" signal) but if set,
		// reject whitespace-only values that would otherwise round-
		// trip as a hidden bug.
		if r.FilePath != "" && strings.TrimSpace(r.FilePath) == "" {
			return nil, fmt.Errorf("placement map entry for %s has whitespace-only file_path", r.ResourceKind)
		}
		out = append(out, r)
	}
	return out, nil
}

// renderPlacementDiff prints the per-row delta between two
// placement maps. Format mirrors the prompt's tone — single-line
// "<DisplayName>: <old> → <new>" rows for kinds that changed, and a
// silent skip for rows that didn't.
func renderPlacementDiff(
	out io.Writer,
	before, after []cliapi.IaCGitHubPlacementEntry,
) {
	beforeByKind := make(map[string]string, len(before))
	for _, p := range before {
		beforeByKind[p.ResourceKind] = p.FilePath
	}
	afterByKind := make(map[string]string, len(after))
	for _, p := range after {
		afterByKind[p.ResourceKind] = p.FilePath
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Placement map diff:")
	changes := 0
	for _, k := range iacGitHubPlacementKinds {
		b := beforeByKind[k.ResourceKind]
		a := afterByKind[k.ResourceKind]
		if b == a {
			continue
		}
		changes++
		bDisp := b
		if bDisp == "" {
			bDisp = "<skipped>"
		}
		aDisp := a
		if aDisp == "" {
			aDisp = "<skipped>"
		}
		fmt.Fprintf(out, "  %s: %s -> %s\n", k.DisplayName, bDisp, aDisp)
	}
	if changes == 0 {
		fmt.Fprintln(out, "  (no changes)")
	}
	fmt.Fprintln(out, "")
}

// renderUpdatePlacementError humanises the v0.89.4 #610 endpoint's
// error codes. Unrecognised codes fall through to renderAPIError so
// the existing suggested_step + doc_link rendering still fires.
func renderUpdatePlacementError(err error) error {
	var apiErr *cliapi.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	switch apiErr.Code {
	case "ConnectionNotFound":
		return fmt.Errorf(
			"Connection not found. List with 'squadronctl iac list'.")
	}
	return renderAPIError(err)
}

// --- Wizard / prompts ---------------------------------------------

// runConnectWizardWithReader drives the eight interactive steps for
// `connect`. out gets prose and prompts; in is the byte source
// (terminal or piped stdin in tests); r is the shared bufio.Reader
// the caller will keep using for the post-validate confirm prompt.
//
// The masked PAT prompt bypasses bufio when stdin is a TTY — it
// goes straight to the FD via x/term.ReadPassword — and falls back
// to the shared bufio.Reader otherwise so tests can drive it
// deterministically.
//
// Threading the reader through is load-bearing. Creating multiple
// bufio.Readers against the same underlying io.Reader silently
// drops bytes (each reader buffers ahead from the source), and any
// later prompt against a fresh reader would read empty strings.
func runConnectWizardWithReader(in io.Reader, r *bufio.Reader, out io.Writer) (cliapi.IaCGitHubSaveConnectionRequest, error) {
	req := cliapi.IaCGitHubSaveConnectionRequest{}

	fmt.Fprintln(out, "Squadron IaC GitHub connection wizard")
	fmt.Fprintln(out, "See docs/discovery-iac-first-time-setup.md for the trust thesis.")
	fmt.Fprintln(out, "")

	// Step 1 — PAT.
	fmt.Fprintln(out, "Token scope must include `repo`.")
	token, err := readSecret(in, r, out, "GitHub Personal Access Token: ")
	if err != nil {
		return req, err
	}
	if token == "" {
		return req, fmt.Errorf("PAT cannot be empty")
	}
	req.Token = token

	// Step 2 — repo.
	repo, err := promptString(r, out, "Repository (owner/repo): ", "")
	if err != nil {
		return req, err
	}
	if !iacGitHubRepoFullNameRe.MatchString(repo) {
		return req, fmt.Errorf("repo must match owner/repo (got %q)", repo)
	}
	req.RepoFullName = repo

	// Step 3 — layout.
	layout, err := promptString(r, out,
		`Repo layout — "mono" (one root per env, e.g. environments/prod/...) or "multi" (per-module repo) [multi]: `,
		"multi")
	if err != nil {
		return req, err
	}
	if layout != "mono" && layout != "multi" {
		return req, fmt.Errorf(`repo_layout must be "mono" or "multi" (got %q)`, layout)
	}
	req.RepoLayout = layout

	// Step 4 — default branch.
	branch, err := promptString(r, out,
		"Default branch (blank = server fetches via the GitHub API): ", "")
	if err != nil {
		return req, err
	}
	req.DefaultBranch = branch

	// Step 5 — branch prefix.
	prefix, err := promptString(r, out,
		"Branch prefix for PRs (blank = server default `squadron/rec`): ", "")
	if err != nil {
		return req, err
	}
	req.BranchPrefix = prefix

	// Step 6 — reviewer team.
	reviewer, err := promptString(r, out,
		"Reviewer team handle (blank = no auto-request): ", "")
	if err != nil {
		return req, err
	}
	req.ReviewerTeamHandle = reviewer

	// Step 7 — placement map. Show one row per canonical kind with
	// a hint, accept a file path, empty = skip.
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Placement map — file path per resource_kind. Blank to skip.")
	fmt.Fprintf(out, "Example file paths: %s\n\n", placementHintFor(layout))
	req.PlacementMap = make([]cliapi.IaCGitHubPlacementEntry, 0, len(iacGitHubPlacementKinds))
	for _, k := range iacGitHubPlacementKinds {
		hint := strings.ReplaceAll(placementHintFor(layout), "{kind}", k.ResourceKind)
		fmt.Fprintf(out, "  %s — %s\n", k.DisplayName, k.Description)
		path, err := promptString(r, out, fmt.Sprintf("    file_path [%s]: ", hint), "")
		if err != nil {
			return req, err
		}
		req.PlacementMap = append(req.PlacementMap, cliapi.IaCGitHubPlacementEntry{
			Provider:     k.Provider,
			ResourceKind: k.ResourceKind,
			FilePath:     path,
		})
	}
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "Configured: %d of %d kinds have a file path; %d skipped.\n",
		placementSetCount(req.PlacementMap),
		len(req.PlacementMap),
		len(req.PlacementMap)-placementSetCount(req.PlacementMap))
	fmt.Fprintln(out, "")
	return req, nil
}

// runValidateWizard is the trimmed wizard for `validate` — PAT,
// repo, default branch, placement map. No persistence-only fields.
// Same one-bufio-reader discipline as runConnectWizardWithReader.
// There's no post-validate confirm prompt, so allocating the reader
// inside this function is fine.
func runValidateWizard(in io.Reader, out io.Writer) (cliapi.IaCGitHubValidateRequest, error) {
	r := bufio.NewReader(in)
	req := cliapi.IaCGitHubValidateRequest{}

	fmt.Fprintln(out, "Token scope must include `repo`.")
	token, err := readSecret(in, r, out, "GitHub Personal Access Token: ")
	if err != nil {
		return req, err
	}
	if token == "" {
		return req, fmt.Errorf("PAT cannot be empty")
	}
	req.Token = token

	repo, err := promptString(r, out, "Repository (owner/repo): ", "")
	if err != nil {
		return req, err
	}
	if !iacGitHubRepoFullNameRe.MatchString(repo) {
		return req, fmt.Errorf("repo must match owner/repo (got %q)", repo)
	}
	req.RepoFullName = repo

	branch, err := promptString(r, out,
		"Default branch (blank = server fetches): ", "")
	if err != nil {
		return req, err
	}
	req.DefaultBranch = branch

	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Placement map — file path per resource_kind. Blank to skip.")
	req.PlacementMap = make([]cliapi.IaCGitHubPlacementEntry, 0, len(iacGitHubPlacementKinds))
	for _, k := range iacGitHubPlacementKinds {
		path, err := promptString(r, out,
			fmt.Sprintf("  %s file_path: ", k.DisplayName), "")
		if err != nil {
			return req, err
		}
		req.PlacementMap = append(req.PlacementMap, cliapi.IaCGitHubPlacementEntry{
			Provider:     k.Provider,
			ResourceKind: k.ResourceKind,
			FilePath:     path,
		})
	}
	return req, nil
}

// readSecret reads a line from `in` without echoing it to `out`.
//
// Production path: when `in` is the same *os.File as os.Stdin AND
// that FD is a terminal, use term.ReadPassword on the FD so the
// terminal handles the no-echo. This is the only path that
// actually suppresses local echo.
//
// Fallback (CI, piped stdin, tests): read a line from the shared
// bufio.Reader `bufR` without echoing it ourselves. There's no
// echo to suppress when stdin is a pipe; the goal here is just to
// consume one line. The PAT must not be echoed by US — and it
// isn't, because we never write `token` to out.
//
// The PAT bytes are returned but never logged or written to out.
// bufR is the same reader the rest of the wizard uses; we never
// allocate a fresh bufio.Reader against the same underlying source
// (doing so would silently consume bytes meant for later prompts).
func readSecret(in io.Reader, bufR *bufio.Reader, out io.Writer, prompt string) (string, error) {
	fmt.Fprint(out, prompt)
	if f, ok := in.(*os.File); ok && f != nil && term.IsTerminal(int(f.Fd())) {
		raw, err := term.ReadPassword(int(f.Fd()))
		// term.ReadPassword leaves the prompt + cursor on the same
		// line because it ate the newline; emit one so the next
		// prompt starts on a fresh line.
		fmt.Fprintln(out, "")
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		return strings.TrimRight(string(raw), "\r\n"), nil
	}
	// Non-terminal fallback. Read one line from the shared reader.
	line, err := bufR.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read secret: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// promptString writes the prompt, reads one trimmed line from r,
// and returns either the trimmed input or the default if input was
// empty.
func promptString(r *bufio.Reader, out io.Writer, prompt, def string) (string, error) {
	fmt.Fprint(out, prompt)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", fmt.Errorf("read input: %w", err)
	}
	s := strings.TrimRight(line, "\r\n")
	if s == "" {
		return def, nil
	}
	return s, nil
}

// promptConfirm asks the prompt with a "(y/N)" or "(Y/n)" suffix
// depending on default, then returns the boolean answer. Anything
// other than y/yes/n/no falls back to the default.
//
// Allocates its own bufio.Reader, so callers must NOT mix it with a
// reader they're using for other prompts on the same underlying
// io.Reader — bytes would be silently lost between readers. The
// wizard paths in this file resolve that by always handing
// promptConfirm a *fresh* underlying io.Reader (cmd.InOrStdin()
// from cobra's exec) since the wizard has finished reading before
// confirm is asked. Callers that interleave prompts and confirms
// should use promptConfirmShared.
func promptConfirm(in io.Reader, out io.Writer, prompt string, def bool) (bool, error) {
	return promptConfirmShared(bufio.NewReader(in), out, prompt, def)
}

// promptConfirmShared is promptConfirm but reuses a caller-provided
// bufio.Reader. Use this when the same underlying io.Reader is
// already driving other prompts (the connect wizard does this) —
// allocating a fresh bufio.Reader on the same source would silently
// drop bytes the existing reader had already buffered.
func promptConfirmShared(r *bufio.Reader, out io.Writer, prompt string, def bool) (bool, error) {
	suffix := "(y/N)"
	if def {
		suffix = "(Y/n)"
	}
	fmt.Fprintf(out, "%s %s: ", prompt, suffix)
	line, err := r.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirm: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	default:
		return def, nil
	}
}

// --- File-mode parsing --------------------------------------------

// readConnectFile parses a YAML or JSON file into the save-connection
// request shape. Extension drives the parser: .json → encoding/json
// straight onto the struct (which carries json tags); anything else
// (.yaml/.yml or no extension) goes YAML→generic-map→JSON→struct.
//
// The two-hop YAML path keeps the wire types tag-free of yaml tags
// while still letting operators write the more readable YAML form.
// yaml.v3 doesn't honor json struct tags natively, and adding
// parallel yaml tags to every wire shape in internal/cliapi/types.go
// would be noise for one consumer.
func readConnectFile(path string) (cliapi.IaCGitHubSaveConnectionRequest, error) {
	var req cliapi.IaCGitHubSaveConnectionRequest
	raw, err := os.ReadFile(path)
	if err != nil {
		return req, fmt.Errorf("read %s: %w", path, err)
	}
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".json"):
		if err := json.Unmarshal(raw, &req); err != nil {
			return req, fmt.Errorf("parse %s as JSON: %w", path, err)
		}
	default:
		// YAML→generic-map→JSON→struct.
		var generic any
		if err := yaml.Unmarshal(raw, &generic); err != nil {
			return req, fmt.Errorf("parse %s as YAML: %w", path, err)
		}
		bridged, err := json.Marshal(yamlToJSONCompatible(generic))
		if err != nil {
			return req, fmt.Errorf("bridge %s YAML to JSON: %w", path, err)
		}
		if err := json.Unmarshal(bridged, &req); err != nil {
			return req, fmt.Errorf("parse %s as YAML (struct mismatch): %w", path, err)
		}
	}
	if req.Token == "" {
		return req, fmt.Errorf("%s: token is required", path)
	}
	if !iacGitHubRepoFullNameRe.MatchString(req.RepoFullName) {
		return req, fmt.Errorf("%s: repo_full_name must match owner/repo (got %q)",
			path, req.RepoFullName)
	}
	if req.RepoLayout == "" {
		req.RepoLayout = "multi"
	}
	if req.RepoLayout != "mono" && req.RepoLayout != "multi" {
		return req, fmt.Errorf(`%s: repo_layout must be "mono" or "multi" (got %q)`,
			path, req.RepoLayout)
	}
	return req, nil
}

// yamlToJSONCompatible converts the generic value yaml.v3 returns
// (map[any]any for objects) into a JSON-marshalable form
// (map[string]any). yaml.v3 sometimes hands back map[string]any
// directly when every key is a string; we handle both paths.
func yamlToJSONCompatible(v any) any {
	switch x := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[fmt.Sprintf("%v", k)] = yamlToJSONCompatible(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = yamlToJSONCompatible(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = yamlToJSONCompatible(val)
		}
		return out
	default:
		return x
	}
}

// --- Rendering helpers --------------------------------------------

// renderValidateResponse prints the human-friendly preflight table.
// RepoErr (when set) appears first; per-row preflight rows follow.
func renderValidateResponse(out io.Writer, resp *cliapi.IaCGitHubValidateResponse) {
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "repo:           %s\n", resp.RepoFullName)
	fmt.Fprintf(out, "default_branch: %s\n", resp.DefaultBranch)
	if resp.RepoErr != nil {
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "repo error: %s\n", resp.RepoErr.Message)
		if resp.RepoErr.SuggestedStep != "" {
			fmt.Fprintf(out, "  suggested: %s\n", resp.RepoErr.SuggestedStep)
		}
		if resp.RepoErr.DocLink != "" {
			fmt.Fprintf(out, "  see:       %s\n", resp.RepoErr.DocLink)
		}
		return
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Preflight:")
	rows := make([][]string, 0, len(resp.PreflightResults))
	for _, p := range resp.PreflightResults {
		status := "missing"
		if p.Exists {
			status = "ok"
		}
		detail := p.ShaShort
		if p.Err != nil {
			status = "error"
			detail = p.Err.Message
		}
		rows = append(rows, []string{p.ResourceKind, p.FilePath, status, detail})
	}
	table(out, []string{"RESOURCE_KIND", "FILE_PATH", "STATUS", "DETAIL"}, rows)
}

// renderAPIError converts a *cliapi.APIError into a friendly error
// message with the suggested_step + doc_link tail when present.
// Non-APIError errors pass through unchanged. The returned error is
// suitable for returning from RunE — cobra prints it on stderr.
func renderAPIError(err error) error {
	var apiErr *cliapi.APIError
	if !errors.As(err, &apiErr) {
		return err
	}
	if cliapi.Is401(err) {
		return fmt.Errorf("unauthorized — set SQUADRON_TOKEN to an API token issued from the Squadron UI")
	}
	var b strings.Builder
	if apiErr.Detail != "" {
		fmt.Fprintf(&b, "%s: %s", apiErr.Code, apiErr.Detail)
	} else {
		b.WriteString(apiErr.Code)
	}
	if apiErr.SuggestedStep != "" {
		fmt.Fprintf(&b, "\nsuggested: %s", apiErr.SuggestedStep)
	}
	if apiErr.DocLink != "" {
		fmt.Fprintf(&b, "\nsee:       %s", apiErr.DocLink)
	}
	return errors.New(b.String())
}

// placementHintFor returns the placeholder file path the in-product
// wizard uses for the given layout. {kind} is a literal that the
// caller substitutes with the resource_kind.
func placementHintFor(layout string) string {
	if layout == "mono" {
		return "environments/prod/{kind}/main.tf"
	}
	return "modules/{kind}/main.tf"
}

// placementSetCount counts how many entries have a non-empty
// file_path — the entries with empty paths are "skip" rows.
func placementSetCount(rows []cliapi.IaCGitHubPlacementEntry) int {
	n := 0
	for _, r := range rows {
		if r.FilePath != "" {
			n++
		}
	}
	return n
}
