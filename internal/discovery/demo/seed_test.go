package demo

import "testing"

func TestIsDemo(t *testing.T) {
	if !IsDemo(SentinelAccountID) {
		t.Fatalf("IsDemo(%q) = false, want true", SentinelAccountID)
	}
	if IsDemo("123456789012") {
		t.Fatalf("IsDemo of a real-shaped account id = true, want false")
	}
	if IsDemo("") {
		t.Fatalf("IsDemo(empty) = true, want false")
	}
}

func TestConnection_SatisfiesStoreInvariants(t *testing.T) {
	c := Connection()
	if c.AccountID != SentinelAccountID {
		t.Errorf("AccountID = %q, want %q", c.AccountID, SentinelAccountID)
	}
	if c.Provider == "" {
		t.Error("Provider is empty; StoreConnection requires it")
	}
	if c.ConnectionType == "" {
		t.Error("ConnectionType is empty; StoreConnection requires it")
	}
	// The credstore rejects empty credential bytes — the demo connection must
	// carry placeholder ciphertext even though it is never decrypted.
	if len(c.Credentials) == 0 {
		t.Error("Credentials is empty; StoreConnection requires non-empty bytes")
	}
	if len(c.CredentialsNonce) == 0 {
		t.Error("CredentialsNonce is empty; StoreConnection requires non-empty bytes")
	}
	if len(c.Regions) == 0 {
		t.Error("Regions is empty; expected the demo region")
	}
}

func TestBuildResult_ShapeAndMix(t *testing.T) {
	r := BuildResult()
	if r == nil {
		t.Fatal("BuildResult() = nil")
	}
	if r.AccountID != SentinelAccountID {
		t.Errorf("AccountID = %q, want %q", r.AccountID, SentinelAccountID)
	}
	if r.Partial {
		t.Error("Partial = true, want false (demo is a complete sample)")
	}
	if !r.ScanCompletedAt.After(r.ScanStartedAt) {
		t.Errorf("ScanCompletedAt %v not after ScanStartedAt %v", r.ScanCompletedAt, r.ScanStartedAt)
	}

	if got := len(r.Compute); got != 5 {
		t.Errorf("len(Compute) = %d, want 5", got)
	}
	if got := len(r.Functions); got != 3 {
		t.Errorf("len(Functions) = %d, want 3", got)
	}
	if got := len(r.Databases); got != 2 {
		t.Errorf("len(Databases) = %d, want 2", got)
	}

	// The value of the demo is the *mix* of instrumented vs not: a homogeneous
	// inventory would make the recommendations trivial/empty.
	var instrumentedCompute, gapCompute int
	for _, ci := range r.Compute {
		if ci.HasOTel {
			instrumentedCompute++
		} else {
			gapCompute++
		}
	}
	if instrumentedCompute == 0 || gapCompute == 0 {
		t.Errorf("compute mix not balanced: %d instrumented, %d gaps", instrumentedCompute, gapCompute)
	}

	var fnGaps int
	for _, fn := range r.Functions {
		if !fn.HasOTelLayer {
			fnGaps++
		}
	}
	if fnGaps == 0 {
		t.Error("no function has an instrumentation gap; recommendations would be empty")
	}

	// At least one Windows instance so the OS-aware install-snippet path is
	// exercised by demo recommendations.
	var hasWindows bool
	for _, ci := range r.Compute {
		if ci.OSFamily == "windows" {
			hasWindows = true
		}
	}
	if !hasWindows {
		t.Error("no Windows compute instance in the demo inventory")
	}

	// At least one RDS with both observability levers off.
	var dbGap bool
	for _, db := range r.Databases {
		if !db.PerformanceInsightsEnabled && !db.EnhancedMonitoringEnabled {
			dbGap = true
		}
	}
	if !dbGap {
		t.Error("no database with an observability gap in the demo inventory")
	}
}
