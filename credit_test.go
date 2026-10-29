package pacelever

import (
	"testing"
	"time"
)

func creditEntry(id string, at time.Time, balance float64, expires string) ManualObservation {
	return ManualObservation{ID: id, Event: "set", Pool: "anthropic-api", Window: "credit", MeasuredAt: at.Format(time.RFC3339), EnteredAt: at.Format(time.RFC3339), Credit: &Credit{RemainingUSD: balance, TotalUSD: 200, ExpiresOn: expires}}
}

func creditStore(entries ...ManualObservation) ManualStore {
	s := ManualStore{Latest: map[string]string{}}
	for _, m := range entries {
		if prior := s.Latest[m.Pool+":"+m.Window]; prior != "" {
			m.SupersedesRef = &prior
		}
		s.History = append(s.History, m)
		s.Latest[m.Pool+":"+m.Window] = m.ID
	}
	return s
}

func TestCreditStatusPacing(t *testing.T) {
	store := creditStore(
		creditEntry("old", testNow.Add(-72*time.Hour), 170, "2026-11-16"),
		creditEntry("new", testNow.Add(-24*time.Hour), 150, "2026-11-16"),
	)
	in := Inputs{Now: testNow, Manual: store}
	got, ok := in.CreditStatus("anthropic-api")
	if !ok {
		t.Fatal("credit status missing")
	}
	if got.BalanceUSD != 150 || got.TotalUSD != 200 || got.UsedPct == nil || *got.UsedPct != 25 {
		t.Fatalf("balance fields %+v", got)
	}
	if got.DaysLeft != 19 || got.DailyBudgetUSD != 7.89 {
		t.Fatalf("pacing days=%v budget=%v", got.DaysLeft, got.DailyBudgetUSD)
	}
	if got.RecentDailyUSD == nil || *got.RecentDailyUSD != 10 {
		t.Fatalf("recent use %+v", got.RecentDailyUSD)
	}
	if got.Waste || got.Stale || got.Exhausted || got.Expired {
		t.Fatalf("unexpected flags %+v", got)
	}

	store = creditStore(creditEntry("new", testNow.Add(-24*time.Hour), 150, "2026-11-02"))
	got, _ = Inputs{Now: testNow, Manual: store}.CreditStatus("anthropic-api")
	if got.RecentDailyUSD != nil {
		t.Fatalf("single entry invented a burn rate %v", *got.RecentDailyUSD)
	}
	if got.Waste {
		t.Fatal("waste without a recent-use baseline")
	}

	store = creditStore(
		creditEntry("old", testNow.Add(-72*time.Hour), 170, "2026-11-02"),
		creditEntry("new", testNow.Add(-24*time.Hour), 150, "2026-11-02"),
	)
	got, _ = Inputs{Now: testNow, Manual: store}.CreditStatus("anthropic-api")
	if got.DaysLeft != 5 || got.DailyBudgetUSD != 30 || !got.Waste {
		t.Fatalf("paced budget above recent use was not flagged: %+v", got)
	}
}

func TestCreditGate(t *testing.T) {
	req := Request{Profile: "pi-sonnet55", Effort: "high", Role: "worker"}
	fresh := creditEntry("new", testNow.Add(-time.Hour), 5, "2026-11-16")

	d := Gate(Inputs{Now: testNow, Manual: creditStore(fresh)}, req)
	if d.ExitCode != Admitted || !d.OK || d.Credit == nil {
		t.Fatalf("positive balance refused: %d %s", d.ExitCode, d.Reason)
	}
	if d.Credit.BalanceUSD != 5 || d.UsedPct == nil || *d.UsedPct != 97.5 {
		t.Fatalf("credit decision fields %+v", d)
	}

	if d = Gate(Inputs{Now: testNow, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 1, "2026-11-16"))}, req); d.ExitCode != Blocked || d.Credit == nil {
		t.Fatalf("balance above the default cutoff admitted: %d %s", d.ExitCode, d.Reason)
	}
	if d = Gate(Inputs{Now: testNow, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 0, "2026-11-16"))}, req); d.ExitCode != Blocked {
		t.Fatalf("exhausted credit admitted: %s", d.Reason)
	}
	if d = Gate(Inputs{Now: testNow, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 20, "2026-10-27"))}, req); d.ExitCode != Blocked {
		t.Fatalf("expired credit admitted: %s", d.Reason)
	}
	if d = Gate(Inputs{Now: testNow}, req); d.ExitCode != Unmeasurable {
		t.Fatalf("missing balance exit %d", d.ExitCode)
	}

	// An operator cutoff applies to the balance-derived used percentage.
	cut := Config{Pools: map[string]PoolPolicy{"anthropic-api": {Cutoff: 80}}}
	if d = Gate(Inputs{Now: testNow, Config: cut, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 30, "2026-11-16"))}, req); d.ExitCode != Blocked {
		t.Fatalf("operator cutoff ignored: %s", d.Reason)
	}
	if d = Gate(Inputs{Now: testNow, Config: cut, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 45, "2026-11-16"))}, req); d.ExitCode != Admitted {
		t.Fatalf("cutoff blocked a balance below it: %s", d.Reason)
	}

	// preserve without an explicit cutoff uses the same 90% default.
	preserve := Config{Pools: map[string]PoolPolicy{"anthropic-api": {Class: "preserve"}}}
	if d = Gate(Inputs{Now: testNow, Config: preserve, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 15, "2026-11-16"))}, req); d.ExitCode != Blocked {
		t.Fatalf("preserve default ignored: %s", d.Reason)
	}
	if d = Gate(Inputs{Now: testNow, Config: preserve, Manual: creditStore(creditEntry("new", testNow.Add(-time.Hour), 25, "2026-11-16"))}, req); d.ExitCode != Admitted {
		t.Fatalf("preserve blocked a balance above 10%%: %s", d.Reason)
	}

	// A configured inflight reserve participates like any other pool.
	one := 1
	reserved := Config{Pools: map[string]PoolPolicy{"anthropic-api": {InflightReservePct: map[string]float64{"credit": 3}}}}
	in := Inputs{Now: testNow, Config: reserved, Manual: creditStore(fresh), Inflight: map[string]*int{"anthropic-api": &one}}
	if d = Gate(in, req); d.ExitCode != Blocked || d.Inflight != "inflight=1" {
		t.Fatalf("inflight reserve ignored: %d %s %s", d.ExitCode, d.Inflight, d.Reason)
	}

	stale := creditEntry("new", testNow.Add(-96*time.Hour), 150, "2026-11-16")
	d = Gate(Inputs{Now: testNow, Manual: creditStore(stale)}, req)
	if d.ExitCode != Admitted || d.Credit == nil || !d.Credit.Stale || !d.StaleAccepted {
		t.Fatalf("stale entry not admitted with a warning: %d %s", d.ExitCode, d.Reason)
	}
	cfg := Config{Pools: map[string]PoolPolicy{"anthropic-api": {CreditMaxAgeS: 7 * 86400}}}
	d = Gate(Inputs{Now: testNow, Config: cfg, Manual: creditStore(stale)}, req)
	if d.ExitCode != Admitted || d.Credit == nil || d.Credit.Stale {
		t.Fatalf("configured max age ignored: %s", d.Reason)
	}
}

func TestCreditClearBreaksPacing(t *testing.T) {
	store := creditStore(
		creditEntry("old", testNow.Add(-72*time.Hour), 170, "2026-11-16"),
		creditEntry("new", testNow.Add(-24*time.Hour), 150, "2026-11-16"),
	)
	delete(store.Latest, "anthropic-api:credit")
	if _, ok := (Inputs{Now: testNow, Manual: store}).CreditStatus("anthropic-api"); ok {
		t.Fatal("cleared balance revived from history")
	}
	if d := Gate(Inputs{Now: testNow, Manual: store}, Request{Profile: "pi-sonnet55", Effort: "high", Role: "worker"}); d.ExitCode != Unmeasurable {
		t.Fatalf("cleared credit admitted: %d %s", d.ExitCode, d.Reason)
	}

	// A balance set after a clear starts a new lineage: the pre-clear entry
	// cannot seed a burn rate for it.
	store.History = append(store.History, creditEntry("fresh", testNow.Add(-time.Hour), 150, "2026-11-16"))
	store.Latest["anthropic-api:credit"] = "fresh"
	got, ok := (Inputs{Now: testNow, Manual: store}).CreditStatus("anthropic-api")
	if !ok || got.RecentDailyUSD != nil || got.Waste {
		t.Fatalf("pre-clear balance seeded the new lineage: %+v", got)
	}
}

func TestCreditManualPreparation(t *testing.T) {
	m, e := prepareManual(ManualObservation{Pool: "anthropic-api", MeasuredAt: testNow.Format(time.RFC3339), Reason: "console", Credit: &Credit{RemainingUSD: 150, TotalUSD: 200, ExpiresOn: "2026-11-16T00:00:00Z"}}, 0, testNow)
	if e != nil {
		t.Fatal(e)
	}
	if m.Window != "credit" || m.Credit.ExpiresOn != "2026-11-16T00:00:00Z" || m.UsedPct == nil || *m.UsedPct != 25 {
		t.Fatalf("credit preparation %+v", m)
	}
	if expiry, e := ParseTime(m.ExpiresAt); e != nil || expiry != testNow.AddDate(0, 0, 19) {
		t.Fatalf("credit observation life %s", m.ExpiresAt)
	}
	if _, e = prepareManual(ManualObservation{Pool: "anthropic-api", MeasuredAt: testNow.Format(time.RFC3339), Reason: "console", Credit: &Credit{RemainingUSD: 250, TotalUSD: 200, ExpiresOn: "2026-11-16"}}, 0, testNow); e == nil {
		t.Fatal("balance above total accepted")
	}
}
