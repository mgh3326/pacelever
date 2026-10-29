package pacelever

import (
	"fmt"
	"math"
	"strconv"
	"strings"
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

// USD keeps a sub-cent balance visible; rounding it to $0.00 would contradict
// a positive-balance admission.
func USD(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	i := strings.IndexByte(s, '.')
	if i < 0 {
		return s + ".00"
	}
	if len(s)-i == 2 {
		return s + "0"
	}
	return s
}

// latestCreditObservation returns only the entry the store still points at.
// History keeps superseded and cleared entries, and a clear removes just the
// Latest key, so a missing pointer means the balance was cleared.
func latestCreditObservation(s ManualStore, pool string) (ManualObservation, bool) {
	id := s.Latest[pool+":credit"]
	if id == "" {
		return ManualObservation{}, false
	}
	for _, m := range s.History {
		if m.ID == id && m.Pool == pool && m.Window == "credit" && m.Event == "set" && m.Credit != nil {
			return m, true
		}
	}
	return ManualObservation{}, false
}

// creditLineage walks the supersede chain of the current entry, newest first.
// A clear breaks the chain, so pre-clear balances never seed a burn rate.
func creditLineage(s ManualStore, pool string, limit int) []ManualObservation {
	byID := map[string]ManualObservation{}
	for _, m := range s.History {
		byID[m.ID] = m
	}
	cur, ok := latestCreditObservation(s, pool)
	var out []ManualObservation
	for ok && len(out) < limit {
		out = append(out, cur)
		if cur.SupersedesRef == nil {
			break
		}
		next, found := byID[*cur.SupersedesRef]
		if !found || next.Credit == nil {
			break
		}
		cur = next
	}
	return out
}

// recentCreditBurn derives a daily spend rate from the newest pair of manual
// balances. A top-up or a pair closer than an hour makes the recent rate
// unknown; older pairs are not substituted for it.
func recentCreditBurn(s ManualStore, pool string, now time.Time) *float64 {
	const recent = 7 * 24 * time.Hour
	entries := creditLineage(s, pool, 2)
	if len(entries) < 2 {
		return nil
	}
	newer, older := entries[0], entries[1]
	at, e := ParseTime(newer.MeasuredAt)
	if e != nil || now.Sub(at) > recent || now.Sub(at) < 0 {
		return nil
	}
	prev, e := ParseTime(older.MeasuredAt)
	if e != nil {
		return nil
	}
	days := at.Sub(prev).Hours() / 24
	if days < 1.0/24 || newer.Credit.RemainingUSD > older.Credit.RemainingUSD {
		return nil
	}
	burn := (older.Credit.RemainingUSD - newer.Credit.RemainingUSD) / days
	if burn < 0 {
		return nil
	}
	return &burn
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
		BalanceUSD: c.RemainingUSD,
		TotalUSD:   c.TotalUSD,
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
	if stored, e := ParseTime(m.ExpiresAt); e == nil && !stored.After(now) {
		return failure(d, Blocked, fmt.Sprintf("credit observation expired pool=%s; record the current balance", st.Pool)), true
	}
	if st.Expired {
		return failure(d, Blocked, fmt.Sprintf("credit expired pool=%s expires=%s; record the current balance", st.Pool, st.ExpiresOn)), true
	}
	if st.Exhausted {
		return failure(d, Blocked, fmt.Sprintf("credit exhausted pool=%s balance_usd=0.00 expires=%s", st.Pool, st.ExpiresOn)), true
	}
	// The same policy steps the automatic path applies: the configured cutoff,
	// the preserve-class default and any inflight reserve.
	policy := in.Config.Pool(d.ProviderID, now)
	cutoff := policy.Cutoff
	if in.Config.Pools[d.ProviderID].Cutoff == 0 && policy.Class == "preserve" {
		cutoff = 90
	}
	if st.UsedPct != nil && *st.UsedPct >= cutoff {
		return failure(d, Blocked, fmt.Sprintf("credit cutoff pool=%s used=%g%% cutoff=%g balance=$%s of $%s", st.Pool, *st.UsedPct, cutoff, USD(st.BalanceUSD), USD(st.TotalUSD))), true
	}
	if len(policy.InflightReservePct) > 0 {
		count := in.Inflight[d.ProviderID]
		if count == nil {
			d.Inflight = "inflight=unknown(local|remote)"
		} else {
			d.Inflight = fmt.Sprintf("inflight=%d", *count)
			if reserve := policy.InflightReservePct["credit"]; reserve > 0 && st.UsedPct != nil && *st.UsedPct+float64(*count)*reserve >= cutoff {
				return failure(d, Blocked, fmt.Sprintf("inflight_headroom window=credit used=%g n=%d reserve=%g cutoff=%g", *st.UsedPct, *count, reserve, cutoff)), true
			}
		}
	}
	d.OK = true
	d.ExitCode = Admitted
	d.Reason = fmt.Sprintf("credit pool=%s balance=$%s of $%s expires=%s days_left=%.1f daily_budget=$%s", st.Pool, USD(st.BalanceUSD), USD(st.TotalUSD), st.ExpiresOn, st.DaysLeft, USD(st.DailyBudgetUSD))
	if st.RecentDailyUSD != nil {
		d.Reason += fmt.Sprintf(" recent_daily=$%s", USD(*st.RecentDailyUSD))
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
