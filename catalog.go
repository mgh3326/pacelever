package pacelever

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed catalog_snapshot.json
var snapshot []byte

type CatalogRow struct {
	Profile         string   `json:"profile"`
	Effort          string   `json:"effort"`
	ModelID         string   `json:"model_id"`
	Pool            string   `json:"pool"`
	Grade           string   `json:"grade"`
	Score           *float64 `json:"score"`
	Gate            string   `json:"gate"`
	GateReason      *string  `json:"gate_reason"`
	Placed          bool     `json:"placed"`
	Billing         string   `json:"billing,omitempty"`
	Family          string   `json:"family,omitempty"`
	Group           string   `json:"group,omitempty"`
	RetiredAt       *string  `json:"retired_at,omitempty"`
	BoundaryVersion string   `json:"boundary_version,omitempty"`
	DeviationRef    string   `json:"deviation_ref,omitempty"`
	DecidedBy       string   `json:"decided_by,omitempty"`
	DecidedAt       string   `json:"decided_at,omitempty"`
}
type CatalogInfo struct {
	Source string   `json:"source"`
	Stale  bool     `json:"stale"`
	AgeS   *float64 `json:"age_s"`
}
type Catalog struct {
	Rows     []CatalogRow      `json:"rows"`
	Info     CatalogInfo       `json:"catalog"`
	Defaults map[string]string `json:"-"`
	Aliases  map[string]string `json:"-"`
}

func BundledCatalog() Catalog {
	var v struct {
		Rows     []CatalogRow      `json:"rows"`
		Defaults map[string]string `json:"defaults"`
		Aliases  map[string]string `json:"aliases"`
	}
	if e := json.Unmarshal(snapshot, &v); e != nil {
		panic(e)
	}
	return Catalog{Rows: v.Rows, Info: CatalogInfo{Source: "snapshot"}, Defaults: v.Defaults, Aliases: v.Aliases}
}
func (c Catalog) Canonical(profile string) string {
	if v := c.Aliases[profile]; v != "" {
		return v
	}
	return profile
}
func (c Catalog) profileRows(profile string) []CatalogRow {
	var rows []CatalogRow
	for _, r := range c.Rows {
		if r.Profile == profile && r.RetiredAt == nil {
			rows = append(rows, r)
		}
	}
	return rows
}
func credit(profile string) bool {
	return strings.HasPrefix(profile, "pi-") || profile == "cmd-dsflash41"
}
func effortRank(s string) int {
	for i, k := range []string{"", "off", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if s == k {
			return i
		}
	}
	return 99
}
func defaultEffort(profile string, rows []CatalogRow, defaults map[string]string) (string, string) {
	preferred := defaults[profile]
	var ordinary []CatalogRow
	for _, r := range rows {
		if r.Gate == "default" && r.Placed {
			ordinary = append(ordinary, r)
			if preferred != "" && r.Effort == preferred {
				return preferred, preferred
			}
		}
	}
	if len(ordinary) == 0 {
		ordinary = append(ordinary, rows...)
	}
	sort.SliceStable(ordinary, func(i, j int) bool {
		a, b := ordinary[i], ordinary[j]
		if gradeRank(a.Grade) != gradeRank(b.Grade) {
			return gradeRank(a.Grade) < gradeRank(b.Grade)
		}
		return effortRank(a.Effort) < effortRank(b.Effort)
	})
	if len(ordinary) == 0 {
		return "", ""
	}
	if ordinary[0].Effort == "" {
		return "", preferred
	}
	return ordinary[0].Effort, ordinary[0].Effort
}

type LaunchDecision struct {
	Profile               string            `json:"profile"`
	ModelID               string            `json:"model_id"`
	Effort                string            `json:"effort"`
	Pool                  string            `json:"pool"`
	Gate                  string            `json:"gate"`
	Grade                 string            `json:"grade"`
	GateReason            *string           `json:"gate_reason"`
	Catalog               CatalogInfo       `json:"catalog"`
	OperatorRequest       bool              `json:"operator_request"`
	E6Arm                 *string           `json:"e6_arm"`
	Billing               string            `json:"billing"`
	Family                string            `json:"family"`
	RoundExtensionBuilder map[string]string `json:"round_extension_builder,omitempty"`
}

func validRef(s string) bool {
	if strings.TrimSpace(s) != s || strings.ContainsAny(s, "\r\n\t\x00 ") {
		return false
	}
	if strings.HasPrefix(s, "hk:task/") {
		s = strings.TrimPrefix(s, "hk:task/")
		if s == "" {
			return false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return false
			}
		}
		return s != "0"
	}
	return (strings.HasPrefix(s, "hk:doc/") && len(s) > 7) || (strings.HasPrefix(s, "hk:doc ") && len(s) > 7)
}
func roleCheck(req Request, row CatalogRow) error {
	role := normalize(req.Role)
	purpose := normalize(req.Purpose)
	if credit(req.Profile) {
		allowed := role == "worker" || role == "tester" || (role == "builder" && row.Placed && row.Gate == "default")
		if !allowed {
			return deny(RoleDenied, "role_restricted: credit rung requires worker/tester; placed default rungs also allow builder")
		}
	}
	if req.Profile == "codex-astra" || req.Profile == "fable" {
		if role == "worker" || role == "captain" {
			return deny(RoleDenied, "role_restricted: consult model cannot hold this seat")
		}
		if role == "builder" || role == "tester" || role == "reviewer" {
			if purpose != "round-extension" || !strings.HasPrefix(req.OperatorRequest, "hk:doc/round-ext/") {
				return deny(RoleDenied, "role_restricted: round-extension and a round-ext document required")
			}
		}
	}
	if strings.Contains(req.Profile, "astra") && req.Profile != "codex-astra" {
		return deny(RoleDenied, "role_restricted: removed astra seat alias")
	}
	if req.Profile == "codex-astra" && purpose != "director" && purpose != "architect" && purpose != "operator-request" && purpose != "round-extension" {
		return deny(RoleDenied, "role_restricted: astra requires director, architect, or operator-request purpose")
	}
	return nil
}

// ResolveLaunch resolves model identity and placement without probing providers.
func ResolveLaunch(c Catalog, cfg Config, req Request) (LaunchDecision, error) {
	row, resolved, arm, e := resolveRow(c, req)
	if e != nil {
		return LaunchDecision{}, e
	}
	req.Profile = c.Canonical(req.Profile)
	if e = roleCheck(req, row); e != nil {
		return LaunchDecision{}, e
	}
	if req.OperatorRequest != "" && !validRef(req.OperatorRequest) {
		return LaunchDecision{}, deny(Blocked, "operator_request_ref_invalid")
	}
	if !cfg.Subscribed(req.Profile, row.Pool) {
		return LaunchDecision{}, deny(Unsubscribed, "profile is unsubscribed")
	}
	if row.Gate == "consult_only" && req.OperatorRequest == "" && req.Profile != "codex-astra" {
		return LaunchDecision{}, deny(Blocked, "profile is consult_only; pass --operator-request")
	}
	if c.Info.Stale && row.Gate != "default" && row.Gate != "consult_only" && req.OperatorRequest == "" {
		return LaunchDecision{}, deny(Blocked, "catalog is stale; non-default gate requires --operator-request")
	}
	d := LaunchDecision{Profile: req.Profile, ModelID: row.ModelID, Effort: resolved, Pool: row.Pool, Gate: row.Gate, Grade: row.Grade, GateReason: row.GateReason, Catalog: c.Info, OperatorRequest: req.OperatorRequest != "", Billing: row.Billing, Family: row.Family}
	if d.Billing == "" {
		d.Billing = "unknown"
	}
	if arm != "" {
		d.E6Arm = &arm
	}
	if (req.Profile == "fable" || req.Profile == "codex-astra") && normalize(req.Role) == "builder" {
		d.RoundExtensionBuilder = map[string]string{"policy": "hk1672/v1", "profile": req.Profile, "role": "builder", "purpose": "round-extension", "ref": req.OperatorRequest}
	}
	return d, nil
}
func resolveRow(c Catalog, req Request) (CatalogRow, string, string, error) {
	profile := c.Canonical(req.Profile)
	req.Profile = profile
	requested := normalize(req.Effort)
	if credit(profile) && requested == "" {
		return CatalogRow{}, "", "", deny(Blocked, "explicit --effort required for credit profile")
	}
	if requested != "" && effortRank(requested) == 99 {
		known := false
		for _, row := range c.Rows {
			if row.Profile == profile && row.Effort == requested {
				known = true
			}
		}
		if !known {
			return CatalogRow{}, "", "", deny(Blocked, "unknown effort rung")
		}
	}
	if credit(profile) {
		allowed := requested == "low" || requested == "high" || requested == "max"
		if strings.HasPrefix(profile, "pi-") {
			allowed = requested == "off" || requested == "minimal" || requested == "low" || requested == "medium" || requested == "high" || requested == "xhigh" || requested == "max"
		}
		if !allowed {
			return CatalogRow{}, "", "", deny(Blocked, "credit effort is not placed")
		}
	}
	for _, barred := range []string{"codex-med", "builder-terra-high", "builder-terra-xhigh", "builder-terra-max"} {
		if profile == barred {
			return CatalogRow{}, "", "", deny(Blocked, "model_refused: retired model identity")
		}
	}
	for _, candidate := range c.Rows {
		if candidate.Profile == profile && candidate.ModelID == "gpt-5.6-terra" {
			return CatalogRow{}, "", "", deny(Blocked, "model_refused: retired model identity")
		}
	}

	rows := c.profileRows(profile)
	if len(rows) == 0 {
		return CatalogRow{}, "", "", deny(Blocked, fmt.Sprintf("profile %q is unknown or retired", profile))
	}
	if requested == "" && strings.HasPrefix(req.E6Arm, profile+"@") {
		for _, r := range rows {
			if r.Placed {
				goto selected
			}
		}
		requested = strings.TrimPrefix(req.E6Arm, profile+"@")
	}
selected:
	var eligible []CatalogRow
	for _, r := range rows {
		if r.Placed || r.Gate == "consult_only" || strings.HasPrefix(profile, "devin-argon") || req.E6Arm == profile+"@"+r.Effort {
			eligible = append(eligible, r)
		}
	}
	if len(eligible) == 0 {
		return CatalogRow{}, "", "", deny(Blocked, "e6_arm_required: unplaced profile requires exact measurement marker")
	}
	fallback, resolved := defaultEffort(profile, eligible, c.Defaults)
	if requested != "" {
		resolved = requested
	}
	for _, r := range c.Rows {
		if r.Profile == profile && r.Effort == resolved && r.RetiredAt != nil {
			return CatalogRow{}, "", "", deny(Blocked, "rung is retired in the catalog")
		}
	}
	var chosen *CatalogRow
	for i := range eligible {
		if eligible[i].Effort == resolved {
			chosen = &eligible[i]
			break
		}
	}
	if chosen == nil {
		if credit(profile) || profile == "codex-terra61" {
			return CatalogRow{}, "", "", deny(Blocked, "e6_arm_required: rung is not placed")
		}
		for i := range eligible {
			if eligible[i].Effort == fallback {
				chosen = &eligible[i]
				break
			}
		}
	}
	if chosen == nil {
		return CatalogRow{}, "", "", deny(Blocked, "rung is not in the catalog")
	}
	arm := ""
	if !chosen.Placed && chosen.Gate != "consult_only" && !strings.HasPrefix(profile, "devin-argon") {
		if req.E6Arm != profile+"@"+resolved {
			return CatalogRow{}, "", "", deny(Blocked, "e6_arm_required")
		}
		arm = req.E6Arm
	}
	return *chosen, resolved, arm, nil
}
