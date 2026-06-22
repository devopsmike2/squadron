// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package verdictprompt

import (
	"strings"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/proposer/verdictsel"
)

// fixedNow is a deterministic anchor for relative `when:` math.
var fixedNow = time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)

// daysAgo returns the time N days before fixedNow.
func daysAgo(n int) time.Time {
	return fixedNow.Add(-time.Duration(n) * 24 * time.Hour)
}

func costSpikeOpts() RenderOpts {
	return RenderOpts{
		Surface:         SurfaceCostSpike,
		Now:             fixedNow,
		Header:          CostSpikeHeader,
		InstructionTail: CostSpikeInstructionTail,
	}
}

func discoveryOpts() RenderOpts {
	return RenderOpts{
		Surface:         SurfaceDiscovery,
		Now:             fixedNow,
		Header:          DiscoveryHeader,
		InstructionTail: DiscoveryInstructionTail,
	}
}

func TestRender_EmptyPool_ReturnsEmptyString(t *testing.T) {
	got := Render(nil, nil, costSpikeOpts())
	if got != "" {
		t.Fatalf("Render(nil,nil) = %q, want empty", got)
	}
	got = Render([]verdictsel.Verdict{}, []verdictsel.Verdict{}, discoveryOpts())
	if got != "" {
		t.Fatalf("Render([],[]) = %q, want empty", got)
	}
}

func TestRender_CostSpike_PinnedOnFixedInput(t *testing.T) {
	approved := []verdictsel.Verdict{
		{ID: "rlt_a1", Kind: "cost-spike-rollout", State: verdictsel.StateApproved, Timestamp: daysAgo(3), Body: "container.id was 60% of spike."},
		{ID: "rlt_a2", Kind: "cost-spike-rollout", State: verdictsel.StateApproved, Timestamp: daysAgo(5), Body: "isolated to single namespace."},
	}
	rejected := []verdictsel.Verdict{
		{ID: "rlt_r1", Kind: "cost-spike-rollout", State: verdictsel.StateRejected, Timestamp: daysAgo(2), Body: "dropped two dims in one step."},
		{ID: "rlt_r2", Kind: "cost-spike-rollout", State: verdictsel.StateRejected, Timestamp: daysAgo(4), Body: "no canary."},
	}
	want := "Prior verdicts for this group (operator decisions on past AI proposals):\n" +
		"\n" +
		"[REJECTED] kind=cost-spike-rollout\n" +
		"  reference: rollout_id=rlt_r1\n" +
		"  when: rejected 2 days ago\n" +
		"  reason: dropped two dims in one step.\n" +
		"\n" +
		"[REJECTED] kind=cost-spike-rollout\n" +
		"  reference: rollout_id=rlt_r2\n" +
		"  when: rejected 4 days ago\n" +
		"  reason: no canary.\n" +
		"\n" +
		"[APPROVED] kind=cost-spike-rollout\n" +
		"  reference: rollout_id=rlt_a1\n" +
		"  when: approved 3 days ago\n" +
		"  reason: container.id was 60% of spike.\n" +
		"\n" +
		"[APPROVED] kind=cost-spike-rollout\n" +
		"  reference: rollout_id=rlt_a2\n" +
		"  when: approved 5 days ago\n" +
		"  reason: isolated to single namespace.\n" +
		"\n" +
		CostSpikeInstructionTail
	got := Render(approved, rejected, costSpikeOpts())
	if got != want {
		t.Fatalf("cost-spike pinned mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_Discovery_PinnedOnFixedInput(t *testing.T) {
	approved := []verdictsel.Verdict{
		{ID: "https://github.com/acme/infra/pull/142", Kind: "rds-pi-em", State: verdictsel.StateMerged, Timestamp: daysAgo(3), Body: "operator merged the PR."},
	}
	rejected := []verdictsel.Verdict{
		{ID: "https://github.com/acme/infra/pull/145", Kind: "rds-pi-em", State: verdictsel.StateClosedNotMerged, Timestamp: daysAgo(1), Body: "operator closed PR without merging."},
		{ID: "rec_eks_001", Kind: "eks-observability-addon", State: verdictsel.StateOperatorExcluded, Timestamp: daysAgo(6), Body: "operator marked this recommendation kind as do-not-propose."},
	}
	excludedDate := daysAgo(6).UTC().Format("2006-01-02")
	want := "Recent operator decisions on past Squadron recommendations for this scope:\n" +
		"\n" +
		"[CLOSED_NOT_MERGED] kind=rds-pi-em\n" +
		"  reference: pr_closed=https://github.com/acme/infra/pull/145\n" +
		"  when: closed yesterday\n" +
		"  reason: operator closed PR without merging.\n" +
		"\n" +
		"[OPERATOR_EXCLUDED] kind=eks-observability-addon\n" +
		"  reference: operator_excluded=" + excludedDate + "\n" +
		"  when: excluded 6 days ago\n" +
		"  reason: operator marked this recommendation kind as do-not-propose.\n" +
		"\n" +
		"[ACCEPTED] kind=rds-pi-em\n" +
		"  reference: pr_url=https://github.com/acme/infra/pull/142\n" +
		"  when: merged 3 days ago\n" +
		"  reason: operator merged the PR.\n" +
		"\n" +
		DiscoveryInstructionTail
	got := Render(approved, rejected, discoveryOpts())
	if got != want {
		t.Fatalf("discovery pinned mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRender_StanzaFormat_ExactlyFourLines(t *testing.T) {
	verdict := verdictsel.Verdict{
		ID:        "rlt_solo",
		Kind:      "cost-spike-rollout",
		State:     verdictsel.StateApproved,
		Timestamp: daysAgo(2),
		Body:      "single verdict body",
	}
	got := Render([]verdictsel.Verdict{verdict}, nil, costSpikeOpts())
	if got == "" {
		t.Fatal("expected non-empty block for 1 verdict")
	}
	// Strip header (line 1), the blank that follows, the trailing
	// blank-line + InstructionTail. What remains must be exactly 4
	// content lines from the stanza.
	body := strings.TrimPrefix(got, CostSpikeHeader+"\n\n")
	body = strings.TrimSuffix(body, "\n"+CostSpikeInstructionTail)
	// Remove the blank line that separates the stanza from the tail.
	body = strings.TrimSuffix(body, "\n")
	lines := strings.Split(body, "\n")
	if len(lines) != 4 {
		t.Fatalf("stanza had %d content lines, want 4: %#v", len(lines), lines)
	}
	expectPrefixes := []string{"[APPROVED] kind=", "  reference: ", "  when: ", "  reason: "}
	for i, prefix := range expectPrefixes {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Fatalf("line %d %q missing prefix %q", i, lines[i], prefix)
		}
	}
}

func TestRender_LongBody_TruncatedTo240(t *testing.T) {
	// "x" is non-hex so the hex_fingerprint pattern doesn't fire;
	// the body survives RedactSecrets unmodified and we exercise
	// the length-truncation path cleanly.
	long := strings.Repeat("x", 300)
	verdict := verdictsel.Verdict{
		ID:        "rlt_long",
		Kind:      "cost-spike-rollout",
		State:     verdictsel.StateApproved,
		Timestamp: daysAgo(1),
		Body:      long,
	}
	got := Render([]verdictsel.Verdict{verdict}, nil, costSpikeOpts())
	// Find the reason line.
	var reasonLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "  reason: ") {
			reasonLine = strings.TrimPrefix(line, "  reason: ")
			break
		}
	}
	if reasonLine == "" {
		t.Fatal("no reason line emitted")
	}
	if !strings.HasSuffix(reasonLine, "...") {
		t.Fatalf("reason line should end with ellipsis when truncated; got %q", reasonLine)
	}
	if len(reasonLine) != reasonMaxChars {
		t.Fatalf("reason line len = %d, want %d", len(reasonLine), reasonMaxChars)
	}
}

func TestRender_RedactionApplied(t *testing.T) {
	// The bearer_token redaction pattern is
	//   (?i)bearer\s+[A-Za-z0-9._\-]{16,}
	// so a literal "Bearer abc123def456ghi789jkl" is matched and
	// replaced by <redacted:bearer_token>.
	token := "Bearer abc123def456ghi789jkl012mno345pqr678stu901vwx234"
	verdict := verdictsel.Verdict{
		ID:        "rlt_redact",
		Kind:      "cost-spike-rollout",
		State:     verdictsel.StateRejected,
		Timestamp: daysAgo(1),
		Body:      "operator pasted secret in notes: " + token,
	}
	got := Render(nil, []verdictsel.Verdict{verdict}, costSpikeOpts())
	if strings.Contains(got, token) {
		t.Fatalf("output contains raw bearer token; redaction did not apply:\n%s", got)
	}
	if !strings.Contains(got, "<redacted:bearer_token>") {
		t.Fatalf("expected redaction placeholder in output:\n%s", got)
	}
}
