package pacelever

import (
	"testing"
	"time"
)

func gradeReps() []Rep {
	var rows []Rep
	for i := 0; i < 2; i++ {
		rows = append(rows, Rep{ID: int64(i + 1), Profile: "example", ModelID: "example-model", TaskRef: []string{"hk:task/1", "hk:task/2"}[i], Tier: "T1", Role: "impl", Grade: "A", Rounds: 1, Completed: ptr(1), Effort: "high", RecordedAt: testNow.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)})
	}
	return rows
}
func gradeCatalog() Catalog {
	c := BundledCatalog()
	c.Rows = []CatalogRow{{Profile: "example", ModelID: "example-model", Pool: "claude", Grade: "B", Effort: "high", Gate: "default", Placed: true}}
	c.Info = CatalogInfo{Source: "server"}
	return c
}
func TestGradeEvidenceAndTampering(t *testing.T) {
	c := gradeCatalog()
	reps := gradeReps()
	p := ProposeGrades(c, reps, "server", 2, 5, 0.4)
	if len(p.Changes) != 1 || p.Changes[0].To != "A" {
		t.Fatalf("proposal %+v", p)
	}
	rows, e := ApplyGrades(p, c, reps, nil, "operator", "hk:task/1", "", testNow)
	if e != nil || rows[0].Grade != "A" {
		t.Fatalf("apply %v", e)
	}
	p.Changes[0].To = "S+"
	if _, e = ApplyGrades(p, c, reps, nil, "operator", "hk:task/1", "", testNow); e == nil {
		t.Fatal("tampered proposal applied")
	}
}
func TestGradeExactTaskEvidence(t *testing.T) {
	c := gradeCatalog()
	reps := gradeReps()
	reps[1].TaskRef = reps[0].TaskRef
	if p := ProposeGrades(c, reps, "server", 2, 5, 0.4); len(p.Changes) != 0 {
		t.Fatal("duplicate task counted twice")
	}
	reps = gradeReps()
	reps[1].ModelID = "older-model"
	if p := ProposeGrades(c, reps, "server", 2, 5, 0.4); len(p.Changes) != 0 {
		t.Fatal("other generation counted")
	}
	reps = gradeReps()
	reps[1].Effort = "medium"
	if p := ProposeGrades(c, reps, "server", 2, 5, 0.4); len(p.Changes) != 0 {
		t.Fatal("other effort counted")
	}
}
func TestGradeDegradedAndMonotonicity(t *testing.T) {
	c := gradeCatalog()
	reps := gradeReps()
	c.Info.Source = "snapshot"
	p := ProposeGrades(c, reps, "local", 2, 5, 0.4)
	if _, e := ApplyGrades(p, c, reps, nil, "operator", "hk:task/1", "", testNow); e == nil {
		t.Fatal("degraded evidence applied without reason")
	}
	c.Rows = append(c.Rows, CatalogRow{Profile: "example", Effort: "max", ModelID: "example-model", Pool: "claude", Grade: "B", Gate: "default", Placed: true})
	p = ProposeGrades(c, reps, "server", 2, 5, 0.4)
	if len(p.Changes) != 1 || !p.Changes[0].Blocked {
		t.Fatal("nonmonotonic move offered")
	}
}
