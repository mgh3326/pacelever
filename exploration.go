package pacelever

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ExplorationPolicy is opt-in data on one subscription pool. A zero fraction
// is off; enabled=false can suspend a cohort without deleting its identifier.
type ExplorationPolicy struct {
	Fraction     float64  `toml:"fraction" json:"fraction"`
	Enabled      *bool    `toml:"enabled" json:"enabled,omitempty"`
	ExperimentID string   `toml:"experiment_id" json:"experiment_id"`
	Seed         string   `toml:"seed" json:"seed,omitempty"`
	ModelOrder   []string `toml:"model_order" json:"model_order,omitempty"`
}

func (p ExplorationPolicy) Active() bool { return p.Fraction > 0 && (p.Enabled == nil || *p.Enabled) }
func (p ExplorationPolicy) Validate() error {
	if math.IsNaN(p.Fraction) || math.IsInf(p.Fraction, 0) || p.Fraction < 0 || p.Fraction > 1 {
		return fmt.Errorf("exploration fraction must be in 0..1")
	}
	if p.Active() && (p.ExperimentID == "" || len(p.ExperimentID) > 128 || strings.ContainsAny(p.ExperimentID, "\x00\r\n\t ")) {
		return fmt.Errorf("active exploration requires a single-token experiment_id of at most 128 characters")
	}
	if len(p.Seed) > 256 || strings.ContainsAny(p.Seed, "\x00\r\n") {
		return fmt.Errorf("invalid exploration seed")
	}
	seen := map[string]bool{}
	for _, profile := range p.ModelOrder {
		if profile == "" || seen[profile] || strings.ContainsAny(profile, "\x00\r\n\t ") {
			return fmt.Errorf("exploration model_order requires distinct profile names")
		}
		seen[profile] = true
	}
	return nil
}

type ExperimentLabel struct {
	Profile      string  `json:"profile"`
	Effort       string  `json:"effort"`
	Arm          string  `json:"arm"`
	ExperimentID *string `json:"experiment_id"`
}
type ExplorationAudit struct {
	Eligible      bool     `json:"eligible"`
	Selected      bool     `json:"selected"`
	NormalProfile string   `json:"normal_profile"`
	NormalEffort  string   `json:"normal_effort"`
	PaceMultiple  *float64 `json:"pace_multiple"`
	Reason        string   `json:"reason"`
}

func baselineLabel(profile, effort string, p ExplorationPolicy) ExperimentLabel {
	label := ExperimentLabel{Profile: profile, Effort: effort, Arm: "baseline"}
	if p.Active() && p.ExperimentID != "" {
		label.ExperimentID = &p.ExperimentID
	}
	return label
}
func taskExperimentKey(task string) string {
	if id := TaskID(task); id > 0 {
		return "hk:" + strconv.FormatInt(id, 10)
	}
	return strings.TrimSpace(task)
}
func experimentSelected(policy ExplorationPolicy, pool, task string) bool {
	if policy.Fraction <= 0 {
		return false
	}
	if policy.Fraction >= 1 {
		return true
	}
	hash := sha256.Sum256([]byte(policy.ExperimentID + "\x00" + policy.Seed + "\x00" + pool + "\x00" + taskExperimentKey(task)))
	// Use 53 bits, which are exactly representable by float64.
	sample := float64(binary.BigEndian.Uint64(hash[:8])>>11) / float64(uint64(1)<<53)
	return sample < policy.Fraction
}

var defaultModelOrders = map[string][]string{
	"claude":        {"opus", "sonnet", "haiku"},
	"codex":         {"codex-sol", "codex-terra61", "codex-luna"},
	"anthropic-api": {"pi-opus55", "pi-sonnet55", "pi-haiku55"},
	"devin":         {"devin-swe2-max", "devin-swe2", "devin-swe2-medium"},
}

// ResolveLaunchWithInputs adds opt-in exploration to the baseline resolver.
// Unknown headroom or missing task attribution leaves the baseline unchanged.
func ResolveLaunchWithInputs(in Inputs, req Request) (LaunchDecision, error) {
	c := in.catalog()
	baseline, e := ResolveLaunch(c, in.Config, req)
	if e != nil {
		return baseline, e
	}
	policy := in.Config.Pools[baseline.Pool].Exploration
	if !policy.Active() {
		return baseline, nil
	}
	if e = policy.Validate(); e != nil {
		return LaunchDecision{}, deny(Invalid, e.Error())
	}
	audit := &ExplorationAudit{NormalProfile: baseline.Profile, NormalEffort: baseline.Effort}
	baseline.Exploration = audit
	keep := func(reason string) (LaunchDecision, error) { audit.Reason = reason; return baseline, nil }
	if taskExperimentKey(req.TaskID) == "" {
		return keep("task id required for deterministic exploration")
	}
	if req.RequiredGrade == "" || gradeRank(req.RequiredGrade) == len(Grades) {
		return keep("a valid task grade floor is required")
	}
	normalRow, _, _, e := resolveRow(c, req)
	if e != nil {
		return baseline, e
	}
	if !normalRow.Placed || normalRow.Gate != "default" || (normalRow.Effort != "" && normalRow.Effort != baseline.Effort) {
		return keep("normal pick is not an exact placed default-gate rung")
	}
	if gradeRank(normalRow.Grade) > gradeRank(req.RequiredGrade) {
		return keep("normal placement does not meet the task grade floor")
	}
	eligible, pace, reason := explorationHeadroom(in, req, baseline.Pool)
	audit.PaceMultiple = pace
	if !eligible {
		return keep(reason)
	}
	target, found := downshiftTarget(in, req, baseline, policy)
	if !found {
		return keep("no one-step downshift has a placed grade meeting the task floor and seat")
	}
	audit.Eligible = true
	baseline.Label.ExperimentID = &policy.ExperimentID
	if !experimentSelected(policy, baseline.Pool, req.TaskID) {
		return keep("baseline control arm for deterministic task cohort")
	}
	targetRequest := req
	targetRequest.Profile = target.Profile
	targetRequest.Effort = target.Effort
	targetRequest.E6Arm = ""
	targetRequest.OperatorRequest = ""
	targetRequest.RequestedBy = ""
	shifted, e := ResolveLaunch(c, in.Config, targetRequest)
	if e != nil {
		return keep("downshift resolution refused")
	}
	// Same subscription is a hard boundary, including explicit profile overrides.
	if shifted.Pool != baseline.Pool {
		return keep("downshift crossed the subscription pool")
	}
	shifted.Label = ExperimentLabel{Profile: shifted.Profile, Effort: shifted.Effort, Arm: "downshift", ExperimentID: &policy.ExperimentID}
	audit.Selected = true
	audit.Reason = "one-step downshift under headroom, seat and placed-grade guards"
	shifted.Exploration = audit
	return shifted, nil
}
func explorationHeadroom(in Inputs, req Request, pool string) (bool, *float64, string) {
	// A free claim is proved by the ordinary exact-rung admission path.
	if d := gate(in, req); d.OK && d.GateBasis != "" {
		return true, nil, "free model-scoped bucket"
	}
	var observation *Observation
	for _, o := range in.Observations {
		if o.ID == pool {
			copy := DisplayObservation(o, in.Config, in.Manual, in.clock())
			observation = &copy
			break
		}
	}
	if observation == nil || observation.Error != "" || observation.Warning != "" || observation.Stale {
		return false, nil, "headroom is unknown or stale"
	}
	var multiple *float64
	for _, bucket := range observation.Buckets {
		if bucket.Window != "7d" || bucket.Scope.Kind != ScopeAccount {
			continue
		}
		pace := Pace(bucket, in.clock())
		if pace == nil {
			return false, nil, "weekly pace is unknown"
		}
		if multiple == nil || *pace > *multiple {
			multiple = pace
		}
	}
	if multiple == nil {
		return false, nil, "weekly account pace is required for a paid pool"
	}
	if *multiple >= 1 {
		return false, multiple, "weekly pace must be strictly below 1.0"
	}
	if !gate(in, req).OK {
		return false, multiple, "normal admission is unavailable"
	}
	return true, multiple, "weekly pace below 1.0"
}
func downshiftTarget(in Inputs, req Request, normal LaunchDecision, policy ExplorationPolicy) (CatalogRow, bool) {
	c := in.catalog()
	allowed := func(row CatalogRow) bool {
		if !row.Placed || row.Gate != "default" || row.RetiredAt != nil || row.Pool != normal.Pool || gradeRank(row.Grade) > gradeRank(req.RequiredGrade) {
			return false
		}
		candidate := req
		candidate.Profile = row.Profile
		candidate.Effort = row.Effort
		candidate.E6Arm = ""
		candidate.OperatorRequest = ""
		candidate.RequestedBy = ""
		// The target still passes ordinary quota and seat checks. No experiment
		// marker may widen a gate or graduate a measurement rung.
		return gate(in, candidate).OK
	}
	// Pick only the nearest lower known effort. An ineligible intermediate
	// rung is never skipped in favor of a second lower effort.
	var lower *CatalogRow
	if normal.Effort != "" && effortRank(normal.Effort) != 99 {
		for _, row := range c.Rows {
			if row.Profile != normal.Profile || row.ModelID != normal.ModelID || row.Effort == "" || row.RetiredAt != nil {
				continue
			}
			rank := effortRank(row.Effort)
			if rank < effortRank(normal.Effort) && (lower == nil || rank > effortRank(lower.Effort)) {
				copy := row
				lower = &copy
			}
		}
	}
	if lower != nil && allowed(*lower) {
		return *lower, true
	}
	order := policy.ModelOrder
	if len(order) == 0 {
		order = defaultModelOrders[normal.Pool]
	}
	next := ""
	for i, profile := range order {
		if profile == normal.Profile && i+1 < len(order) {
			next = order[i+1]
			break
		}
	}
	if next == "" {
		return CatalogRow{}, false
	}
	rows := c.profileRows(next)
	if len(rows) == 0 {
		return CatalogRow{}, false
	}
	// Keep the same effort when that lower model places it. Otherwise use its
	// normal catalog default; never cherry-pick a stronger rung to claim a floor.
	for _, row := range rows {
		if row.Effort == normal.Effort {
			if allowed(row) {
				return row, true
			}
			return CatalogRow{}, false
		}
	}
	defaultKey, _ := defaultEffort(next, rows, c.Defaults)
	for _, row := range rows {
		if row.Effort == defaultKey && allowed(row) {
			return row, true
		}
	}
	return CatalogRow{}, false
}

type RecommendationRequest struct {
	Grade  string
	Role   string
	Area   string
	TaskID string
}

// RecommendFor offers the selected downshift ahead of its baseline while keeping
// the ordinary alternatives available. The same task key resolves identically
// when handed to ResolveLaunchWithInputs for the normal pick.
func RecommendFor(in Inputs, req RecommendationRequest) []Recommendation {
	rows := recommendBase(in, req.Grade, req.Role, req.Area)
	for i := range rows {
		rows[i].Label = baselineLabel(rows[i].Profile, rows[i].Effort, ExplorationPolicy{})
	}
	normalIndex := -1
	for i, r := range rows {
		if r.Kind == "candidate" {
			normalIndex = i
			break
		}
	}
	if normalIndex < 0 {
		return rows
	}
	normal := rows[normalIndex]
	launchReq := Request{Profile: normal.Profile, Effort: normal.Effort, Role: req.Role, Area: req.Area, TaskID: req.TaskID, RequiredGrade: req.Grade}
	if launchReq.Role == "" && credit(normal.Profile) {
		launchReq.Role = "worker"
	}
	selected, e := ResolveLaunchWithInputs(in, launchReq)
	if e != nil {
		return rows
	}
	labelPolicy := ExplorationPolicy{}
	if selected.Exploration != nil && selected.Exploration.Eligible {
		labelPolicy = in.Config.Pools[normal.Pool].Exploration
	}
	rows[normalIndex].Label = baselineLabel(normal.Profile, normal.Effort, labelPolicy)
	rows[normalIndex].Exploration = selected.Exploration
	if selected.Label.Arm != "downshift" {
		return rows
	}
	candidate := Recommendation{Profile: selected.Profile, Effort: selected.Effort, Grade: selected.Grade, Pool: selected.Pool, Family: selected.Family, Billing: selected.Billing, Kind: "candidate", Label: selected.Label, Exploration: selected.Exploration, Reason: "exploration offer: one-step downshift", Promotion: "experiment " + *selected.Label.ExperimentID + " downshift"}
	remaining := make([]Recommendation, 0, len(rows)+1)
	remaining = append(remaining, candidate)
	for _, row := range rows {
		if row.Profile == candidate.Profile && row.Effort == candidate.Effort {
			continue
		}
		remaining = append(remaining, row)
	}
	return remaining
}
