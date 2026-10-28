package pacelever

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func outcomeRep(id int64) Rep {
	r := gradeReps()[0]
	r.ID = id
	r.TaskRef = "hk:task/" + strings.Repeat("1", int(id))
	r.Rounds = 0
	r.Completed = nil
	r.FirstCIGreen = ptr(true)
	r.FindingsBySeverity = map[string]int{}
	r.RevertedWithin7d = ptr(false)
	r.TimeToMergeS = ptr(120.0)
	r.ExperimentID = "cohort-fixture"
	r.ExperimentArm = "baseline"
	return r
}
func TestStructuredOutcomeNotes(t *testing.T) {
	for _, notes := range []string{
		`{"first_ci_green":true,"findings_by_severity":{},"reverted_within_7d":false,"time_to_merge_s":90,"experiment_id":"cohort-fixture","experiment_arm":"downshift"}`,
		`{"source":"mergewake","followup":false,"evidence":"clean_pass","outcome":{"outcome_id":"fixture-pr","profile":"example","effort":"high","decision":"landed","finalized":true,"first_ci_green":true,"findings_by_severity":{"low":2},"reverted_within_7d":false,"time_to_merge_s":90,"experiment_id":"cohort-fixture","arm":"downshift"}}`,
	} {
		r := Rep{Notes: notes}
		if e := HydrateRepOutcomes(&r); e != nil {
			t.Fatal(e)
		}
		if r.FirstCIGreen == nil || !*r.FirstCIGreen || r.FindingsBySeverity == nil || r.RevertedWithin7d == nil || *r.RevertedWithin7d || r.TimeToMergeS == nil || *r.TimeToMergeS != 90 || r.ExperimentArm != "downshift" || r.ExperimentID != "cohort-fixture" || OutcomeClass(r) != "pass" {
			t.Fatalf("notes not hydrated: %+v", r)
		}
		wire := NotesForWire(r)
		roundtrip := Rep{Notes: wire}
		if e := HydrateRepOutcomes(&roundtrip); e != nil || roundtrip.ExperimentArm != "downshift" || OutcomeClass(roundtrip) != "pass" {
			t.Fatalf("roundtrip %v", e)
		}
	}
}
func TestOutcomeRule(t *testing.T) {
	for _, name := range []string{"pass", "critical", "high", "blocking", "revert", "red CI", "unknown findings", "unknown CI", "unknown revert", "unfinalized", "time alone", "rollback marker"} {
		t.Run(name, func(t *testing.T) {
			r := outcomeRep(1)
			want := "pass"
			switch name {
			case "critical", "high", "blocking":
				r.FindingsBySeverity[name] = 1
				want = "fail"
			case "revert":
				r.RevertedWithin7d = ptr(true)
				want = "fail"
			case "red CI":
				r.FirstCIGreen = ptr(false)
				want = "with_fixes"
			case "unknown findings":
				r.FindingsBySeverity = nil
				want = "pending"
			case "unknown CI":
				r.FirstCIGreen = nil
				want = "pending"
			case "unknown revert":
				r.RevertedWithin7d = nil
				want = "pending"
			case "unfinalized":
				r.OutcomeFinalized = ptr(false)
				want = "pending"
			case "time alone":
				r.FirstCIGreen = nil
				r.RevertedWithin7d = nil
				r.FindingsBySeverity = nil
				want = "pending"
			case "rollback marker":
				r.Notes = "[rollback]"
				want = "fail"
			}
			if got := OutcomeClass(r); got != want {
				t.Fatalf("%s want %s", got, want)
			}
		})
	}
}
func TestOutcomeValidation(t *testing.T) {
	for _, notes := range []string{`{"first_ci_green":"yes"}`, `{"findings_by_severity":{"high":-1}}`, `{"findings_by_severity":{"high":0.5}}`, `{"time_to_merge_s":-1}`, `{"experiment_arm":"control"}`, `{"experiment_arm":"downshift"}`, `{"first_ci_green":true,"outcome":{"first_ci_green":false}}`} {
		r := Rep{Notes: notes}
		if HydrateRepOutcomes(&r) == nil {
			t.Fatalf("invalid structured notes accepted: %s", notes)
		}
	}
}
func TestPRGradeEvidenceWithoutRounds(t *testing.T) {
	c := gradeCatalog()
	reps := []Rep{outcomeRep(1), outcomeRep(2)}
	reps[1].ExperimentArm = "downshift"
	reps[1].TimeToMergeS = ptr(360.0)
	p := ProposeGrades(c, reps, "server", 2, 5, 0.4)
	if len(p.Changes) != 1 || p.Changes[0].To != "A" || len(p.Outcomes) != 1 || p.Outcomes[0].Passes != 2 || len(p.Outcomes[0].Experiments) != 2 || p.Outcomes[0].MeanTimeToMergeS == nil || *p.Outcomes[0].MeanTimeToMergeS != 240 || p.Outcomes[0].MedianTimeToMergeS == nil || *p.Outcomes[0].MedianTimeToMergeS != 240 {
		t.Fatalf("PR evidence missing: %+v", p)
	}
	if _, e := ApplyGrades(p, c, reps, nil, "operator", "hk:task/1", "", testNow); e != nil {
		t.Fatal(e)
	}
	p.Outcomes[0].Passes = 99
	if _, e := ApplyGrades(p, c, reps, nil, "operator", "hk:task/1", "", testNow); e == nil {
		t.Fatal("tampered outcome summary applied")
	}
}
func TestPRRevertDemotesAndLatestWins(t *testing.T) {
	c := gradeCatalog()
	c.Rows[0].Grade = "A"
	older := outcomeRep(1)
	older.RecordedAt = testNow.Format(time.RFC3339)
	newer := older
	newer.ID = 2
	newer.RecordedAt = testNow.Add(time.Hour).Format(time.RFC3339)
	newer.RevertedWithin7d = ptr(true)
	p := ProposeGrades(c, []Rep{older, newer}, "server", 2, 5, 0.4)
	if len(p.Changes) != 1 || p.Changes[0].Action != "demote" || p.Changes[0].To != "B" || p.Outcomes[0].Records != 1 || p.Outcomes[0].RevertedWithin7d != 1 {
		t.Fatalf("revert not used: %+v", p)
	}
}
func TestOutcomeGradeExactRungAndTimingAdvisory(t *testing.T) {
	c := gradeCatalog()
	reps := []Rep{outcomeRep(1), outcomeRep(2)}
	reps[1].Effort = "medium"
	if p := ProposeGrades(c, reps, "server", 2, 5, 0.4); len(p.Changes) != 0 {
		t.Fatal("outcomes crossed effort")
	}
	reps = []Rep{outcomeRep(1), outcomeRep(2)}
	for i := range reps {
		reps[i].FirstCIGreen = nil
		reps[i].RevertedWithin7d = nil
		reps[i].FindingsBySeverity = nil
		reps[i].TimeToMergeS = ptr(0.001)
	}
	if p := ProposeGrades(c, reps, "server", 2, 5, 0.4); len(p.Changes) != 0 || p.Outcomes[0].Pending != 2 {
		t.Fatal("merge speed invented placement")
	}
}
func TestOutcomeWireCompatibility(t *testing.T) {
	b := fakeBench(t, func(r *http.Request) (*http.Response, error) {
		data, _ := io.ReadAll(r.Body)
		var payload struct {
			Reps []map[string]json.RawMessage `json:"reps"`
		}
		if json.Unmarshal(data, &payload) != nil {
			t.Fatal("wire JSON")
		}
		row := payload.Reps[0]
		for _, key := range []string{"first_ci_green", "findings_by_severity", "reverted_within_7d", "time_to_merge_s", "experiment_id", "experiment_arm"} {
			if _, exists := row[key]; exists {
				t.Fatalf("new field %s broke old server envelope", key)
			}
		}
		var notes string
		_ = json.Unmarshal(row["notes"], &notes)
		rep := Rep{Notes: notes}
		if e := HydrateRepOutcomes(&rep); e != nil || rep.FirstCIGreen == nil || rep.ExperimentArm != "downshift" {
			t.Fatalf("wire notes dropped outcome: %v", e)
		}
		return response(200, `{"ids":[42]}`), nil
	})
	r := outcomeRep(1)
	r.OriginID = 0
	r.ExperimentArm = "downshift"
	added, e := b.AddRep(context.Background(), r)
	if e != nil || added.ID != 42 {
		t.Fatalf("publish outcome %v", e)
	}
	local, e := localReps()
	if e != nil || len(local) != 1 || local[0].FindingsBySeverity == nil || local[0].RevertedWithin7d == nil || *local[0].RevertedWithin7d {
		t.Fatal("optional false/empty fields lost in store")
	}
}
func TestOutcomeNotesDirectApplyFingerprint(t *testing.T) {
	c := gradeCatalog()
	rows := []Rep{outcomeRep(1), outcomeRep(2)}
	for i := range rows {
		wire := NotesForWire(rows[i])
		rows[i].FirstCIGreen = nil
		rows[i].FindingsBySeverity = nil
		rows[i].RevertedWithin7d = nil
		rows[i].TimeToMergeS = nil
		rows[i].ExperimentID = ""
		rows[i].ExperimentArm = ""
		rows[i].Notes = wire
	}
	p := ProposeGrades(c, rows, "server", 2, 5, 0.4)
	if _, e := ApplyGrades(p, c, rows, nil, "operator", "hk:task/1", "", testNow); e != nil {
		t.Fatalf("raw notes changed fingerprint: %v", e)
	}
}

func TestOutcomeAttributionCannotChangeRung(t *testing.T) {
	r := Rep{Profile: "sonnet", ModelID: "claude-sonnet-5-5", Effort: "high", Notes: `{"source":"mergewake","outcome":{"profile":"sonnet","model":"claude-sonnet-5-5","effort":"medium","first_ci_green":true,"findings_by_severity":{},"reverted_within_7d":false}}`}
	if HydrateRepOutcomes(&r) == nil {
		t.Fatal("outcome changed measured effort")
	}
}
