package pacelever

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type Inputs struct {
	Catalog      Catalog
	Config       Config
	Observations []Observation
	Manual       ManualStore
	Now          time.Time
	// nil denotes an unknown count, which is visible and never invents a reserve.
	Inflight       map[string]*int
	AreaExceptions []AreaException
	// FreeLimit is the recorded SWE-2 free-limit exceed, if any. The gate
	// refuses the free rungs until its reset time passes.
	FreeLimit *LimitRecord
}

// FreeAvailable is the gate wording for an admitted SWE-2 free rung.
const FreeAvailable = "available (free)"

type AreaException struct {
	Profile string `json:"profile"`
	Effort  string `json:"effort"`
	ModelID string `json:"model_id"`
	Area    string `json:"area"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
}

func (in Inputs) clock() time.Time {
	if in.Now.IsZero() {
		return time.Now().UTC()
	}
	return in.Now
}
func (in Inputs) catalog() Catalog {
	if in.Catalog.Defaults == nil {
		c := BundledCatalog()
		if in.Catalog.Rows != nil {
			c.Rows = in.Catalog.Rows
			c.Info = in.Catalog.Info
		}
		return c
	}
	return in.Catalog
}
func decision(req Request) Decision {
	return Decision{Profile: req.Profile, ExitCode: Blocked, Alternatives: []string{}, ManualObservationIDs: []string{}, ManualObservations: []ManualObservation{}, MissingWindows: []string{}}
}
func failure(d Decision, code int, reason string) Decision {
	d.OK = false
	d.ExitCode = code
	d.Reason = reason
	d.Unmeasurable = code == Unmeasurable
	d.RoleDenied = code == RoleDenied
	d.Unsubscribed = code == Unsubscribed
	return d
}

// Gate evaluates one admission. It never sends a message, writes state or probes.
func Gate(in Inputs, req Request) Decision {
	d := gate(in, req)
	if !d.OK && d.Grade != nil && d.ExitCode != RoleDenied && d.ExitCode != Unsubscribed {
		for _, r := range Recommend(in, *d.Grade, req.Role, req.Area) {
			if r.Profile != req.Profile && r.Kind == "candidate" {
				d.Alternatives = append(d.Alternatives, r.LaunchSpelling())
			}
		}
	}
	return d
}
func gate(in Inputs, req Request) Decision {
	d := decision(req)
	now := in.clock()
	c := in.catalog()
	canonical := c.Canonical(req.Profile)
	rows := c.profileRows(canonical)
	var hint CatalogRow
	if len(rows) > 0 {
		hint = rows[0]
	}
	d.ProviderID = hint.Pool
	req.Profile = canonical
	// Consult-role checks precede subscription and request-shape checks.
	if !credit(canonical) {
		if e := roleCheck(req, hint); e != nil {
			return failure(d, ExitCode(e), e.Error())
		}
	}
	if !in.Config.Subscribed(canonical, hint.Pool) {
		return failure(d, Unsubscribed, "unsubscribed: profile or pool subscription disabled")
	}
	if req.RequestedBy != "" && req.OperatorRequest == "" {
		return failure(d, Blocked, "requested_by_requires_operator_request")
	}
	if req.OperatorRequest != "" && !validRef(req.OperatorRequest) {
		return failure(d, Blocked, "operator_request_ref_invalid")
	}
	if req.PaceOverride != "" {
		if !validRef(req.PaceOverride) {
			return failure(d, Blocked, "pace_override_ref_invalid")
		}
		d.PaceOverrideRef = &req.PaceOverride
	}
	if strings.ContainsAny(req.RequestedBy, " \t\n\r\x00") || len(req.RequestedBy) > 80 {
		return failure(d, Blocked, "requested_by_invalid")
	}
	for _, candidate := range rows {
		if req.Effort != "" && candidate.Effort == normalize(req.Effort) && !candidate.Placed && candidate.Gate != "consult_only" && !strings.HasPrefix(canonical, "devin-argon") && req.E6Arm != canonical+"@"+candidate.Effort {
			return failure(d, Blocked, "e6_arm_required: exact measurement marker required")
		}
	}
	row, resolved, arm, e := resolveRow(c, req)
	if e != nil {
		return failure(d, ExitCode(e), e.Error())
	}
	d.ProviderID = row.Pool
	if row.Placed {
		d.Grade = &row.Grade
	}
	if e = roleCheck(req, row); e != nil {
		return failure(d, ExitCode(e), e.Error())
	}
	if req.OperatorRequest != "" {
		if row.Gate == "default" {
			return failure(d, Blocked, "operator_request_not_applicable: ordinary placement")
		}
		d.OperatorRequestRef = &req.OperatorRequest
		by := req.RequestedBy
		if by == "" {
			by = "unknown"
		}
		d.RequestedBy = &by
		d.RefResolution = ptr("unverified")
	}
	if row.Gate == "consult_only" && req.OperatorRequest == "" && canonical != "codex-astra" {
		return failure(d, Blocked, "consult_only: pass --operator-request")
	}
	policy := in.Config.Pool(row.Pool, now)
	d.PoolClass = &policy.Class
	if policy.Class == "exclude" {
		return failure(d, Blocked, "pool excluded by policy")
	}
	var obs *Observation
	for i := range in.Observations {
		if in.Observations[i].ID == row.Pool {
			copy := in.Observations[i]
			obs = &copy
			break
		}
	}
	if strings.HasPrefix(canonical, "devin-fusion-sol61") {
		fp := policy.FusionBillingFP
		if fp == "" || obs == nil || (obs.PublishFP != fp && obs.AccountFP != fp) {
			return failure(d, Unmeasurable, "fusion_billing_account_unavailable")
		}
	}
	if obs == nil {
		obs = &Observation{ID: row.Pool, Error: "no observation"}
	}
	isFree := canonical == "devin-swe2" || canonical == "devin-swe2-medium" || canonical == "devin-swe2-max"
	if isFree {
		// The SWE-2 free bucket cannot be measured. It is available unless a
		// free-limit exceed is on record and its reset time has not passed.
		rung := map[string]string{"devin-swe2": "swe-2-high", "devin-swe2-medium": "swe-2-medium", "devin-swe2-max": "swe-2-max"}[canonical]
		if in.FreeLimit != nil {
			if reset, e := ParseTime(in.FreeLimit.ResetAt); e == nil && reset.After(now) {
				return failure(d, Blocked, "free limit exceeded until "+in.FreeLimit.ResetAt)
			}
		}
		d.OK = true
		d.ExitCode = Admitted
		d.UsedPct = Pct(0)
		d.GateBasis = "free-bucket:" + rung
		d.FreeTag = FreeAvailable
		d.Reason = FreeAvailable + ": SWE-2 free bucket; account credits do not constrain this rung"
		for _, b := range obs.Buckets {
			if b.Scope.Kind == "account" && !obs.Stale {
				if b.Window == "1d" {
					d.AccountDailyPct = b.UsedPct
				}
				if b.Window == "7d" {
					d.AccountWeeklyPct = b.UsedPct
				}
			}
		}
		return d
	}
	cutoff := policy.Cutoff
	if in.Config.Pools[row.Pool].Cutoff == 0 && policy.Class == "preserve" {
		cutoff = 90
	}
	buckets := matching(obs.Buckets, row.Group)
	automaticFailed := obs.Error != "" || obs.Warning != "" || obs.Stale || len(buckets) == 0
	// Preserve confirmed automatic exhaustion even when the probe reports failure.
	for _, b := range buckets {
		if b.Locked || (validPct(b.UsedPct) && *b.UsedPct >= cutoff) {
			if row.Pool == "commandcode" {
				if reset, e := ParseTime(b.ResetsAt); e == nil && !reset.After(now) {
					continue
				}
			}
			d.UsedPct = b.UsedPct
			return failure(d, Blocked, fmt.Sprintf("quota exhausted window=%s cutoff=%g", b.Window, cutoff))
		}
	}
	if automaticFailed {
		accepted := obs.Stale && obs.AgeS != nil && *obs.AgeS <= 21600 && obs.ErrorKind == "probe_in_progress" && (obs.AccountFPMatch == nil || *obs.AccountFPMatch)
		if !accepted && obs.Stale && obs.AgeS != nil && *obs.AgeS <= 21600 && obs.AccountFPMatch != nil && *obs.AccountFPMatch && (obs.ErrorKind == "rate_limited" || obs.ErrorKind == "network") {
			accepted = true
		}
		if accepted {
			for _, required := range RequiredWindows(row.Pool, obs.Plan, false) {
				found := false
				for _, b := range buckets {
					reset, e := ParseTime(b.ResetsAt)
					if b.Window == required && e == nil && reset.After(now) {
						found = true
					}
				}
				if !found {
					accepted = false
				}
			}
		}
		if accepted {
			d.StaleAccepted = true
		} else {
			manual := in.Manual.Valid(row.Pool, now)
			covered := map[string]bool{}
			for _, m := range manual {
				covered[m.Window] = true
			}
			for _, w := range RequiredWindows(row.Pool, obs.Plan, true) {
				if !covered[w] {
					return failure(d, Unmeasurable, "measurement unavailable; manual fallback missing required window "+w)
				}
			}
			if len(manual) == 0 {
				return failure(d, Unmeasurable, "measurement unavailable")
			}
			buckets = nil
			var oldest time.Time
			var expires time.Time
			for _, m := range manual {
				buckets = append(buckets, Bucket{Label: m.Window, Window: m.Window, UsedPct: m.UsedPct, ResetsAt: m.ResetsAt, Scope: Scope{Kind: "account"}, Horizon: horizon(m.Window)})
				d.ManualObservationIDs = append(d.ManualObservationIDs, m.ID)
				d.ManualObservations = append(d.ManualObservations, m)
				t, _ := ParseTime(m.MeasuredAt)
				if oldest.IsZero() || t.Before(oldest) {
					oldest = t
				}
				t, _ = ParseTime(m.ExpiresAt)
				if expires.IsZero() || t.Before(expires) {
					expires = t
				}
			}
			d.Source = ptr("operator")
			d.SourceVerification = ptr("unverified")
			d.SourceLabel = ptr("self-reported manual observation")
			d.LastAutoError = &obs.Error
			d.MeasuredAt = ptr(oldest.UTC().Format(time.RFC3339Nano))
			d.ExpiresAt = ptr(expires.UTC().Format(time.RFC3339Nano))
			d.ObservedAgeS = ptr(now.Sub(oldest).Seconds())
			d.RemainingEffectS = ptr(expires.Sub(now).Seconds())
		}
	}
	covered := map[string]bool{}
	var maxUsed float64 = -1
	for _, b := range buckets {
		if !validPct(b.UsedPct) {
			continue
		}
		covered[b.Window] = true
		if *b.UsedPct > maxUsed {
			maxUsed = *b.UsedPct
		}
		if b.Locked || *b.UsedPct >= cutoff {
			d.UsedPct = b.UsedPct
			return failure(d, Blocked, fmt.Sprintf("quota exhausted window=%s cutoff=%g", b.Window, cutoff))
		}
	}
	if maxUsed < 0 {
		return failure(d, Unmeasurable, "no measured matching quota windows")
	}
	d.UsedPct = &maxUsed
	for _, w := range RequiredWindows(row.Pool, obs.Plan, d.Source != nil && *d.Source == "operator") {
		if !covered[w] {
			d.MissingWindows = append(d.MissingWindows, w)
		}
	}
	if row.Pool == "codex" && (obs.Plan == "plus" || obs.Plan == "prolite" || obs.Plan == "free") && len(d.MissingWindows) > 0 {
		return failure(d, Unmeasurable, "codex_required_window_missing:"+strings.Join(d.MissingWindows, ","))
	}
	if len(policy.InflightReservePct) > 0 {
		count := in.Inflight[row.Pool]
		if count == nil {
			d.Inflight = "inflight=unknown(local|remote)"
		} else {
			d.Inflight = fmt.Sprintf("inflight=%d", *count)
			for _, b := range buckets {
				reserve := policy.InflightReservePct[b.Window]
				if reserve > 0 && validPct(b.UsedPct) && *b.UsedPct+float64(*count)*reserve >= cutoff {
					return failure(d, Blocked, fmt.Sprintf("inflight_headroom window=%s used=%g n=%d reserve=%g cutoff=%g", b.Window, *b.UsedPct, *count, reserve, cutoff))
				}
			}
		}
	}
	if row.Gate == "escalation" && req.Effort == "" && req.OperatorRequest == "" {
		for _, other := range c.Rows {
			if other.Profile == canonical || other.Grade != row.Grade || other.Gate != "default" || !other.Placed {
				continue
			}
			r := Request{Profile: other.Profile, Effort: other.Effort, Role: req.Role}
			if gate(in, r).OK {
				return failure(d, Blocked, "escalation: same-grade ordinary alternative available")
			}
		}
	}
	if row.Gate == "escalation" && req.OperatorRequest != "" {
		d.EscalationOverride = true
	}
	d.OK = true
	d.ExitCode = Admitted
	d.E6Arm = arm
	d.Reason = fmt.Sprintf("%s pool=%s used=%g%% class=%s", d.Profile, row.Pool, maxUsed, policy.Class)
	if resolved != "" {
		d.Reason += " [rung " + canonical + "@" + resolved + "]"
	}
	for _, b := range buckets {
		if p := Pace(b, now); p != nil && *p > 1 {
			d.Reason += fmt.Sprintf("\npace-warning pool=%s window=%s multiple=%.2f", row.Pool, b.Window, *p)
		}
	}
	if d.Source == nil && obs.Source == "remote" {
		d.Source = ptr("remote")
		d.MeasuredAt = ptr(time.UnixMilli(int64(obs.FetchedAt * 1000)).UTC().Format(time.RFC3339Nano))
		d.ObservedAgeS = obs.AgeS
	}
	return d
}
func matching(buckets []Bucket, group string) []Bucket {
	var out []Bucket
	for _, b := range buckets {
		if (b.Scope.Kind == "account" || b.Scope.Kind == "" || (b.Scope.Kind == "group" && b.Scope.Name == group)) && (validPct(b.UsedPct) || b.Locked) {
			out = append(out, b)
		}
	}
	return out
}

type Recommendation struct {
	Label       ExperimentLabel   `json:"label"`
	Exploration *ExplorationAudit `json:"exploration,omitempty"`
	Profile     string            `json:"profile"`
	Effort      string            `json:"effort"`
	Grade       string            `json:"grade"`
	Pool        string            `json:"pool"`
	Family      string            `json:"family"`
	Billing     string            `json:"billing"`
	Kind        string            `json:"kind"`
	Reason      string            `json:"reason"`
	UsedPct     *float64          `json:"used_pct"`
	Promotion   string            `json:"promotion,omitempty"`
	Score       float64           `json:"score"`
	priority    int
}

func (r Recommendation) LaunchSpelling() string {
	if r.Effort != "" {
		return r.Profile + " --effort " + r.Effort
	}
	return r.Profile
}

// Recommend ranks only rows placed in the requested grade. Every paid pool can
// trigger alternative promotion. Pace changes priority, never eligibility.
func Recommend(in Inputs, grade, role, area string) []Recommendation {
	return RecommendFor(in, RecommendationRequest{Grade: grade, Role: role, Area: area})
}
func recommendBase(in Inputs, grade, role, area string) []Recommendation {
	var out []Recommendation
	c := in.catalog()
	now := in.clock()
	over := map[string]float64{}
	for _, o := range in.Observations {
		if o.Stale || o.Error != "" {
			continue
		}
		for _, b := range o.Buckets {
			if b.Window == "7d" && b.Scope.Kind == "account" {
				if p := Pace(b, now); p != nil && *p >= in.Config.PaceThreshold() {
					over[o.ID] = *p
				}
			}
		}
	}
	seen := map[string]bool{}
	for _, row := range c.Rows {
		key := row.Profile + "@" + row.Effort
		if seen[key] || row.Grade != grade || !row.Placed || row.Gate != "default" || row.RetiredAt != nil {
			continue
		}
		seen[key] = true
		req := Request{Profile: row.Profile, Effort: row.Effort, Role: role, Area: area}
		if req.Role == "" && credit(row.Profile) {
			req.Role = "worker"
		}
		d := gate(in, req)
		r := Recommendation{Profile: row.Profile, Effort: row.Effort, Grade: grade, Pool: row.Pool, Family: row.Family, Billing: row.Billing, Kind: "candidate", Reason: d.Reason, UsedPct: d.UsedPct, priority: 2}
		if !d.OK {
			r.Kind = "excluded"
			r.priority = 9
		}
		p := in.Config.Pool(row.Pool, now)
		r.Score = p.Boost + p.CapacityWeight*100
		if d.UsedPct != nil {
			r.Score -= *d.UsedPct
		}
		if credit(row.Profile) {
			r.priority = 4
		}
		if d.GateBasis != "" {
			r.priority = 0
			r.Billing = "free"
		}
		if d.OK && d.GateBasis == "" {
			triggers := make([]string, 0, len(over))
			for pool := range over {
				if pool != row.Pool {
					triggers = append(triggers, pool)
				}
			}
			sort.Strings(triggers)
			if len(triggers) > 0 {
				r.priority = 1
				pool := triggers[0]
				r.Promotion = fmt.Sprintf("promoted: %s 7d pace %.2fx >= %.2fx", pool, over[pool], in.Config.PaceThreshold())
			}
		}
		// Exceptions are tied to the exact current model identity and cannot place
		// a new grade, open a gate or exclude a row.
		for _, ex := range in.AreaExceptions {
			if area != "" && ex.Area == area && ex.Profile == row.Profile && (ex.Effort == "" || ex.Effort == row.Effort) && ex.ModelID == row.ModelID && d.OK {
				if ex.Action == "prefer" {
					r.Score += 10
					r.Reason += " area preference: " + ex.Reason
				}
				if ex.Action == "exclude" {
					r.Kind = "excluded"
					r.priority = 9
					r.Reason = "area exclusion: " + ex.Reason
				}
			}
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.priority != b.priority {
			return a.priority < b.priority
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		preferred := c.Defaults[a.Profile]
		if (a.Effort == preferred) != (b.Effort == preferred) {
			return a.Effort == preferred
		}
		return effortRank(a.Effort) < effortRank(b.Effort)
	})
	return out
}
