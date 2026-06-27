package demo

import "testing"

func TestIsGCPDemoProject(t *testing.T) {
	if !IsGCPDemoProject(GCPProjectID) {
		t.Fatalf("IsGCPDemoProject(%q) = false, want true", GCPProjectID)
	}
	if IsGCPDemoProject("my-real-project") {
		t.Fatal("IsGCPDemoProject of a real project = true, want false")
	}
}

func TestGCPResult_ShapeAndMix(t *testing.T) {
	r := GCPResult()
	if r == nil {
		t.Fatal("GCPResult() = nil")
	}
	if r.AccountID != GCPProjectID {
		t.Errorf("AccountID = %q, want %q", r.AccountID, GCPProjectID)
	}
	if r.Partial {
		t.Error("Partial = true, want false")
	}
	if len(r.Compute) != 3 {
		t.Errorf("len(Compute) = %d, want 3", len(r.Compute))
	}
	if len(r.Databases) != 2 {
		t.Errorf("len(Databases) = %d, want 2", len(r.Databases))
	}
	var instr, gap int
	for _, ci := range r.Compute {
		if ci.HasOTel {
			instr++
		} else {
			gap++
		}
	}
	if instr == 0 || gap == 0 {
		t.Errorf("compute mix not balanced: %d instrumented, %d gaps", instr, gap)
	}
	var dbGap bool
	for _, db := range r.Databases {
		if db.Provider != "gcp" {
			t.Errorf("db %q provider = %q, want gcp", db.ResourceID, db.Provider)
		}
		if !db.QueryInsightsEnabled {
			dbGap = true
		}
	}
	if !dbGap {
		t.Error("no Cloud SQL with Query Insights off; recommendations would be empty")
	}
}

func TestGCPRecommendationSteps(t *testing.T) {
	steps := GCPRecommendationSteps()
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	for i, st := range steps {
		if st.Name == "" {
			t.Errorf("step[%d] empty name", i)
		}
		if st.InlineConfigSnippet == "" {
			t.Errorf("step[%d] empty snippet", i)
		}
		if len(st.AffectedResources) == 0 {
			t.Errorf("step[%d] no affected resources", i)
		}
	}
}
