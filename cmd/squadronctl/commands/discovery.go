// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/devopsmike2/squadron/internal/cliapi"
)

// v0.89.8 (#617, Stream 22) — squadronctl discovery subcommand.
// Backfills the CLI side of the v0.89.7a multi-account scan-all
// endpoint shipped in Stream 21 (#616). One subcommand for now:
// aws scan-all. The single-account scan endpoint already has a UI
// affordance and isn't part of this stream.

func newDiscoveryCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "discovery",
		Short: "Run and inspect discovery scans",
		Long: `Discovery is Squadron's scan-the-cloud-for-uninstrumented-resources
surface — the upstream of the proposer. The CLI today surfaces only
the multi-account AWS scan; the single-account scan stays in the UI
where the connection picker lives.

See docs/discovery-aws-first-time-setup.md for the AWS connection
side of the trust thesis and docs/discovery.md for the discovery
model end-to-end.`,
	}
	cmd.AddCommand(newDiscoveryAWSCommand())
	return cmd
}

func newDiscoveryAWSCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "aws",
		Short: "AWS-side discovery operations",
		Long:  `AWS-side discovery operations. Today: scan-all.`,
	}
	cmd.AddCommand(newDiscoveryAWSScanAllCommand())
	return cmd
}

// newDiscoveryAWSScanAllCommand wraps POST /api/v1/discovery/aws/scan-all.
//
// The endpoint kicks the orchestrator to scan every connected AWS
// account in parallel (capped by --concurrency, server default 3,
// server cap 8). Per-account results — succeeded and failed — come
// back in one response so a CI run can roll up the aggregate
// coverage in a single round trip.
func newDiscoveryAWSScanAllCommand() *cobra.Command {
	var (
		regions     string
		concurrency int
	)
	cmd := &cobra.Command{
		Use:   "scan-all",
		Short: "Scan every connected AWS account in parallel",
		Long: `Scan every connected AWS account in parallel and aggregate the
per-account results. Mirrors the v0.89.7a UI affordance — same
endpoint, same wire shape.

The orchestrator caps fan-out at the server's
DefaultScanAllConcurrency (3) by default and refuses to exceed
MaxScanAllConcurrency (8); the effective bound is echoed back in
the response so a CI script can confirm what actually ran.

Flags:
  --regions      Comma-separated AWS region IDs (e.g. us-east-1,us-west-2).
                 Blank = use each connection's configured regions.
  --concurrency  Max accounts scanned in parallel. 0 = server default.
                 Server caps at 8.

Output:
  Default (human) renders two tables — succeeded accounts with
  per-account counts, then failed accounts with the humanized error
  code + message — followed by a summary line with aggregate totals
  and the effective concurrency.
  -o json prints the full API response so a CI script can pipe it
  into jq.

Failure mode: scan-all is intentionally partial-friendly. A 200
response with Partial=true means at least one account scan failed
but the rest succeeded; the human output flags this. The command
returns a non-zero exit only when the entire call failed (no
accounts succeeded) or the API itself errored — matching the UI's
trust thesis ("show me what worked, show me what didn't").`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// The endpoint reads its overrides from the query string
			// (the UI's call uses the same shape); body stays empty.
			q := url.Values{}
			if normalized := normalizeRegionsCSV(regions); normalized != "" {
				q.Set("regions", normalized)
			}
			if concurrency > 0 {
				q.Set("concurrency", strconv.Itoa(concurrency))
			}

			c := newClient()
			var resp cliapi.AWSScanAllResponse
			if err := c.Do(cmd.Context(), http.MethodPost,
				"/api/v1/discovery/aws/scan-all", q, nil, &resp); err != nil {
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
			renderScanAllResponse(cmd.OutOrStdout(), &resp)

			// scan-all is intentionally partial-friendly: returning
			// 0 even when Partial is true matches the UI. The only
			// non-zero exit on a 2xx response is "zero succeeded
			// accounts" — every fan-out failed — which the caller
			// almost certainly wants to see as a CI failure.
			if len(resp.SucceededAccounts) == 0 && resp.TotalAccounts > 0 {
				return fmt.Errorf("scan-all completed but every account failed")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&regions, "regions", "",
		"Comma-separated region IDs (e.g. us-east-1,us-west-2). Blank uses the connection's configured regions.")
	cmd.Flags().IntVar(&concurrency, "concurrency", 0,
		"Max accounts scanned in parallel. 0 = server default (3); server caps at 8.")
	return cmd
}

// renderScanAllResponse prints the human-friendly tables + summary.
func renderScanAllResponse(out io.Writer, resp *cliapi.AWSScanAllResponse) {
	fmt.Fprintf(out, "scan_all_id:    %s\n", resp.ScanAllID)
	fmt.Fprintf(out, "total_accounts: %d\n", resp.TotalAccounts)
	fmt.Fprintf(out, "concurrency:    %d (effective bound)\n", resp.Concurrency)
	if resp.Partial {
		fmt.Fprintln(out, "partial:        yes (one or more account scans failed)")
	}
	fmt.Fprintln(out, "")

	if len(resp.SucceededAccounts) > 0 {
		fmt.Fprintln(out, "Succeeded:")
		rows := make([][]string, 0, len(resp.SucceededAccounts))
		for _, a := range resp.SucceededAccounts {
			rows = append(rows, []string{
				a.AccountID,
				truncate(a.ScanID, 12),
				strconv.Itoa(a.ResourceCount),
				strconv.Itoa(a.InstrumentedCount),
				strconv.Itoa(a.UninstrumentedCount),
			})
		}
		table(out, []string{"ACCOUNT_ID", "SCAN_ID", "RESOURCES", "INSTRUMENTED", "UNINSTRUMENTED"}, rows)
		fmt.Fprintln(out, "")
	}

	if len(resp.FailedAccounts) > 0 {
		fmt.Fprintln(out, "Failed:")
		rows := make([][]string, 0, len(resp.FailedAccounts))
		for _, a := range resp.FailedAccounts {
			rows = append(rows, []string{
				a.AccountID,
				a.ErrorCode,
				a.HumanizedMessage,
			})
		}
		table(out, []string{"ACCOUNT_ID", "ERROR_CODE", "MESSAGE"}, rows)
		fmt.Fprintln(out, "")
	}

	fmt.Fprintln(out, "Aggregate:")
	fmt.Fprintf(out, "  total_resources:      %d\n", resp.TotalResources)
	fmt.Fprintf(out, "  total_instrumented:   %d\n", resp.TotalInstrumented)
	fmt.Fprintf(out, "  total_uninstrumented: %d\n", resp.TotalUninstrumented)
}

// normalizeRegionsCSV trims a comma-separated region list, dropping
// empty entries, and returns the rejoined CSV (or "" if every
// entry was blank). Keeps a stray trailing comma from being passed
// through to the server.
func normalizeRegionsCSV(s string) string {
	parts := strings.Split(s, ",")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, ",")
}
