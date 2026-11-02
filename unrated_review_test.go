package pacelever

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestUnratedDecodedRowsAreNotPlaced(t *testing.T) {
	var wire []map[string]any
	for _, profile := range []string{"codex-sol", "sonnet", "omp-sol", "omp-sonnet", "omp-terra61"} {
		wire = append(wire, map[string]any{"profile": profile, "effort": "high", "model_id": "example-model", "pool": "claude", "grade": "unrated", "gate": "default", "placed": true})
	}
	wire = append(wire, map[string]any{"profile": "rated-example", "effort": "high", "model_id": "rated-model", "pool": "claude", "grade": "A", "gate": "default"})
	rows, err := decodeCatalog(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Placed != (row.Grade == "A") {
			t.Errorf("decoded %s grade=%s placed=%t", row.Profile, row.Grade, row.Placed)
		}
	}
	d, err := ResolveLaunch(Catalog{Rows: rows}, Config{}, Request{Profile: "omp-terra61", Effort: "high", Role: "lead"})
	if err != nil || d.Grade != "unrated" || d.Effort != "high" {
		t.Fatalf("decoded unrated lead launch: %+v err=%v", d, err)
	}
}

func TestUnratedGateKeepsNullAuditGrade(t *testing.T) {
	for _, placed := range []bool{false, true} {
		row := explorerRow("sonnet", "high", "sonnet-model", "claude", "unrated")
		row.Placed = placed
		in := Inputs{Catalog: Catalog{Rows: []CatalogRow{row}}, Now: testNow, Observations: []Observation{observation("claude", 10)}}
		d := Gate(in, Request{Profile: "sonnet", Effort: "high", Role: "lead"})
		if !d.OK || d.Grade != nil {
			t.Fatalf("unrated gate placed=%t: ok=%t grade=%v reason=%s", placed, d.OK, d.Grade, d.Reason)
		}
		data, err := json.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		var audit map[string]any
		if err := json.Unmarshal(data, &audit); err != nil {
			t.Fatal(err)
		}
		if grade, exists := audit["grade"]; !exists || grade != nil {
			t.Fatalf("gate audit lost nullable grade contract: %s", data)
		}
	}
}

func TestUnratedWorkerNeedsMeasurementMarker(t *testing.T) {
	for _, tc := range []struct{ role, grade string }{{"", ""}, {"worker", ""}, {"tester", ""}, {"reviewer", ""}, {"worker", "A"}, {"lead", "A"}, {"consult", "A"}} {
		t.Run(tc.role+"/"+tc.grade, func(t *testing.T) {
			c := Catalog{Rows: []CatalogRow{
				{Profile: "codex-sol", Effort: "high", ModelID: "sol-model", Pool: "codex", Grade: "unrated", Gate: "default"},
				{Profile: "codex-sol", Effort: "max", ModelID: "sol-model", Pool: "codex", Grade: "S+", Gate: "default", Placed: true},
			}}
			req := Request{Profile: "codex-sol", Effort: "high", Role: tc.role, RequiredGrade: tc.grade}
			if d, err := ResolveLaunch(c, Config{}, req); err == nil || !strings.Contains(err.Error(), "e6_arm_required") {
				t.Errorf("unmarked unrated request resolved: %+v err=%v", d, err)
			}
			in := Inputs{Catalog: c, Now: testNow, Observations: []Observation{observation("codex", 10)}}
			if d := Gate(in, req); d.OK || !strings.Contains(d.Reason, "e6_arm_required") {
				t.Errorf("unmarked unrated request admitted: %+v", d)
			}
			req.E6Arm = "codex-sol@high"
			if d, err := ResolveLaunch(c, Config{}, req); err != nil || d.Grade != "unrated" || d.E6Arm == nil || *d.E6Arm != req.E6Arm {
				t.Errorf("explicit measurement request refused: %+v err=%v", d, err)
			}
			if d := Gate(in, req); !d.OK || d.E6Arm != req.E6Arm {
				t.Errorf("explicit measurement request not admitted: %+v", d)
			}
		})
	}
}

func TestUnratedRepTaskGradeRejected(t *testing.T) {
	for _, grade := range []string{"A", "", "unrated", "unknown"} {
		t.Run(grade, func(t *testing.T) {
			isolate(t)
			r := gradeReps()[0]
			r.Grade, r.TableGrade = grade, "unrated"
			got, err := (BenchClient{}).AddRep(context.Background(), r)
			if grade == "unrated" || grade == "unknown" {
				if err == nil {
					t.Fatalf("AddRep accepted task grade %q: %+v", grade, got)
				}
				stored, readErr := localReps()
				if readErr != nil || len(stored) != 0 || got.OriginID != 0 {
					t.Fatalf("invalid task grade created an intent: rows=%+v rep=%+v err=%v", stored, got, readErr)
				}
			} else if err != nil || got.TableGrade != "unrated" {
				t.Fatalf("rated task evidence on an unrated model was rejected: %+v err=%v", got, err)
			}
		})
	}
}
