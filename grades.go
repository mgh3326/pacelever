package pacelever

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

type GradeChange struct {
	Profile  string   `json:"profile"`
	Effort   string   `json:"effort"`
	From     string   `json:"from"`
	To       string   `json:"to"`
	Action   string   `json:"action"`
	Evidence []string `json:"evidence"`
	Reason   string   `json:"reason"`
	Blocked  bool     `json:"blocked"`
}
type GradeProposal struct {
	Schema        string        `json:"schema"`
	Rule          string        `json:"rule"`
	Fingerprint   string        `json:"fingerprint"`
	CatalogSource string        `json:"catalog_source"`
	RepsSource    string        `json:"reps_source"`
	MinPasses     int           `json:"min_passes"`
	DemoteWindow  int           `json:"demote_window"`
	DemoteRate    float64       `json:"demote_rate"`
	Changes       []GradeChange `json:"changes"`
	Excluded      []string      `json:"excluded"`
}

func EvidenceFingerprint(c Catalog, reps []Rep) string {
	rows := append([]CatalogRow(nil), c.Rows...)
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		return a.Effort < b.Effort
	})
	rr := append([]Rep(nil), reps...)
	sort.Slice(rr, func(i, j int) bool {
		if rr[i].Profile != rr[j].Profile {
			return rr[i].Profile < rr[j].Profile
		}
		if rr[i].RecordedAt != rr[j].RecordedAt {
			return rr[i].RecordedAt < rr[j].RecordedAt
		}
		return RepRef(rr[i]) < RepRef(rr[j])
	})
	b, _ := json.Marshal(struct {
		Rows []CatalogRow
		Reps []Rep
	}{rows, rr})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ProposeGrades uses exact model/effort evidence and the latest counted rep per
// task. It produces an artifact only; it never mutates a catalog.
func ProposeGrades(c Catalog, reps []Rep, source string, minPasses, window int, rate float64) GradeProposal {
	if minPasses <= 0 {
		minPasses = 2
	}
	if window <= 0 {
		window = 5
	}
	if rate <= 0 || rate > 1 {
		rate = 0.4
	}
	p := GradeProposal{Schema: "pacelever.grades.v1", Rule: "1.2", Fingerprint: EvidenceFingerprint(c, reps), CatalogSource: c.Info.Source, RepsSource: source, MinPasses: minPasses, DemoteWindow: window, DemoteRate: rate, Changes: []GradeChange{}, Excluded: []string{}}
	for _, row := range c.Rows {
		if row.RetiredAt != nil || row.Gate != "default" {
			continue
		}
		latest := map[string]Rep{}
		for _, r := range reps {
			if r.Profile != row.Profile {
				continue
			}
			if r.ModelID != row.ModelID || r.Effort != row.Effort {
				continue
			}
			if r.Pending {
				p.Excluded = append(p.Excluded, RepRef(r)+": pending write")
				continue
			}
			if r.Completed == nil || (r.Completed != nil && *r.Completed == 0 && r.BlockersFound == 0) || r.Rounds == 0 {
				p.Excluded = append(p.Excluded, RepRef(r)+": anomalous evidence")
				continue
			}
			if r.Grade == "" || gradeRank(r.Grade) == len(Grades) {
				p.Excluded = append(p.Excluded, RepRef(r)+": task grade missing")
				continue
			}
			if r.TaskRef == "" {
				p.Excluded = append(p.Excluded, RepRef(r)+": task reference missing")
				continue
			}
			if strings.Contains(strings.ToLower(r.Notes), "[non-coding]") {
				p.Excluded = append(p.Excluded, RepRef(r)+": non-coding task")
				continue
			}
			old, ok := latest[r.TaskRef]
			if !ok || r.RecordedAt > old.RecordedAt || (r.RecordedAt == old.RecordedAt && RepRef(r) > RepRef(old)) {
				latest[r.TaskRef] = r
			}
		}
		var counted []Rep
		for _, r := range latest {
			counted = append(counted, r)
		}
		sort.Slice(counted, func(i, j int) bool { return counted[i].RecordedAt > counted[j].RecordedAt })
		passes := map[string]int{}
		anyFail := false
		var evidence []string
		for _, r := range counted {
			if RepFailure(r) {
				anyFail = true
			} else if r.Completed != nil && *r.Completed == 1 && r.BlockersFound == 0 {
				passes[r.Grade]++
			}
			evidence = append(evidence, RepRef(r))
		}
		to := row.Grade
		action := "keep"
		reason := "insufficient clean evidence"
		if !anyFail {
			for _, g := range Grades {
				if passes[g] >= minPasses && gradeRank(g) < gradeRank(to) {
					to = g
					action = "promote"
					reason = "clean exact-model task passes without failure evidence"
				}
			}
		}
		recent := counted
		if len(recent) > window {
			recent = recent[:window]
		}
		failCount := 0
		postMerge := false
		worst := row.Grade
		for _, r := range recent {
			if RepFailure(r) && gradeRank(r.Grade) >= gradeRank(row.Grade) {
				failCount++
				if gradeRank(r.Grade) > gradeRank(worst) {
					worst = r.Grade
				}
				if strings.Contains(strings.ToLower(r.Notes), "[rollback]") || strings.Contains(strings.ToLower(r.Notes), "[post-merge-blocker]") {
					postMerge = true
				}
			}
		}
		if postMerge || (failCount >= 2 && len(recent) > 0 && float64(failCount)/float64(len(recent)) >= rate) {
			rank := gradeRank(worst) + 1
			if rank >= len(Grades) {
				rank = len(Grades) - 1
			}
			to = Grades[rank]
			action = "demote"
			reason = "recent failures at or below current placement"
		}
		if action != "keep" && to != row.Grade {
			p.Changes = append(p.Changes, GradeChange{Profile: row.Profile, Effort: row.Effort, From: row.Grade, To: to, Action: action, Evidence: evidence, Reason: reason})
		}
	}
	projected := append([]CatalogRow(nil), c.Rows...)
	for _, change := range p.Changes {
		for i := range projected {
			if projected[i].Profile == change.Profile && projected[i].Effort == change.Effort {
				projected[i].Grade = change.To
			}
		}
	}
	if ValidateMonotonic(projected) != nil {
		for i := range p.Changes {
			p.Changes[i].Blocked = true
			p.Changes[i].Reason += "; blocked-by-monotonicity"
		}
	}
	return p
}
func ApplyGrades(p GradeProposal, c Catalog, reps []Rep, only map[string]bool, decidedBy, ref, allowDegraded string, now time.Time) ([]CatalogRow, error) {
	if p.Schema != "pacelever.grades.v1" || p.Fingerprint != EvidenceFingerprint(c, reps) {
		return nil, fmt.Errorf("proposal evidence changed; regenerate proposal")
	}
	if (c.Info.Stale || c.Info.Source == "snapshot" || p.RepsSource == "local" || p.RepsSource == "cache-stale") && strings.TrimSpace(allowDegraded) == "" {
		return nil, fmt.Errorf("degraded evidence requires --allow-degraded reason")
	}
	if decidedBy == "" || !validRef(ref) {
		return nil, fmt.Errorf("--decided-by and valid --deviation-ref required")
	}
	verified := ProposeGrades(c, reps, p.RepsSource, p.MinPasses, p.DemoteWindow, p.DemoteRate)
	a, _ := json.Marshal(verified.Changes)
	b, _ := json.Marshal(p.Changes)
	if string(a) != string(b) {
		return nil, fmt.Errorf("proposal changes differ from recomputed evidence")
	}
	rows := append([]CatalogRow(nil), c.Rows...)
	selected := map[string]bool{}
	for _, change := range p.Changes {
		key := change.Profile + "@" + change.Effort
		if len(only) > 0 && !only[key] && !only[change.Profile] {
			continue
		}
		if change.Blocked {
			return nil, fmt.Errorf("proposal change blocked by monotonicity")
		}
		selected[key] = true
		for i := range rows {
			if rows[i].Profile == change.Profile && rows[i].Effort == change.Effort {
				rows[i].Grade = change.To
				rows[i].DecidedBy = decidedBy
				rows[i].DecidedAt = now.UTC().Format(time.RFC3339Nano)
				rows[i].DeviationRef = ref
				if allowDegraded != "" {
					rows[i].DeviationRef += " (degraded: " + allowDegraded + ")"
				}
			}
		}
	}
	if len(selected) == 0 {
		return nil, fmt.Errorf("no approved grade changes")
	}
	if e := ValidateMonotonic(rows); e != nil {
		return nil, e
	}
	return rows, nil
}
