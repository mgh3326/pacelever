package pacelever

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// A credit balance is hand-entered from the operator's Console view. Nothing
// here is probed, and a missing or stale balance never invents a number.
type CreditStatus struct {
	Pool           string   `json:"pool"`
	BalanceUSD     float64  `json:"balance_usd"`
	TotalUSD       float64  `json:"total_usd"`
	UsedPct        *float64 `json:"used_pct"`
	ExpiresOn      string   `json:"expires_at"`
	DaysLeft       float64  `json:"days_left"`
	DailyBudgetUSD float64  `json:"daily_budget_usd"`
	RecentDailyUSD *float64 `json:"recent_daily_usd"`
	Waste          bool     `json:"waste"`
	Exhausted      bool     `json:"exhausted"`
	Expired        bool     `json:"expired"`
	Stale          bool     `json:"stale"`
	AgeS           float64  `json:"age_s"`
	MaxAgeS        float64  `json:"max_age_s"`
	MeasuredAt     string   `json:"measured_at"`
	UpdatedAt      string   `json:"updated_at"`
}

// ParseCreditExpiry accepts the historical date-only form and RFC3339.
func ParseCreditExpiry(s string) (time.Time, error) {
	if t, e := time.Parse("2006-01-02", s); e == nil {
		return t, nil
	}
	return ParseTime(s)
}

func dateOnly(s string) bool {
	_, e := time.Parse("2006-01-02", s)
	return e == nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func latestCreditObservation(s ManualStore, pool string) (ManualObservation, bool) {
	id := s.Latest[pool+":credit"]
	var newest ManualObservation
	var newestAt time.Time
	found := false
	for _, m := range s.History {
		if m.Pool != pool || m.Window != "credit" || m.Event != "set" || m.Credit == nil {
			continue
		}
		if id != "" && m.ID == id {
			return m, true
		}
		at, e := ParseTime(m.MeasuredAt)
		if e != nil {
			continue
		}
		if !found || at.After(newestAt) {
			newest, newestAt, found = m, at, true
		}
	}
	return newest, found
}

// recentCreditBurn derives a daily spend rate from the two most recent manual
// balances. Top-ups and pairs closer than an hour are ignored; an unknown rate
// stays unknown.
func recentCreditBurn(s ManualStore, pool string, now time.Time) *float64 {
	const recent = 7 * 24 * time.Hour
	var entries []ManualObservation
	for _, m := range s.History {
		if m.Pool != pool || m.Window != "credit" || m.Event != "set" || m.Credit == nil {
			continue
		}
		if _, e := ParseTime(m.MeasuredAt); e != nil {
			continue
		}
		entries = append(entries, m)
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].MeasuredAt < entries[j].MeasuredAt })
	for i := len(entries) - 1; i > 0; i-- {
		newer, older := entries[i], entries[i-1]
		at, _ := ParseTime(newer.MeasuredAt)
		if now.Sub(at) > recent || now.Sub(at) < 0 {
			continue
		}
		prev, _ := ParseTime(older.MeasuredAt)
		days := at.Sub(prev).Hours() / 24
		if days < 1.0/24 || newer.Credit.RemainingUSD > older.Credit.RemainingUSD {
			continue
		}
		burn := (older.Credit.RemainingUSD - newer.Credit.RemainingUSD) / days
		if burn < 0 {
			continue
		}
		return &burn
	}
	return nil
}

func creditStatusFor(s ManualStore, pool string, now time.Time) (*CreditStatus, bool) {
	m, ok := latestCreditObservation(s, pool)
	if !ok {
		return nil, false
	}
	c := m.Credit
	if c.TotalUSD <= 0 || c.RemainingUSD < 0 || c.RemainingUSD > c.TotalUSD {
		return nil, false
	}
	expiry, e := ParseCreditExpiry(c.ExpiresOn)
	if e != nil {
		return nil, false
	}
	measured, e := ParseTime(m.MeasuredAt)
	if e != nil {
		return nil, false
	}
	days := expiry.Sub(now).Hours() / 24
	st := &CreditStatus{
		Pool:       pool,
		BalanceUSD: round2(c.RemainingUSD),
		TotalUSD:   round2(c.TotalUSD),
		UsedPct:    Pct(100 * (1 - c.RemainingUSD/c.TotalUSD)),
		ExpiresOn:  c.ExpiresOn,
		DaysLeft:   round2(days),
		MeasuredAt: m.MeasuredAt,
		UpdatedAt:  m.EnteredAt,
	}
	if st.UpdatedAt == "" {
		st.UpdatedAt = m.MeasuredAt
	}
	st.AgeS = round2(now.Sub(measured).Seconds())
	if st.AgeS < 0 {
		st.AgeS = 0
	}
	st.Exhausted = c.RemainingUSD <= 0
	st.Expired = !expiry.After(now)
	if days > 0 {
		st.DailyBudgetUSD = round2(c.RemainingUSD / days)
	}
	if burn := recentCreditBurn(s, pool, now); burn != nil {
		st.RecentDailyUSD = ptr(round2(*burn))
	}
	st.Waste = !st.Exhausted && !st.Expired && st.RecentDailyUSD != nil && st.DailyBudgetUSD > *st.RecentDailyUSD
	return st, true
}

// CreditStatus reports the configured view, including max-age staleness.
func (in Inputs) CreditStatus(pool string) (*CreditStatus, bool) {
	st, ok := creditStatusFor(in.Manual, pool, in.clock())
	if !ok {
		return nil, false
	}
	st.MaxAgeS = in.Config.CreditMaxAge(pool).Seconds()
	st.Stale = st.AgeS > st.MaxAgeS
	return st, true
}

// creditGate decides a credit-pool admission from the hand-entered balance. It
// reports false when no dollar balance is recorded, leaving the percentage
// path (a shared or legacy entry) in charge.
func (in Inputs) creditGate(d Decision, now time.Time) (Decision, bool) {
	st, ok := in.CreditStatus(d.ProviderID)
	if !ok {
		return d, false
	}
	m, _ := latestCreditObservation(in.Manual, d.ProviderID)
	d.Credit = st
	d.UsedPct = st.UsedPct
	d.Source = ptr("operator")
	d.SourceVerification = ptr("unverified")
	label := m.SourceLabel
	if label == "" {
		label = "self-reported manual observation"
	}
	d.SourceLabel = &label
	d.ManualObservationIDs = []string{m.ID}
	d.ManualObservations = []ManualObservation{m}
	if measured, e := ParseTime(m.MeasuredAt); e == nil {
		d.MeasuredAt = ptr(measured.UTC().Format(time.RFC3339Nano))
	}
	if expiry, e := ParseCreditExpiry(m.Credit.ExpiresOn); e == nil {
		d.ExpiresAt = ptr(expiry.UTC().Format(time.RFC3339Nano))
		d.RemainingEffectS = ptr(expiry.Sub(now).Seconds())
	}
	d.ObservedAgeS = ptr(st.AgeS)
	if st.Expired {
		return failure(d, Blocked, fmt.Sprintf("credit expired pool=%s expires=%s; record the current balance", st.Pool, st.ExpiresOn)), true
	}
	if st.Exhausted {
		return failure(d, Blocked, fmt.Sprintf("credit exhausted pool=%s balance_usd=0.00 expires=%s", st.Pool, st.ExpiresOn)), true
	}
	d.OK = true
	d.ExitCode = Admitted
	d.Reason = fmt.Sprintf("credit pool=%s balance=$%.2f of $%.2f expires=%s days_left=%.1f daily_budget=$%.2f", st.Pool, st.BalanceUSD, st.TotalUSD, st.ExpiresOn, st.DaysLeft, st.DailyBudgetUSD)
	if st.RecentDailyUSD != nil {
		d.Reason += fmt.Sprintf(" recent_daily=$%.2f", *st.RecentDailyUSD)
	}
	if st.Waste {
		d.Reason += " waste=true paced budget exceeds recent use; spend it before expiry"
	}
	if st.Stale {
		d.StaleAccepted = true
		d.Reason += fmt.Sprintf(" credit_stale=true age=%.1fd max_age=%.1fd; balance unverified, update it by hand", st.AgeS/86400, st.MaxAgeS/86400)
	}
	return d, true
}
