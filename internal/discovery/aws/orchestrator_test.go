// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// stubLister implements ConnectionLister with a pre-canned slice.
// Optional listErr exercises the lister-failure path.
type stubLister struct {
	conns   []*credstore.CloudConnection
	listErr error

	mu         sync.Mutex
	gotFilters []credstore.ListFilter
}

func (s *stubLister) ListConnections(_ context.Context, filter credstore.ListFilter) ([]*credstore.CloudConnection, error) {
	s.mu.Lock()
	s.gotFilters = append(s.gotFilters, filter)
	s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.conns, nil
}

// scanCall captures one invocation of the per-account scan callback —
// asserted on by every test that cares about what the orchestrator
// passed down (regions, scan_all_id, the connection itself).
type scanCall struct {
	accountID string
	regions   []string
	scanAllID string
}

// makeStubScan returns a PerAccountScan closure that records every
// invocation and dispatches per-account behavior from the supplied
// per-account map. unknown accounts fall through to a "ScannerInternal"
// failure so tests don't silently mis-spell account IDs.
//
// Behaviors:
//   - ok(*scanner.Result): return success with the supplied result
//   - fail(*AccountScanFailure): return failure with the supplied entry
//   - block(chan struct{}): hold the call until the channel is closed
//     (used for concurrency-bound tests)
//   - nilResult: return (nil, nil) — exercises the orchestrator's
//     belt-and-braces "contract violation" branch
//
// The map keys are connection AccountIDs. Behavior matches by key.
type stubBehavior struct {
	ok        *scanner.Result
	fail      *AccountScanFailure
	block     chan struct{}
	nilResult bool
}

type stubScan struct {
	mu        sync.Mutex
	calls     []scanCall
	behaviors map[string]stubBehavior

	// inflight tracks the number of in-flight scans (held tokens) —
	// concurrency-bound tests read peak() to assert the orchestrator
	// never exceeded the requested bound.
	inflight     atomic.Int32
	inflightPeak atomic.Int32
}

func (s *stubScan) call() PerAccountScan {
	return func(ctx context.Context, conn *credstore.CloudConnection, regions []string, scanAllID string) (*scanner.Result, *AccountScanFailure) {
		// Bump inflight + peak BEFORE checking behavior so the
		// block branch's high-water mark is visible.
		cur := s.inflight.Add(1)
		for {
			prev := s.inflightPeak.Load()
			if cur <= prev || s.inflightPeak.CompareAndSwap(prev, cur) {
				break
			}
		}
		defer s.inflight.Add(-1)

		s.mu.Lock()
		s.calls = append(s.calls, scanCall{
			accountID: conn.AccountID,
			regions:   append([]string(nil), regions...),
			scanAllID: scanAllID,
		})
		b, ok := s.behaviors[conn.AccountID]
		s.mu.Unlock()
		if !ok {
			return nil, &AccountScanFailure{
				AccountID:        conn.AccountID,
				ErrorCode:        "TestUnknownAccount",
				HumanizedMessage: "stubScan: no behavior configured for " + conn.AccountID,
			}
		}
		if b.block != nil {
			select {
			case <-b.block:
			case <-ctx.Done():
				return nil, &AccountScanFailure{
					AccountID:        conn.AccountID,
					ErrorCode:        "ContextCancelled",
					HumanizedMessage: "stubScan: cancelled mid-scan",
				}
			}
		}
		if b.nilResult {
			return nil, nil
		}
		if b.fail != nil {
			cp := *b.fail
			if cp.AccountID == "" {
				cp.AccountID = conn.AccountID
			}
			return nil, &cp
		}
		return b.ok, nil
	}
}

// awsConn builds a CloudConnection with the supplied account ID and
// a single us-east-1 region. Keeps the test bodies readable.
func awsConn(accountID string) *credstore.CloudConnection {
	return &credstore.CloudConnection{
		AccountID:      accountID,
		Provider:       credstore.ProviderAWS,
		ConnectionType: credstore.ConnectionAPIDiscovered,
		DisplayName:    "test-" + accountID,
		Regions:        []string{"us-east-1"},
	}
}

// scanResult builds a minimal scanner.Result with the supplied
// per-account counts. compute / instrumented / uninstrumented are
// the levers the aggregate roll-up reads.
func scanResult(scanID string, compute int, instrumented int, uninstrumented int) *scanner.Result {
	r := &scanner.Result{
		ScanID:              scanID,
		Provider:            credstore.ProviderAWS,
		ScanStartedAt:       time.Now(),
		ScanCompletedAt:     time.Now(),
		InstrumentedCount:   instrumented,
		UninstrumentedCount: uninstrumented,
	}
	for i := 0; i < compute; i++ {
		r.Compute = append(r.Compute, scanner.ComputeInstanceSnapshot{
			ResourceID: fmt.Sprintf("i-%s-%d", scanID, i),
			Region:     "us-east-1",
		})
	}
	return r
}

func TestOrchestrator_ThreeAccounts_AllSucceed_ReturnsAggregate(t *testing.T) {
	conns := []*credstore.CloudConnection{
		awsConn("111111111111"),
		awsConn("222222222222"),
		awsConn("333333333333"),
	}
	lister := &stubLister{conns: conns}
	scn := &stubScan{
		behaviors: map[string]stubBehavior{
			"111111111111": {ok: scanResult("scan-a", 3, 2, 1)},
			"222222222222": {ok: scanResult("scan-b", 5, 4, 1)},
			"333333333333": {ok: scanResult("scan-c", 2, 0, 2)},
		},
	}
	orch := NewOrchestrator(lister, scn.call())

	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if res.TotalAccounts != 3 {
		t.Errorf("TotalAccounts = %d, want 3", res.TotalAccounts)
	}
	if got := len(res.Succeeded); got != 3 {
		t.Errorf("succeeded = %d, want 3", got)
	}
	if got := len(res.Failed); got != 0 {
		t.Errorf("failed = %d, want 0", got)
	}
	if res.Partial {
		t.Errorf("Partial should be false when no account failed")
	}
	if res.TotalResources != 10 { // 3+5+2
		t.Errorf("TotalResources = %d, want 10", res.TotalResources)
	}
	if res.TotalInstrumented != 6 { // 2+4+0
		t.Errorf("TotalInstrumented = %d, want 6", res.TotalInstrumented)
	}
	if res.TotalUninstrumented != 4 { // 1+1+2
		t.Errorf("TotalUninstrumented = %d, want 4", res.TotalUninstrumented)
	}
	if res.ScanAllID == "" {
		t.Errorf("ScanAllID must be non-empty")
	}
}

func TestOrchestrator_ThreeAccounts_OneFails_RestSucceedPartial(t *testing.T) {
	conns := []*credstore.CloudConnection{
		awsConn("111111111111"),
		awsConn("222222222222"),
		awsConn("333333333333"),
	}
	lister := &stubLister{conns: conns}
	scn := &stubScan{
		behaviors: map[string]stubBehavior{
			"111111111111": {ok: scanResult("scan-a", 4, 3, 1)},
			"222222222222": {fail: &AccountScanFailure{
				ErrorCode:        "AccessDenied",
				HumanizedMessage: "Squadron's role lost permissions to ec2:DescribeInstances. Re-validate the trust policy.",
			}},
			"333333333333": {ok: scanResult("scan-c", 2, 1, 1)},
		},
	}
	orch := NewOrchestrator(lister, scn.call())

	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if !res.Partial {
		t.Errorf("Partial should be true when any account failed")
	}
	if len(res.Succeeded) != 2 {
		t.Errorf("Succeeded = %d, want 2", len(res.Succeeded))
	}
	if len(res.Failed) != 1 {
		t.Fatalf("Failed = %d, want 1", len(res.Failed))
	}
	if res.Failed[0].AccountID != "222222222222" {
		t.Errorf("Failed[0].AccountID = %q", res.Failed[0].AccountID)
	}
	if res.Failed[0].ErrorCode != "AccessDenied" {
		t.Errorf("Failed[0].ErrorCode = %q", res.Failed[0].ErrorCode)
	}
	if res.Failed[0].HumanizedMessage == "" {
		t.Errorf("Failed[0].HumanizedMessage must be non-empty")
	}
	// Aggregate counts roll up only succeeded accounts.
	if res.TotalResources != 6 { // 4+2
		t.Errorf("TotalResources = %d, want 6", res.TotalResources)
	}
	if res.TotalInstrumented != 4 { // 3+1
		t.Errorf("TotalInstrumented = %d, want 4", res.TotalInstrumented)
	}
}

func TestOrchestrator_AllFail_PartialTrue_SucceededEmpty(t *testing.T) {
	conns := []*credstore.CloudConnection{
		awsConn("111111111111"),
		awsConn("222222222222"),
	}
	lister := &stubLister{conns: conns}
	scn := &stubScan{
		behaviors: map[string]stubBehavior{
			"111111111111": {fail: &AccountScanFailure{
				ErrorCode:        "AccessDenied",
				HumanizedMessage: "role lost permissions",
			}},
			"222222222222": {fail: &AccountScanFailure{
				ErrorCode:        "Throttling",
				HumanizedMessage: "STS throttled the assume-role call",
			}},
		},
	}
	orch := NewOrchestrator(lister, scn.call())

	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if !res.Partial {
		t.Errorf("Partial should be true when every account failed")
	}
	if len(res.Succeeded) != 0 {
		t.Errorf("Succeeded = %d, want 0", len(res.Succeeded))
	}
	if len(res.Failed) != 2 {
		t.Errorf("Failed = %d, want 2", len(res.Failed))
	}
	if res.TotalResources != 0 || res.TotalInstrumented != 0 || res.TotalUninstrumented != 0 {
		t.Errorf("all-fail aggregate counts should be zero; got resources=%d instrumented=%d uninstrumented=%d",
			res.TotalResources, res.TotalInstrumented, res.TotalUninstrumented)
	}
}

func TestOrchestrator_ConcurrencyBoundRespected(t *testing.T) {
	// 6 accounts, concurrency=2 — the in-flight peak must not
	// exceed 2 at any point during the fan-out. The stubScan's
	// inflightPeak tracks the high-water mark of held tokens; we
	// block all per-account scans until the test releases them so
	// the bound is observable.
	const n = 6
	const concurrency = 2

	conns := make([]*credstore.CloudConnection, 0, n)
	behaviors := make(map[string]stubBehavior, n)
	release := make(chan struct{})
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("acct-%d", i)
		conns = append(conns, awsConn(id))
		behaviors[id] = stubBehavior{
			ok:    scanResult("scan-"+id, 1, 1, 0),
			block: release,
		}
	}
	lister := &stubLister{conns: conns}
	scn := &stubScan{behaviors: behaviors}
	orch := NewOrchestrator(lister, scn.call())

	done := make(chan struct{})
	var res *ScanAllResult
	var scanErr error
	go func() {
		res, scanErr = orch.ScanAll(context.Background(), ScanAllRequest{Concurrency: concurrency})
		close(done)
	}()

	// Wait for the inflight count to plateau at concurrency. The
	// orchestrator launches all goroutines immediately but the
	// semaphore caps how many are actually held inside the scan
	// callback. Poll up to 2 seconds — generous bound for CI.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if scn.inflight.Load() == int32(concurrency) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := scn.inflight.Load(); got != int32(concurrency) {
		// Release before failing so we don't leak goroutines.
		close(release)
		<-done
		t.Fatalf("in-flight = %d, want %d after plateau wait", got, concurrency)
	}

	// Release every scan; the fan-out completes. Peak should
	// never have exceeded concurrency.
	close(release)
	<-done
	if scanErr != nil {
		t.Fatalf("ScanAll: %v", scanErr)
	}
	if peak := scn.inflightPeak.Load(); peak > int32(concurrency) {
		t.Errorf("inflight peak = %d, exceeded bound %d", peak, concurrency)
	}
	if len(res.Succeeded) != n {
		t.Errorf("succeeded = %d, want %d", len(res.Succeeded), n)
	}
}

func TestOrchestrator_ZeroConnections_ReturnsEmptyAggregate(t *testing.T) {
	lister := &stubLister{conns: nil}
	scn := &stubScan{behaviors: map[string]stubBehavior{}}
	orch := NewOrchestrator(lister, scn.call())

	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if res.TotalAccounts != 0 {
		t.Errorf("TotalAccounts = %d, want 0", res.TotalAccounts)
	}
	if len(res.Succeeded) != 0 || len(res.Failed) != 0 {
		t.Errorf("succeeded=%d failed=%d, want 0/0", len(res.Succeeded), len(res.Failed))
	}
	if res.Partial {
		t.Errorf("Partial should be false on an empty install")
	}
	if res.ScanAllID == "" {
		t.Errorf("ScanAllID still required on empty install")
	}
	if len(scn.calls) != 0 {
		t.Errorf("per-account scan should not have been called; got %d calls", len(scn.calls))
	}
}

func TestOrchestrator_ContextCancellation_StopsInFlightScans(t *testing.T) {
	// 4 accounts, concurrency=2 — the first 2 enter the scan
	// callback and block; the orchestrator's parent context is
	// then cancelled. The blocking scans observe the cancellation
	// (their inner select on ctx.Done fires) and exit with a
	// ContextCancelled failure; the remaining 2 never acquire a
	// token because the select-on-sem case loses to the
	// ctx.Done case after cancellation.
	conns := []*credstore.CloudConnection{
		awsConn("acct-0"),
		awsConn("acct-1"),
		awsConn("acct-2"),
		awsConn("acct-3"),
	}
	block := make(chan struct{})
	defer close(block) // belt-and-braces — fire if the test path doesn't
	scn := &stubScan{behaviors: map[string]stubBehavior{
		"acct-0": {block: block, ok: scanResult("a", 1, 1, 0)},
		"acct-1": {block: block, ok: scanResult("b", 1, 1, 0)},
		"acct-2": {block: block, ok: scanResult("c", 1, 1, 0)},
		"acct-3": {block: block, ok: scanResult("d", 1, 1, 0)},
	}}
	lister := &stubLister{conns: conns}
	orch := NewOrchestrator(lister, scn.call())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	var res *ScanAllResult
	var scanErr error
	go func() {
		res, scanErr = orch.ScanAll(ctx, ScanAllRequest{Concurrency: 2})
		close(done)
	}()

	// Wait for the first 2 to be in-flight inside the block, then
	// cancel.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if scn.inflight.Load() == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	if scanErr != nil {
		t.Fatalf("ScanAll: %v (cancellation should yield a result, not an error)", scanErr)
	}
	if !res.Partial {
		t.Errorf("Partial should be true after cancellation")
	}
	// All 4 accounts should report failure under ContextCancelled.
	// The 2 in-flight see ctx.Done in their block-select; the 2
	// queued see ctx.Done in their semaphore-select.
	if len(res.Failed) != 4 {
		t.Errorf("Failed = %d, want 4 after cancellation", len(res.Failed))
	}
	if len(res.Succeeded) != 0 {
		t.Errorf("Succeeded = %d, want 0 after cancellation", len(res.Succeeded))
	}
	for _, f := range res.Failed {
		if f.ErrorCode != "ContextCancelled" {
			t.Errorf("Failed[%s].ErrorCode = %q, want ContextCancelled", f.AccountID, f.ErrorCode)
		}
	}
}

func TestOrchestrator_ConcurrencyDefaultsToThree_WhenRequestZero(t *testing.T) {
	if got := normalizeConcurrency(0); got != DefaultScanAllConcurrency {
		t.Errorf("normalizeConcurrency(0) = %d, want %d", got, DefaultScanAllConcurrency)
	}
	if got := normalizeConcurrency(-5); got != DefaultScanAllConcurrency {
		t.Errorf("normalizeConcurrency(-5) = %d, want %d", got, DefaultScanAllConcurrency)
	}
}

func TestOrchestrator_ConcurrencyCapsAtEight_WhenRequestTen(t *testing.T) {
	if got := normalizeConcurrency(10); got != MaxScanAllConcurrency {
		t.Errorf("normalizeConcurrency(10) = %d, want %d", got, MaxScanAllConcurrency)
	}
	if got := normalizeConcurrency(MaxScanAllConcurrency + 1); got != MaxScanAllConcurrency {
		t.Errorf("normalizeConcurrency(%d) = %d, want %d", MaxScanAllConcurrency+1, got, MaxScanAllConcurrency)
	}
	if got := normalizeConcurrency(5); got != 5 {
		t.Errorf("normalizeConcurrency(5) = %d, want 5 (within bounds)", got)
	}
}

func TestOrchestrator_ScanAllIDPassedToPerAccountScan(t *testing.T) {
	// The orchestrator generates one UUID at the top of the fan-out
	// and passes the same value to every per-account scan. Tests
	// install a deterministic generator so the assertion is stable.
	const fixedID = "scan-all-deterministic-uuid"
	conns := []*credstore.CloudConnection{
		awsConn("acct-1"),
		awsConn("acct-2"),
		awsConn("acct-3"),
	}
	scn := &stubScan{behaviors: map[string]stubBehavior{
		"acct-1": {ok: scanResult("s-1", 1, 1, 0)},
		"acct-2": {ok: scanResult("s-2", 1, 0, 1)},
		"acct-3": {ok: scanResult("s-3", 1, 1, 0)},
	}}
	orch := NewOrchestrator(&stubLister{conns: conns}, scn.call()).
		WithScanAllIDFunc(func() string { return fixedID })

	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if res.ScanAllID != fixedID {
		t.Errorf("result.ScanAllID = %q, want %q", res.ScanAllID, fixedID)
	}
	if len(scn.calls) != 3 {
		t.Fatalf("per-account scan calls = %d, want 3", len(scn.calls))
	}
	// Every per-account call received the same scan_all_id.
	for _, c := range scn.calls {
		if c.scanAllID != fixedID {
			t.Errorf("scan call for %s received scan_all_id %q, want %q", c.accountID, c.scanAllID, fixedID)
		}
	}
}

// --- Defense-in-depth coverage on top of the spec-required tests ----

// TestOrchestrator_NilDepsReturnError pins the constructor-side
// contract: if the orchestrator is constructed (or mutated) with a
// nil dependency, ScanAll returns a typed error rather than nil-
// derefing. Defends the handler layer's wiring posture.
func TestOrchestrator_NilDepsReturnError(t *testing.T) {
	_, err := (&Orchestrator{}).ScanAll(context.Background(), ScanAllRequest{})
	if err == nil {
		t.Fatalf("ScanAll with nil deps should return an error")
	}
	var notWired *orchestratorNotWiredError
	if !errors.As(err, &notWired) {
		t.Errorf("error type = %T (%v), want *orchestratorNotWiredError", err, err)
	}
}

// TestOrchestrator_NilResultAndNilFailure_TreatedAsFailure pins
// the belt-and-braces branch in the orchestrator: a PerAccountScan
// that returns (nil, nil) violates the contract; the orchestrator
// records it as a "ScannerInternal" failure rather than panicking.
func TestOrchestrator_NilResultAndNilFailure_TreatedAsFailure(t *testing.T) {
	conns := []*credstore.CloudConnection{awsConn("acct-1")}
	scn := &stubScan{behaviors: map[string]stubBehavior{
		"acct-1": {nilResult: true},
	}}
	orch := NewOrchestrator(&stubLister{conns: conns}, scn.call())
	res, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(res.Failed) != 1 {
		t.Fatalf("Failed = %d, want 1", len(res.Failed))
	}
	if res.Failed[0].ErrorCode != "ScannerInternal" {
		t.Errorf("Failed[0].ErrorCode = %q, want ScannerInternal", res.Failed[0].ErrorCode)
	}
}

// TestOrchestrator_ListerError_BubblesUp pins the error-on-list
// path: ListConnections errors surface as the orchestrator's
// returned error (not a panic, not a silent empty result). The
// handler converts the error to a 500 with a humanized envelope.
func TestOrchestrator_ListerError_BubblesUp(t *testing.T) {
	sentinel := errors.New("synthetic credstore failure")
	lister := &stubLister{listErr: sentinel}
	orch := NewOrchestrator(lister, (&stubScan{behaviors: map[string]stubBehavior{}}).call())
	_, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want errors.Is(%v) == true", err, sentinel)
	}
}

// TestOrchestrator_FilterIsAWS pins the lister-call shape: the
// orchestrator filters server-side via ListFilter{Provider=AWS}
// rather than fetching every provider's connections and filtering
// in memory. Defends the "scan only AWS" contract.
func TestOrchestrator_FilterIsAWS(t *testing.T) {
	lister := &stubLister{conns: nil}
	scn := &stubScan{behaviors: map[string]stubBehavior{}}
	orch := NewOrchestrator(lister, scn.call())
	_, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(lister.gotFilters) != 1 {
		t.Fatalf("lister received %d filter calls, want 1", len(lister.gotFilters))
	}
	if lister.gotFilters[0].Provider != credstore.ProviderAWS {
		t.Errorf("filter.Provider = %q, want %q", lister.gotFilters[0].Provider, credstore.ProviderAWS)
	}
}

// TestOrchestrator_PerRequestRegionsOverrideStoredList pins the
// per-call regions override: when ScanAllRequest.Regions is non-
// empty, every per-account scan receives the override list rather
// than the connection's stored Regions.
func TestOrchestrator_PerRequestRegionsOverrideStoredList(t *testing.T) {
	conns := []*credstore.CloudConnection{
		{AccountID: "acct-1", Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		{AccountID: "acct-2", Provider: credstore.ProviderAWS, Regions: []string{"eu-west-1"}},
	}
	scn := &stubScan{behaviors: map[string]stubBehavior{
		"acct-1": {ok: scanResult("s-1", 1, 1, 0)},
		"acct-2": {ok: scanResult("s-2", 1, 1, 0)},
	}}
	orch := NewOrchestrator(&stubLister{conns: conns}, scn.call())
	_, err := orch.ScanAll(context.Background(), ScanAllRequest{Regions: []string{"ap-south-1", "ap-northeast-2"}})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(scn.calls) != 2 {
		t.Fatalf("calls = %d, want 2", len(scn.calls))
	}
	for _, c := range scn.calls {
		if len(c.regions) != 2 || c.regions[0] != "ap-south-1" || c.regions[1] != "ap-northeast-2" {
			t.Errorf("scan(%s) regions = %v, want override [ap-south-1 ap-northeast-2]", c.accountID, c.regions)
		}
	}
}

// TestOrchestrator_EmptyRequestRegions_UsesPerConnectionStoredList
// pins the fallback: empty Regions in the request means each
// per-account scan receives the connection's own stored Regions.
func TestOrchestrator_EmptyRequestRegions_UsesPerConnectionStoredList(t *testing.T) {
	conns := []*credstore.CloudConnection{
		{AccountID: "acct-1", Provider: credstore.ProviderAWS, Regions: []string{"us-east-1"}},
		{AccountID: "acct-2", Provider: credstore.ProviderAWS, Regions: []string{"eu-west-1", "eu-central-1"}},
	}
	scn := &stubScan{behaviors: map[string]stubBehavior{
		"acct-1": {ok: scanResult("s-1", 1, 1, 0)},
		"acct-2": {ok: scanResult("s-2", 1, 1, 0)},
	}}
	orch := NewOrchestrator(&stubLister{conns: conns}, scn.call())
	_, err := orch.ScanAll(context.Background(), ScanAllRequest{})
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	// Sort calls by account id for stable assertions (the fan-out
	// completes in arbitrary order).
	sort.Slice(scn.calls, func(i, j int) bool { return scn.calls[i].accountID < scn.calls[j].accountID })
	if len(scn.calls[0].regions) != 1 || scn.calls[0].regions[0] != "us-east-1" {
		t.Errorf("acct-1 regions = %v, want [us-east-1]", scn.calls[0].regions)
	}
	if len(scn.calls[1].regions) != 2 || scn.calls[1].regions[0] != "eu-west-1" || scn.calls[1].regions[1] != "eu-central-1" {
		t.Errorf("acct-2 regions = %v, want [eu-west-1 eu-central-1]", scn.calls[1].regions)
	}
}

// TestTotalResourceCount pins the per-category roll-up: every
// category contributes to the total. A regression that forgets one
// of the seven categories (e.g. omitting DynamoDB after slice 4)
// shows up here.
func TestTotalResourceCount(t *testing.T) {
	r := &scanner.Result{
		Compute:        make([]scanner.ComputeInstanceSnapshot, 3),
		Functions:      make([]scanner.FunctionRuntimeSnapshot, 2),
		Databases:      make([]scanner.DatabaseInstanceSnapshot, 1),
		ObjectStores:   make([]scanner.ObjectStoreSnapshot, 4),
		LoadBalancers:  make([]scanner.LoadBalancerSnapshot, 1),
		Clusters:       make([]scanner.ClusterSnapshot, 2),
		DynamoDBTables: make([]scanner.DynamoDBTableSnapshot, 5),
	}
	if got := totalResourceCount(r); got != 18 { // 3+2+1+4+1+2+5
		t.Errorf("totalResourceCount = %d, want 18", got)
	}
	if got := totalResourceCount(nil); got != 0 {
		t.Errorf("totalResourceCount(nil) = %d, want 0", got)
	}
}
