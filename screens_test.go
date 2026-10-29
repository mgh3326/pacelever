package pacelever

import (
	"os"
	"testing"
)

func TestScreenFixtures(t *testing.T) {
	for _, tt := range []struct {
		pool, file string
		want       []float64
		fail       bool
	}{
		{"kimi", "kimi_usage.txt", []float64{20, 25, 10}, false},
		{"devin", "devin_usage.txt", []float64{10, 25}, false},
		{"grok", "grok_usage.txt", []float64{24}, false},
		{"kiro", "kiro_usage.txt", []float64{60}, false},
	} {
		t.Run(tt.pool+"/"+tt.file, func(t *testing.T) {
			data, e := os.ReadFile("testdata/screens/" + tt.file)
			if e != nil {
				t.Fatal(e)
			}
			o := ParseScreen(tt.pool, string(data), testNow)
			if (o.Error != "") != tt.fail {
				t.Fatalf("error=%q", o.Error)
			}
			if len(o.Buckets) != len(tt.want) {
				t.Fatalf("got %d buckets", len(o.Buckets))
			}
			for i, n := range tt.want {
				if o.Buckets[i].UsedPct == nil || *o.Buckets[i].UsedPct != n {
					t.Fatalf("bucket %d = %v want %g", i, o.Buckets[i].UsedPct, n)
				}
			}
		})
	}
}
func TestDevinFreeExactUID(t *testing.T) {
	b, e := os.ReadFile("testdata/screens/devin_models.txt")
	if e != nil {
		t.Fatal(e)
	}
	if rows := ParseDevinModels(string(b)); len(rows) != 3 {
		t.Fatalf("free rows=%+v", rows)
	}
	if len(ParseDevinModels("fusion-swe-2-high-sidekick-swe-2-medium [Sidekick: Free]")) != 0 {
		t.Fatal("sidekick mistaken for free lead")
	}
}
