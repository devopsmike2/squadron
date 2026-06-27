package demo

import "testing"

func TestIsAzureDemoSubscription(t *testing.T) {
	if !IsAzureDemoSubscription(AzureSubscriptionID) {
		t.Fatalf("IsAzureDemoSubscription(%q) = false, want true", AzureSubscriptionID)
	}
	if IsAzureDemoSubscription("00000000-1111-2222-3333-444444444444") {
		t.Fatal("IsAzureDemoSubscription of a real subscription = true, want false")
	}
}

func TestAzureResult_ShapeAndMix(t *testing.T) {
	r := AzureResult()
	if r == nil {
		t.Fatal("AzureResult() = nil")
	}
	if r.AccountID != AzureSubscriptionID {
		t.Errorf("AccountID = %q, want %q", r.AccountID, AzureSubscriptionID)
	}
	if len(r.Compute) != 3 {
		t.Errorf("len(Compute) = %d, want 3", len(r.Compute))
	}
	if len(r.Databases) != 2 {
		t.Errorf("len(Databases) = %d, want 2", len(r.Databases))
	}
	var instr, gap, windows int
	for _, ci := range r.Compute {
		if ci.HasOTel {
			instr++
		} else {
			gap++
		}
		if ci.OSFamily == "windows" {
			windows++
		}
	}
	if instr == 0 || gap == 0 {
		t.Errorf("compute mix not balanced: %d instrumented, %d gaps", instr, gap)
	}
	if windows == 0 {
		t.Error("no Windows VM in the Azure demo inventory (OS-aware snippet path unexercised)")
	}
	var dbGap bool
	for _, db := range r.Databases {
		if db.Provider != "azure" {
			t.Errorf("db %q provider = %q, want azure", db.ResourceID, db.Provider)
		}
		if !db.SQLInsightsDiagEnabled {
			dbGap = true
		}
	}
	if !dbGap {
		t.Error("no Azure SQL DB with SQL Insights off; recommendations would be empty")
	}
}

func TestAzureRecommendationSteps(t *testing.T) {
	steps := AzureRecommendationSteps()
	if len(steps) != 3 {
		t.Fatalf("steps = %d, want 3", len(steps))
	}
	for i, st := range steps {
		if st.Name == "" || st.InlineConfigSnippet == "" || len(st.AffectedResources) == 0 {
			t.Errorf("step[%d] incomplete: name=%q snippet=%d affected=%d", i, st.Name, len(st.InlineConfigSnippet), len(st.AffectedResources))
		}
	}
}
