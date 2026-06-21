// Copyright (c) 2024 Squadron Contributors
// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/devopsmike2/squadron/internal/discovery/credstore"
	"github.com/devopsmike2/squadron/internal/discovery/scanner"
)

// Multi-account AWS scan orchestrator — v0.89.7a (#616 Stream 21).
//
// The backend half of multi-account support: a new endpoint
// (POST /api/v1/discovery/aws/scan-all) fans out per-account scans
// across every stored CloudConnection for provider=aws, with bounded
// concurrency, aggregates the result, and emits one
// discovery.aws.scan_all_completed audit event. Per-account
// scan_started / scan_completed events still fire from the existing
// per-account path — the orchestrator's contribution is one
// aggregate event in addition.
//
// Why this lives in internal/discovery/aws/ rather than next to the
// handler: the orchestration logic is provider-specific (the
// per-account "scan" is the AWS scanner walking EC2/Lambda/RDS/S3/
// ALB/EKS/DynamoDB), and v0.89.7a's locked design keeps the per-
// account endpoint and per-account scan_completed event completely
// unchanged. The orchestrator calls back into the handler's
// extracted reusable per-account method via the PerAccountScan
// callback; the orchestrator itself owns the fan-out, the
// concurrency bound, and the partial-failure accounting. Future
// providers (GCP, Azure) will gain sibling orchestrators in their
// own packages with the same shape.

// MaxScanAllConcurrency caps the per-request concurrency knob so an
// operator who hand-rolls a curl with a wild number doesn't fan out
// 100 simultaneous sts:AssumeRole calls and trip AWS account-wide
// throttles. 8 was chosen as the comfortable ceiling: a typical
// medium-large org has 10–30 accounts; running 8 in parallel gets
// through them in 2–4 batches without coming anywhere near sts's
// per-second limits.
const MaxScanAllConcurrency = 8

// DefaultScanAllConcurrency is the value used when the request
// omits the concurrency parameter (or passes 0). 3 keeps the
// fan-out gentle by default — operators with one or two
// connections see no observable difference from serial, and
// operators with a dozen accounts see a 3x speedup without
// risking throttle. The cap is bumped via an explicit query
// parameter, not by changing this default.
const DefaultScanAllConcurrency = 3

// ScanAllRequest is the orchestrator's input shape. Both fields are
// optional — empty Regions falls back to each connection's stored
// region list (mirroring the per-account endpoint's empty-body
// posture), and Concurrency=0 falls back to DefaultScanAllConcurrency.
type ScanAllRequest struct {
	// Regions, when non-empty, overrides every connection's stored
	// region list for this scan-all invocation. Empty means "use
	// each connection's configured regions" — the same posture as
	// the per-account endpoint's empty-body branch.
	Regions []string

	// Concurrency is the maximum number of simultaneous per-account
	// scans. Values <= 0 fall back to DefaultScanAllConcurrency;
	// values above MaxScanAllConcurrency are capped silently (the
	// HTTP handler surfaces the cap in the response so an operator
	// who asked for 10 sees they got 8).
	Concurrency int
}

// ScanAllResult is the orchestrator's output shape — one aggregate
// across all per-account scans plus the per-account result and
// failure slices for the audit payload and the HTTP response.
//
// Counts (TotalResources, TotalInstrumented, TotalUninstrumented)
// roll up across all per-service categories the per-account scans
// produce (EC2 + Lambda + RDS + S3 + ALB + EKS + DynamoDB). The
// aggregate event does NOT enumerate per-service counts — that's
// per-account event territory; operators reading the timeline see
// the per-service counts on the N per-account events and the
// rolled-up tally on the one aggregate event, linked by scan_all_id.
type ScanAllResult struct {
	// ScanAllID is the UUID the orchestrator generated at the top
	// of the fan-out. Passed down to each per-account scan via the
	// PerAccountScan callback so the per-account scan_completed
	// event carries the same trace link in its scan_all_id field.
	ScanAllID string

	// TotalAccounts is the number of CloudConnections the
	// orchestrator iterated over — succeeded + failed. Zero when
	// no AWS connections are configured.
	TotalAccounts int

	// Succeeded carries one entry per successful per-account scan.
	// Order is not guaranteed (the fan-out completes in arbitrary
	// order); callers that want a stable ordering sort by
	// AccountID after the fact.
	Succeeded []AccountScanResult

	// Failed carries one entry per per-account scan failure. Same
	// no-guaranteed-order posture as Succeeded. Empty when every
	// scan succeeded.
	Failed []AccountScanFailure

	// TotalResources is the sum of every per-service category
	// count across every successful per-account scan (EC2 +
	// Lambda + RDS + S3 + ALB + EKS + DynamoDB). Failed accounts
	// contribute nothing — the orchestrator doesn't speculate
	// about what was in an account it couldn't read.
	TotalResources int

	// TotalInstrumented is the sum of InstrumentedCount across
	// every successful per-account scan.
	TotalInstrumented int

	// TotalUninstrumented is the sum of UninstrumentedCount
	// across every successful per-account scan.
	TotalUninstrumented int

	// Partial is true when at least one per-account scan failed.
	// Mirrors the per-account scanner.Result.Partial flag at the
	// aggregate granularity — a partial aggregate means some
	// accounts' inventory is missing from this run; the operator
	// can re-scan the failed accounts from the per-account
	// endpoint without re-scanning the rest.
	Partial bool
}

// AccountScanResult is the per-account roll-up the orchestrator
// records on success. The full scanner.Result is intentionally NOT
// kept here — the orchestrator does not need to surface the
// per-resource inventory through the aggregate response (the
// Inventory tab still drives off per-account scans), and keeping
// only the counts means the orchestrator's audit payload stays
// bounded by O(accounts) rather than O(resources).
type AccountScanResult struct {
	// AccountID is the connection's primary identifier.
	AccountID string

	// ScanID is the per-account scan's UUID (the same value that
	// landed on the per-account scan_completed event).
	ScanID string

	// ResourceCount is the sum of per-service category lengths
	// for this account's scan (EC2 + Lambda + RDS + S3 + ALB +
	// EKS + DynamoDB).
	ResourceCount int

	// InstrumentedCount mirrors scanner.Result.InstrumentedCount
	// for this account.
	InstrumentedCount int

	// UninstrumentedCount mirrors scanner.Result.UninstrumentedCount
	// for this account.
	UninstrumentedCount int
}

// AccountScanFailure is the per-account failure record. ErrorCode
// is the machine-readable identifier the orchestrator pattern-
// matches on (e.g. "ScannerConstructFailed", "AccessDenied",
// "ContextCancelled"); HumanizedMessage is the operator-visible
// prose. Neither field ever contains credential material — the
// orchestrator never sees the cleartext credentials (the per-
// account scan handles decryption inside its own scope).
type AccountScanFailure struct {
	// AccountID is the connection whose scan failed.
	AccountID string

	// ErrorCode is a stable identifier the operator (or a SIEM
	// forwarder) can pattern-match on. Examples:
	// "ScannerConstructFailed" (credential decryption failed),
	// "ScannerInternal" (the scanner returned a Go error, which
	// the AWS implementation is contracted not to do),
	// "ContextCancelled" (the orchestrator's parent context was
	// cancelled mid-scan).
	ErrorCode string

	// HumanizedMessage is the operator-visible prose. Mirrors the
	// per-account handler's humanized-error convention from
	// #586/#587 — the scanner package's HumanizedError shape is
	// the source of truth for the words; the orchestrator just
	// records the result.
	HumanizedMessage string
}

// PerAccountScan is the callback the orchestrator invokes for each
// connection. The handler layer extracts the per-account scan body
// out of HandleAWSRunScan into a method that conforms to this
// signature and supplies the function value when constructing the
// Orchestrator. Indirected for two reasons:
//   - keeps the orchestrator package free of any handler-layer
//     imports (no gin, no http);
//   - lets tests substitute a stub that returns pre-canned
//     scanner.Result / AccountScanFailure pairs without dragging
//     the AWS SDK into the orchestrator's test binary.
//
// The callback receives the scanAllID the orchestrator generated
// so the per-account scan_completed event can include it in its
// payload — that's the audit-side trace link tying per-account
// events to the aggregate event.
type PerAccountScan func(ctx context.Context, conn *credstore.CloudConnection, regions []string, scanAllID string) (*scanner.Result, *AccountScanFailure)

// ConnectionLister mirrors the slice of credstore.Store the
// orchestrator depends on — strictly ListConnections, nothing else.
// Indirected so tests can return a pre-canned slice without
// implementing the full Store interface.
type ConnectionLister interface {
	ListConnections(ctx context.Context, filter credstore.ListFilter) ([]*credstore.CloudConnection, error)
}

// Orchestrator is the multi-account scan-all driver. The production
// wiring is in internal/api/handlers/discovery.go's
// scanAllOrchestrator method; tests construct an Orchestrator
// directly with stubs for PerAccountScan and ConnectionLister.
type Orchestrator struct {
	// lister is the source of truth for "which connections are
	// configured". The orchestrator filters server-side via
	// ListFilter{Provider: ProviderAWS} — the same call shape
	// HandleAWSListConnections uses (discovery.go:610).
	lister ConnectionLister

	// scan is the per-account scan callback. See PerAccountScan
	// godoc for the contract.
	scan PerAccountScan

	// newScanAllID is the UUID generator. Production wires
	// uuid.NewString; tests can substitute a deterministic
	// generator for snapshot-friendly assertions.
	newScanAllID func() string
}

// NewOrchestrator constructs an Orchestrator wired with the
// supplied dependencies. Both lister and scan are required; a nil
// for either is a programming error and the orchestrator's first
// ScanAll call will surface the nil with a clear error.
func NewOrchestrator(lister ConnectionLister, scan PerAccountScan) *Orchestrator {
	return &Orchestrator{
		lister:       lister,
		scan:         scan,
		newScanAllID: uuid.NewString,
	}
}

// WithScanAllIDFunc overrides the UUID generator. Tests that want
// to assert on the scan_all_id propagated through to the per-
// account scan use this to install a deterministic generator.
// Production callers don't need it.
func (o *Orchestrator) WithScanAllIDFunc(f func() string) *Orchestrator {
	o.newScanAllID = f
	return o
}

// ScanAll fans out per-account scans across every stored AWS
// CloudConnection, with bounded concurrency per req.Concurrency
// (or DefaultScanAllConcurrency when unset), and returns the
// aggregate result. The aggregate is built incrementally — the
// orchestrator does not buffer per-account results until after
// the fan-out completes; it appends under a mutex as each scan
// returns.
//
// Partial-failure posture: a single per-account scan failure does
// NOT abort the rest. The failed account lands in result.Failed
// with its error code + humanized message; the rest of the
// fan-out continues unaffected. The aggregate's Partial field is
// true when any account failed.
//
// Context cancellation: cancelling ctx aborts in-flight scans
// (each per-account scan is contracted to honor its context).
// In-flight scans that observe the cancellation before their
// AWS call returns surface as failures with ErrorCode
// "ContextCancelled". The orchestrator itself returns whatever
// has been collected so far — partial coverage rather than
// pretending the scan didn't happen.
//
// Zero connections is not an error. The aggregate returns with
// TotalAccounts=0, empty Succeeded + Failed, and Partial=false.
// The handler emits the scan_all_completed event anyway so an
// operator who runs scan-all on an empty install sees the event
// in the timeline (proof the operation ran, even with nothing to
// do).
func (o *Orchestrator) ScanAll(ctx context.Context, req ScanAllRequest) (*ScanAllResult, error) {
	if o.lister == nil {
		return nil, errOrchestratorNotWired("ConnectionLister")
	}
	if o.scan == nil {
		return nil, errOrchestratorNotWired("PerAccountScan")
	}

	conns, err := o.lister.ListConnections(ctx, credstore.ListFilter{
		Provider: credstore.ProviderAWS,
	})
	if err != nil {
		return nil, err
	}

	scanAllID := o.newScanAllID()
	concurrency := normalizeConcurrency(req.Concurrency)

	result := &ScanAllResult{
		ScanAllID:     scanAllID,
		TotalAccounts: 0,
	}

	// Filter out nil entries up front so the count and the
	// semaphore both see the same number of "real" units of
	// work. credstore.Store implementations are not contracted
	// to omit nils, so this is defense in depth.
	live := make([]*credstore.CloudConnection, 0, len(conns))
	for _, c := range conns {
		if c == nil {
			continue
		}
		live = append(live, c)
	}
	result.TotalAccounts = len(live)
	if len(live) == 0 {
		// Zero connections is a legitimate run — the handler
		// emits the aggregate audit event with an empty Failed
		// list and Partial=false. Bail out before constructing
		// any fan-out machinery.
		return result, nil
	}

	// Hand-rolled chan struct{} semaphore + sync.WaitGroup —
	// matches the codebase's existing bounded-fan-out idiom (no
	// errgroup precedent in production code; siem/dispatcher.go
	// and worker/pool.go both use sync.WaitGroup with channel
	// coordination). Importantly: the orchestrator does NOT use
	// the semaphore to gate "have we started" — we gate "are we
	// currently running an AWS call". The goroutine spawn is
	// O(connections); only the concurrent in-flight count is
	// bounded.
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex

	for _, conn := range live {
		wg.Add(1)
		go func(conn *credstore.CloudConnection) {
			defer wg.Done()

			// Block until a token is available — bounds in-flight
			// concurrency to len(sem) = concurrency. The select
			// also honors context cancellation so a cancelled
			// orchestrator doesn't strand goroutines waiting for
			// a token that will never come if the parent ctx is
			// done.
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				result.Failed = append(result.Failed, AccountScanFailure{
					AccountID:        conn.AccountID,
					ErrorCode:        "ContextCancelled",
					HumanizedMessage: "Scan was cancelled before it began. The operator's request context was cancelled or timed out.",
				})
				mu.Unlock()
				return
			}

			// Resolve regions: per-request override (if supplied)
			// takes precedence over the connection's stored list.
			// Empty list (after the override fallback) is what the
			// per-account scan saw in the single-account endpoint
			// before — same posture there.
			regions := req.Regions
			if len(regions) == 0 {
				regions = append([]string(nil), conn.Regions...)
			}

			scanResult, failure := o.scan(ctx, conn, regions, scanAllID)

			mu.Lock()
			defer mu.Unlock()
			if failure != nil {
				// Defensive: zero the failure's AccountID if the
				// callback forgot to set it. The orchestrator is
				// the source of truth for which connection's
				// failure this is.
				if failure.AccountID == "" {
					failure.AccountID = conn.AccountID
				}
				result.Failed = append(result.Failed, *failure)
				return
			}
			if scanResult == nil {
				// Belt-and-braces: a nil result with a nil
				// failure is a contract violation. Treat it as
				// a failure rather than panicking; the aggregate
				// loses one account's contribution but the rest
				// of the fan-out finishes.
				result.Failed = append(result.Failed, AccountScanFailure{
					AccountID:        conn.AccountID,
					ErrorCode:        "ScannerInternal",
					HumanizedMessage: "Per-account scan returned no result and no error. Re-run the scan; if it persists, file an issue.",
				})
				return
			}

			rc := totalResourceCount(scanResult)
			result.Succeeded = append(result.Succeeded, AccountScanResult{
				AccountID:           conn.AccountID,
				ScanID:              scanResult.ScanID,
				ResourceCount:       rc,
				InstrumentedCount:   scanResult.InstrumentedCount,
				UninstrumentedCount: scanResult.UninstrumentedCount,
			})
			result.TotalResources += rc
			result.TotalInstrumented += scanResult.InstrumentedCount
			result.TotalUninstrumented += scanResult.UninstrumentedCount
		}(conn)
	}

	wg.Wait()

	result.Partial = len(result.Failed) > 0
	return result, nil
}

// normalizeConcurrency clamps the request's Concurrency value into
// [1, MaxScanAllConcurrency], substituting DefaultScanAllConcurrency
// for zero / negative inputs. Pure function — the orchestrator's
// tests assert against it directly via the public ScanAll path,
// but normalizeConcurrency is callable independently from the
// handler so the HTTP layer can echo the cap back in the response.
func normalizeConcurrency(requested int) int {
	if requested <= 0 {
		return DefaultScanAllConcurrency
	}
	if requested > MaxScanAllConcurrency {
		return MaxScanAllConcurrency
	}
	return requested
}

// NormalizeConcurrency is the exported wrapper around the package-
// private normalizer so the handler layer can surface the actual
// concurrency the orchestrator used in the response without
// re-implementing the clamp logic. Same defaults + cap as the
// orchestrator's internal call.
func NormalizeConcurrency(requested int) int { return normalizeConcurrency(requested) }

// totalResourceCount sums the per-service category lengths the
// scanner.Result exposes. Mirrors the rule used in the audit
// payload's *_count fields (handlers/discovery.go:1083-1119).
// EC2 + Lambda + RDS + S3 + ALB + EKS + DynamoDB are all
// in-scope per the spec; future categories slot in here.
func totalResourceCount(r *scanner.Result) int {
	if r == nil {
		return 0
	}
	return len(r.Compute) +
		len(r.Functions) +
		len(r.Databases) +
		len(r.ObjectStores) +
		len(r.LoadBalancers) +
		len(r.Clusters) +
		len(r.DynamoDBTables)
}

// orchestratorNotWiredError is the static error type returned when
// the orchestrator was constructed with a nil dependency. The
// handler layer never produces this in production (the constructor
// always supplies the deps); the type exists so tests can assert
// on the failure mode via errors.As.
type orchestratorNotWiredError struct {
	component string
}

func (e *orchestratorNotWiredError) Error() string {
	return "orchestrator dependency not wired: " + e.component
}

func errOrchestratorNotWired(component string) error {
	return &orchestratorNotWiredError{component: component}
}
