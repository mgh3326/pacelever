package pacelever

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestGolden(t *testing.T) {
	b, e := os.ReadFile("testdata/golden.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Cases []struct {
			Request     Request         `json:"request"`
			State       string          `json:"state"`
			LaunchCode  int             `json:"launch_code"`
			Launch      json.RawMessage `json:"launch"`
			GateCode    int             `json:"gate_code"`
			Intentional string          `json:"intentional_change"`
		} `json:"cases"`
	}
	if e = json.Unmarshal(b, &data); e != nil {
		t.Fatal(e)
	}
	for _, tt := range data.Cases {
		t.Run(tt.Request.Profile+"/"+tt.Request.Effort+"/"+tt.Request.Role+"/"+tt.State, func(t *testing.T) {
			observations := []Observation{}
			for _, p := range ProviderIDs {
				o := observation(p, 10)
				o.Buckets = append(o.Buckets, Bucket{Label: "30d", Window: "30d", UsedPct: Pct(10), ResetsAt: testNow.AddDate(0, 1, 0).Format("2006-01-02T15:04:05Z"), Scope: Scope{Kind: "account"}, Horizon: "month"})
				if p == "devin" {
					for _, r := range []string{"swe-2-high", "swe-2-medium", "swe-2-max"} {
						o.Buckets = append(o.Buckets, Bucket{Label: r, Scope: Scope{Kind: "model", Name: r}, UsedPct: Pct(0)})
					}
				}
				observations = append(observations, o)
			}
			in := Inputs{Now: testNow, Observations: observations}
			switch tt.State {
			case "exhausted":
				in.Observations = []Observation{observation("claude", 99)}
			case "unmeasurable":
				in.Observations = nil
			case "unsubscribed":
				in.Config = Config{Pools: map[string]PoolPolicy{"claude": {Subscribed: ptr(false)}}}
			}
			d := Gate(in, tt.Request)
			want := tt.GateCode
			if tt.Intentional != "" {
				if tt.Request.Profile == "cmd-dsflash41" {
					want = 0
				} else {
					want = 4
				}
			}
			if d.ExitCode != want {
				t.Errorf("gate got %d (%s), want %d", d.ExitCode, d.Reason, want)
			}
			if tt.State != "" {
				return
			}
			launch, e := ResolveLaunch(BundledCatalog(), Config{}, tt.Request)
			if tt.Intentional != "" {
				if e != nil {
					t.Errorf("intentional builder admission: %v", e)
				}
				return
			}
			if tt.LaunchCode != 0 {
				if e == nil {
					t.Errorf("golden refused launch, pacelever admitted %+v", launch)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			actual, _ := json.Marshal(launch)
			var got, expected any
			_ = json.Unmarshal(actual, &got)
			_ = json.Unmarshal(tt.Launch, &expected)
			if m, ok := got.(map[string]any); ok {
				delete(m, "label")
			}
			if launch.Label.Profile != launch.Profile || launch.Label.Effort != launch.Effort || launch.Label.Arm != "baseline" {
				t.Error("baseline label missing")
			}
			if !reflect.DeepEqual(got, expected) {
				t.Errorf("launch JSON mismatch\ngot  %s\nwant %s", actual, tt.Launch)
			}
		})
	}
}
