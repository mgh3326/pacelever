package pacelever

import (
	"testing"
	"time"
)

func TestInflightDedupAndAge(t *testing.T) {
	local := []InflightJob{{"job-a", testNow.Add(-time.Hour).Format(time.RFC3339)}, {"job-a", testNow.Add(-2 * time.Hour).Format(time.RFC3339)}, {"old-job", testNow.Add(-9 * time.Hour).Format(time.RFC3339)}}
	read := Inflight(local, true, testNow)
	if read.N == nil || *read.N != 1 || len(read.Jobs) != 1 {
		t.Fatalf("count %+v", read)
	}
	read = Inflight(nil, false, testNow)
	if read.N != nil {
		t.Fatal("unknown local count converted to zero")
	}
}
func TestInflightReserve(t *testing.T) {
	in := Inputs{Now: testNow, Observations: []Observation{observation("claude", 80)}, Config: Config{Pools: map[string]PoolPolicy{"claude": {InflightReservePct: map[string]float64{"7d": 10}}}}, Inflight: map[string]*int{"claude": ptr(2)}}
	if d := Gate(in, Request{Profile: "sonnet"}); d.ExitCode != 3 {
		t.Fatalf("reserve did not block: %s", d.Reason)
	}
	in.Inflight = nil
	if d := Gate(in, Request{Profile: "sonnet"}); d.ExitCode != 0 || d.Inflight != "inflight=unknown(local)" {
		t.Fatalf("unknown count changed admission %+v", d)
	}
}
func TestFreeLimitAdvisoryAndIdentity(t *testing.T) {
	isolate(t)
	r := LimitRecord{ResetAt: testNow.Add(time.Hour).Format(time.RFC3339), ObservedAt: testNow.Format(time.RFC3339), Rung: "devin-swe2", Source: "job:fixture"}
	if e := ObserveLimit("devin", r, false, "", testNow); e != nil {
		t.Fatal(e)
	}
	if ActiveLimit("devin", testNow) == nil {
		t.Fatal("observation not active")
	}
	if e := ObserveLimit("devin", r, true, "", testNow); e != nil {
		t.Fatal(e)
	}
	if ActiveLimit("devin", testNow) != nil {
		t.Fatal("clear failed")
	}
}
func TestAreaPlacementBoundary(t *testing.T) {
	in := Inputs{Now: testNow, Observations: []Observation{observation("claude", 10)}, AreaExceptions: []AreaException{{Profile: "sonnet", Effort: "high", ModelID: "claude-sonnet-5-5", Area: "backend", Action: "exclude", Reason: "hk:task/1"}}}
	rows := Recommend(in, "A", "builder", "backend")
	found := false
	for _, r := range rows {
		if r.Profile == "sonnet" && r.Effort == "high" {
			found = true
			if r.Kind != "excluded" {
				t.Fatal("area exclusion ignored")
			}
		}
	}
	if !found {
		t.Fatal("placement disappeared")
	}
	in.AreaExceptions[0].ModelID = "old-model"
	for _, r := range Recommend(in, "A", "builder", "backend") {
		if r.Profile == "sonnet" && r.Effort == "high" && r.Kind != "candidate" {
			t.Fatal("retired model exception applied")
		}
	}
}
