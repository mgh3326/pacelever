package pacelever

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestHerdrScopedEvent(t *testing.T) {
	e, err := ParseHerdrEvent(`{"data":{"pane_id":"fixture-pane","display_agent":"Claude Code","env":{"HOME":"/example/account","CLINE_API_KEY":"never-retain"}}}`)
	if err != nil || e.Provider != "claude" || e.Selectors["CLAUDE_CONFIG_DIR"] != "/example/account/.claude" || e.Credential == "default" {
		t.Fatalf("event %+v %v", e, err)
	}
	if e.Selectors["CLINE_API_KEY"] != "" {
		t.Fatal("secret selector retained")
	}
	label := HerdrLabel(observation("claude", 10), e.Credential)
	if strings.Contains(label, "/example/account") {
		t.Fatal("selector path leaked")
	}
}
func TestExhaustionNoticeDedup(t *testing.T) {
	isolate(t)
	cfg := Config{Pools: map[string]PoolPolicy{"claude": {OnExhaust: "operator-switch"}}}
	sent := 0
	sink := func(context.Context, string, string) bool { sent++; return true }
	o := observation("claude", 99)
	if notice := ExhaustionNotice(context.Background(), cfg, o, "", testNow, sink); !strings.Contains(notice, "notified") {
		t.Fatal(notice)
	}
	_ = ExhaustionNotice(context.Background(), cfg, o, "", testNow.Add(time.Minute), sink)
	if sent != 1 {
		t.Fatal("duplicate notice")
	}
	healthy := observation("claude", 10)
	_ = ExhaustionNotice(context.Background(), cfg, healthy, "", testNow.Add(time.Hour), sink)
	_ = ExhaustionNotice(context.Background(), cfg, o, "", testNow.Add(2*time.Hour), sink)
	if sent != 2 {
		t.Fatal("recovery did not reset episode")
	}
}
func TestScopedCollectorNeverReadsDefaultSnapshot(t *testing.T) {
	isolate(t)
	if e := WriteJSON(legacyCachePath(), map[string]CacheEntry{"claude": {FetchedAt: float64(testNow.Unix()), Result: observation("claude", 1)}}); e != nil {
		t.Fatal(e)
	}
	c := Collector{CachePath: t.TempDir() + "/scoped.json", Now: func() time.Time { return testNow }, Providers: map[string]Provider{"claude": ProviderFunc(func(context.Context) Observation { return observation("claude", 20) })}}
	rows, e := c.Collect(context.Background(), []string{"claude"})
	if e != nil || *rows[0].Buckets[0].UsedPct != 20 {
		t.Fatalf("scoped cache borrowed default account: %v", e)
	}
}
