package pacelever

import "testing"

func TestTaskAreaKeys(t *testing.T) {
	for _, tt := range []struct {
		ref string
		id  int64
	}{{"hk:task/42", 42}, {"hk 42", 42}, {"42", 42}, {"hk42 hk43", 0}, {"https://example.invalid/pull/42", 0}, {"", 0}} {
		if got := TaskID(tt.ref); got != tt.id {
			t.Fatalf("%q=%d want %d", tt.ref, got, tt.id)
		}
	}
}
func TestAreaRoleBands(t *testing.T) {
	rows := gradeReps()
	rows[0].Grade = "A+"
	rows[1].Grade = "A"
	rows[1].Role = "verify"
	lookup := TaskAreaLookup{Areas: map[int64]string{1: "backend", 2: "frontend"}}
	cells := GradeAreas(rows, lookup)
	if len(cells) != 2 {
		t.Fatal(cells)
	}
	for _, c := range cells {
		if c.Band != "A" || c.Eligible || c.N != 1 || c.CleanPct == nil || *c.CleanPct != 100 {
			t.Fatalf("bad area cell %+v", c)
		}
	}
}
