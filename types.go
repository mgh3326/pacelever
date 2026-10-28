// Package pacelever measures quota and resolves sustainable model admissions.
// Gate, ResolveLaunch and Recommend accept explicit inputs and perform no I/O.
package pacelever

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	Admitted     = 0
	Invalid      = 2
	Blocked      = 3
	Unmeasurable = 4
	RoleDenied   = 5
	Unsubscribed = 6
)

var ProviderIDs = []string{"claude", "codex", "devin", "kimi", "grok", "kiro", "clinepass", "commandcode"}
var Grades = []string{"S+", "S", "A+", "A", "B", "C"}

type Scope struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
}
type Bucket struct {
	Label    string   `json:"label"`
	Window   string   `json:"window"`
	UsedPct  *float64 `json:"used_pct"`
	ResetsAt string   `json:"resets_at,omitempty"`
	Scope    Scope    `json:"scope"`
	Horizon  string   `json:"horizon"`
	Note     string   `json:"note,omitempty"`
	Locked   bool     `json:"locked,omitempty"`
}
type Observation struct {
	ID             string   `json:"id"`
	Plan           string   `json:"plan,omitempty"`
	Buckets        []Bucket `json:"buckets"`
	PoolClass      string   `json:"pool_class"`
	Source         string   `json:"source,omitempty"`
	Error          string   `json:"error,omitempty"`
	ErrorKind      string   `json:"error_kind,omitempty"`
	Warning        string   `json:"warning,omitempty"`
	Note           string   `json:"note,omitempty"`
	Stale          bool     `json:"stale"`
	FetchedAt      float64  `json:"fetched_at,omitempty"`
	AgeS           *float64 `json:"age_s,omitempty"`
	AccountFP      string   `json:"account_fp,omitempty"`
	AccountFPKind  string   `json:"account_fp_kind,omitempty"`
	AccountFPMatch *bool    `json:"account_fp_match,omitempty"`
	PublishFP      string   `json:"publish_fp,omitempty"`
	PublishFPKind  string   `json:"publish_fp_kind,omitempty"`
}
type Request struct {
	TaskID          string `json:"task_id,omitempty"`
	RequiredGrade   string `json:"required_grade,omitempty"`
	Profile         string `json:"profile"`
	Effort          string `json:"effort,omitempty"`
	Role            string `json:"role,omitempty"`
	Purpose         string `json:"purpose,omitempty"`
	OperatorRequest string `json:"operator_request,omitempty"`
	RequestedBy     string `json:"requested_by,omitempty"`
	PaceOverride    string `json:"pace_override,omitempty"`
	E6Arm           string `json:"e6_arm,omitempty"`
	Area            string `json:"area,omitempty"`
}
type Decision struct {
	OK                   bool                `json:"ok"`
	ExitCode             int                 `json:"exit_code"`
	Profile              string              `json:"profile"`
	ProviderID           string              `json:"provider_id"`
	Grade                *string             `json:"grade"`
	Reason               string              `json:"reason"`
	UsedPct              *float64            `json:"used_pct"`
	PoolClass            *string             `json:"pool_class"`
	Unmeasurable         bool                `json:"unmeasurable"`
	RoleDenied           bool                `json:"role_denied"`
	Unsubscribed         bool                `json:"unsubscribed,omitempty"`
	Alternatives         []string            `json:"alternatives"`
	StaleAccepted        bool                `json:"stale_accepted"`
	EscalationOverride   bool                `json:"escalation_override"`
	OperatorRequestRef   *string             `json:"operator_request_ref"`
	PaceOverrideRef      *string             `json:"pace_override_ref,omitempty"`
	RequestedBy          *string             `json:"requested_by"`
	RefResolution        *string             `json:"ref_resolution"`
	Source               *string             `json:"source"`
	SourceVerification   *string             `json:"source_verification"`
	SourceLabel          *string             `json:"source_label"`
	ManualObservationIDs []string            `json:"manual_observation_ids"`
	ManualObservations   []ManualObservation `json:"manual_observations"`
	MeasuredAt           *string             `json:"measured_at"`
	ExpiresAt            *string             `json:"expires_at"`
	ObservedAgeS         *float64            `json:"observed_age_s"`
	RemainingEffectS     *float64            `json:"remaining_effect_s"`
	LastAutoError        *string             `json:"last_auto_error"`
	MissingWindows       []string            `json:"missing_windows"`
	GateBasis            string              `json:"gate_basis,omitempty"`
	FreeTag              string              `json:"free_tag,omitempty"`
	AccountDailyPct      *float64            `json:"account_daily_pct,omitempty"`
	AccountWeeklyPct     *float64            `json:"account_weekly_pct,omitempty"`
	E6Arm                string              `json:"e6_arm,omitempty"`
	Inflight             string              `json:"inflight,omitempty"`
}

type PolicyError struct {
	Code    int
	Message string
}

func (e *PolicyError) Error() string      { return e.Message }
func deny(code int, message string) error { return &PolicyError{code, message} }
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	if e, ok := err.(*PolicyError); ok {
		return e.Code
	}
	return Invalid
}
func ptr[T any](v T) *T { return &v }
func validPct(p *float64) bool {
	return p != nil && !math.IsNaN(*p) && !math.IsInf(*p, 0) && *p >= 0 && *p <= 100
}
func Pct(v float64) *float64 {
	if !validPct(&v) {
		return nil
	}
	return &v
}
func gradeRank(g string) int {
	for i, v := range Grades {
		if v == g {
			return i
		}
	}
	return len(Grades)
}
func normalize(s string) string             { return strings.ToLower(strings.TrimSpace(s)) }
func ParseTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }
func Duration(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	for len(s) > 0 {
		i := 0
		for i < len(s) && ((s[i] >= '0' && s[i] <= '9') || s[i] == '.') {
			i++
		}
		if i == 0 || i == len(s) {
			return 0, fmt.Errorf("invalid duration")
		}
		n, e := strconv.ParseFloat(s[:i], 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("invalid duration")
		}
		unit := map[byte]time.Duration{'s': time.Second, 'm': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 168 * time.Hour}[s[i]]
		if unit == 0 || n < 0 || n > 1e6 {
			return 0, fmt.Errorf("invalid duration")
		}
		total += time.Duration(n * float64(unit))
		s = strings.TrimSpace(s[i+1:])
	}
	return total, nil
}
func horizon(w string) string {
	d, _ := Duration(w)
	if d > 0 && d <= 24*time.Hour {
		return "now"
	}
	if d >= 28*24*time.Hour {
		return "month"
	}
	return "week"
}
func Pace(b Bucket, now time.Time) *float64 {
	if !validPct(b.UsedPct) {
		return nil
	}
	window, e := Duration(b.Window)
	if e != nil || window <= 0 {
		return nil
	}
	reset, e := ParseTime(b.ResetsAt)
	if e != nil {
		return nil
	}
	left := reset.Sub(now)
	fraction := 1 - float64(left)/float64(window)
	if left <= 0 || fraction < 0.05 || fraction > 1 {
		return nil
	}
	return ptr(*b.UsedPct / 100 / fraction)
}
func knownPool(pool string) bool {
	if pool == "anthropic-api" {
		return true
	}
	for _, p := range ProviderIDs {
		if p == pool {
			return true
		}
	}
	return false
}

// DisplayObservation uses manual evidence only for an unavailable automatic
// reading. A confirmed automatic cutoff cannot be hidden on a dashboard.
func DisplayObservation(o Observation, cfg Config, manual ManualStore, now time.Time) Observation {
	if o.Error == "" && o.Warning == "" && !o.Stale && len(matching(o.Buckets, "")) > 0 {
		return o
	}
	policy := cfg.Pool(o.ID, now)
	cutoff := policy.Cutoff
	if cfg.Pools[o.ID].Cutoff == 0 && policy.Class == "preserve" {
		cutoff = 90
	}
	for _, b := range matching(o.Buckets, "") {
		if validPct(b.UsedPct) && *b.UsedPct >= cutoff {
			return o
		}
	}
	valid := manual.Valid(o.ID, now)
	covered := map[string]bool{}
	for _, m := range valid {
		covered[m.Window] = true
	}
	for _, w := range RequiredWindows(o.ID, o.Plan, true) {
		if !covered[w] {
			return o
		}
	}
	if len(valid) == 0 {
		return o
	}
	out := o
	out.Error = ""
	out.Warning = ""
	out.Stale = false
	out.Source = "operator"
	out.Buckets = nil
	for _, m := range valid {
		out.Buckets = append(out.Buckets, Bucket{Label: m.Window, Window: m.Window, UsedPct: m.UsedPct, ResetsAt: m.ResetsAt, Scope: Scope{Kind: "account"}, Horizon: horizon(m.Window)})
	}
	return out
}
