package demo

import "testing"

func TestIsOCIDemoTenancy(t *testing.T) {
	if !IsOCIDemoTenancy(OCITenancyOCID) {
		t.Fatalf("IsOCIDemoTenancy(%q) = false, want true", OCITenancyOCID)
	}
	if IsOCIDemoTenancy("ocid1.tenancy.oc1..real") {
		t.Fatal("IsOCIDemoTenancy of a real tenancy = true, want false")
	}
}

func TestOCIResult_ShapeAndMix(t *testing.T) {
	r := OCIResult()
	if r == nil {
		t.Fatal("OCIResult() = nil")
	}
	if r.AccountID != OCITenancyOCID {
		t.Errorf("AccountID = %q, want %q", r.AccountID, OCITenancyOCID)
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
		if db.Provider != "oci" {
			t.Errorf("db %q provider = %q, want oci", db.ResourceID, db.Provider)
		}
		if !db.DatabaseManagementEnabled {
			dbGap = true
		}
	}
	if !dbGap {
		t.Error("no OCI DB with Database Management off; recommendations would be empty")
	}
}

func TestOCIRecommendationSteps(t *testing.T) {
	steps := OCIRecommendationSteps()
	if len(steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(steps))
	}
	for i, st := range steps {
		if st.Name == "" || st.InlineConfigSnippet == "" || len(st.AffectedResources) == 0 {
			t.Errorf("step[%d] incomplete", i)
		}
	}
}
