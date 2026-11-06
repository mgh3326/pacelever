package pacelever

import "testing"

// cmd-dsflash41 may hold the reviewer seat; the other credit rungs may not.
func TestCmdMayReviewOtherCreditRungsMayNot(t *testing.T) {
	row := CatalogRow{Profile: "cmd-dsflash41", Gate: "default"}
	if err := roleCheck(Request{Profile: "cmd-dsflash41", Role: "reviewer"}, row); err != nil {
		t.Fatalf("cmd-dsflash41 reviewer refused: %v", err)
	}
	if err := roleCheck(Request{Profile: "cmd-dsflash41", Role: "counsel"}, row); err == nil {
		t.Fatal("cmd-dsflash41 counsel admitted")
	}
	if err := roleCheck(Request{Profile: "pi-anthropic", Role: "reviewer"}, CatalogRow{Profile: "pi-anthropic", Gate: "default"}); err == nil {
		t.Fatal("pi- credit rung admitted as reviewer")
	}
}
