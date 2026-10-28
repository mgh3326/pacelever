package pacelever

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
)

const OutcomeRule = "first-ci-green+no-critical-high+no-revert-7d/v1"

// HydrateRepOutcomes accepts flat notes JSON and the mergewake
// {source,outcome:{...}} envelope. Explicit fields and notes must agree.
func HydrateRepOutcomes(r *Rep) error {
	note := strings.TrimSpace(r.Notes)
	if strings.HasPrefix(note, "{") {
		var raw map[string]json.RawMessage
		if json.Unmarshal([]byte(note), &raw) != nil {
			return fmt.Errorf("invalid structured notes JSON")
		}
		sources := []map[string]json.RawMessage{raw}
		for _, key := range []string{"outcome", "outcomes"} {
			if data := raw[key]; len(data) > 0 && string(data) != "null" {
				var nested map[string]json.RawMessage
				if json.Unmarshal(data, &nested) != nil {
					return fmt.Errorf("invalid structured outcome object")
				}
				sources = append(sources, nested)
			}
		}
		for sourceIndex, source := range sources {
			for _, key := range []string{"first_ci_green", "reverted_within_7d"} {
				data, ok := source[key]
				if !ok || string(data) == "null" {
					continue
				}
				var value bool
				if json.Unmarshal(data, &value) != nil {
					return fmt.Errorf("invalid outcome field %s", key)
				}
				target := &r.FirstCIGreen
				if key == "reverted_within_7d" {
					target = &r.RevertedWithin7d
				}
				if *target != nil && **target != value {
					return fmt.Errorf("conflicting outcome field %s", key)
				}
				*target = &value
			}
			if data, ok := source["findings_by_severity"]; ok && string(data) != "null" {
				var findings map[string]int
				if json.Unmarshal(data, &findings) != nil || findings == nil {
					return fmt.Errorf("invalid findings_by_severity map")
				}
				normalized := map[string]int{}
				for severity, count := range findings {
					key := normalize(severity)
					if key == "" || strings.ContainsAny(key, "\r\n\x00") || len(key) > 64 || count < 0 || count > 1000000 {
						return fmt.Errorf("invalid findings_by_severity entry")
					}
					normalized[key] += count
				}
				if r.FindingsBySeverity != nil && !reflect.DeepEqual(r.FindingsBySeverity, normalized) {
					return fmt.Errorf("conflicting findings_by_severity")
				}
				r.FindingsBySeverity = normalized
			}
			if data, ok := source["time_to_merge_s"]; ok && string(data) != "null" {
				var value float64
				if json.Unmarshal(data, &value) != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
					return fmt.Errorf("invalid time_to_merge_s")
				}
				if r.TimeToMergeS != nil && *r.TimeToMergeS != value {
					return fmt.Errorf("conflicting time_to_merge_s")
				}
				r.TimeToMergeS = &value
			}
			for _, key := range []string{"experiment_id", "experiment_arm", "arm"} {
				data, ok := source[key]
				if !ok || string(data) == "null" {
					continue
				}
				var value string
				if json.Unmarshal(data, &value) != nil {
					return fmt.Errorf("invalid outcome field %s", key)
				}
				target := &r.ExperimentID
				if key != "experiment_id" {
					target = &r.ExperimentArm
				}
				if *target != "" && value != "" && *target != value {
					return fmt.Errorf("conflicting outcome field %s", key)
				}
				if value != "" {
					*target = value
				}
			}
			if sourceIndex > 0 && HasOutcomeFields(*r) {
				for _, key := range []string{"profile", "model", "model_id", "effort", "task_ref"} {
					data, exists := source[key]
					if !exists || string(data) == "null" {
						continue
					}
					var value string
					if json.Unmarshal(data, &value) != nil {
						return fmt.Errorf("invalid outcome attribution field %s", key)
					}
					if value == "" {
						continue
					}
					expected := ""
					switch key {
					case "profile":
						expected = r.Profile
					case "model", "model_id":
						expected = r.ModelID
					case "effort":
						expected = r.Effort
						if expected == "" {
							r.Effort = value
						}
					case "task_ref":
						expected = r.TaskRef
						if taskExperimentKey(value) == taskExperimentKey(expected) {
							continue
						}
					}
					if expected != "" && expected != value {
						return fmt.Errorf("outcome attribution conflicts with rep field %s", key)
					}
				}
			}
			if data, ok := source["finalized"]; ok && string(data) != "null" {
				var value bool
				if json.Unmarshal(data, &value) != nil {
					return fmt.Errorf("invalid finalized outcome field")
				}
				if r.OutcomeFinalized != nil && *r.OutcomeFinalized != value {
					return fmt.Errorf("conflicting finalized outcome field")
				}
				r.OutcomeFinalized = &value
			}
			if data, ok := source["decision"]; ok && string(data) != "null" {
				var value string
				if json.Unmarshal(data, &value) != nil {
					return fmt.Errorf("invalid outcome decision")
				}
				r.OutcomeDecision = value
			}
		}
	}
	return ValidateRepOutcomes(*r)
}
func ValidateRepOutcomes(r Rep) error {
	if r.ExperimentArm != "" && r.ExperimentArm != "baseline" && r.ExperimentArm != "downshift" {
		return fmt.Errorf("experiment_arm must be baseline or downshift")
	}
	if len(r.ExperimentID) > 128 || strings.ContainsAny(r.ExperimentID, "\r\n\t\x00 ") {
		return fmt.Errorf("invalid experiment_id")
	}
	if r.ExperimentArm == "downshift" && r.ExperimentID == "" {
		return fmt.Errorf("downshift requires experiment_id")
	}
	if r.TimeToMergeS != nil && (math.IsNaN(*r.TimeToMergeS) || math.IsInf(*r.TimeToMergeS, 0) || *r.TimeToMergeS < 0) {
		return fmt.Errorf("invalid time_to_merge_s")
	}
	for severity, count := range r.FindingsBySeverity {
		if normalize(severity) == "" || len(severity) > 64 || strings.ContainsAny(severity, "\r\n\x00") || count < 0 || count > 1000000 {
			return fmt.Errorf("invalid findings_by_severity entry")
		}
	}
	return nil
}
func HasOutcomeFields(r Rep) bool {
	return r.FirstCIGreen != nil || r.FindingsBySeverity != nil || r.RevertedWithin7d != nil || r.TimeToMergeS != nil || r.OutcomeFinalized != nil || r.OutcomeDecision != ""
}

// NotesForWire carries optional fields through older bench servers, whose rep
// envelope does not yet declare them. Unknown notes keys are retained.
func NotesForWire(r Rep) string {
	if !HasOutcomeFields(r) && r.ExperimentID == "" && r.ExperimentArm == "" {
		return r.Notes
	}
	raw := map[string]any{}
	if json.Unmarshal([]byte(r.Notes), &raw) != nil || raw == nil {
		raw = map[string]any{}
		if r.Notes != "" {
			raw["text"] = r.Notes
		}
	}
	if r.FirstCIGreen != nil {
		raw["first_ci_green"] = *r.FirstCIGreen
	}
	if r.FindingsBySeverity != nil {
		raw["findings_by_severity"] = r.FindingsBySeverity
	}
	if r.RevertedWithin7d != nil {
		raw["reverted_within_7d"] = *r.RevertedWithin7d
	}
	if r.TimeToMergeS != nil {
		raw["time_to_merge_s"] = *r.TimeToMergeS
	}
	if r.ExperimentID != "" {
		raw["experiment_id"] = r.ExperimentID
	}
	if r.ExperimentArm != "" {
		raw["experiment_arm"] = r.ExperimentArm
	}
	if r.OutcomeFinalized != nil {
		raw["finalized"] = *r.OutcomeFinalized
	}
	if r.OutcomeDecision != "" {
		raw["decision"] = r.OutcomeDecision
	}
	data, _ := json.Marshal(raw)
	return string(data)
}
func severeFinding(severity string) bool {
	switch normalize(severity) {
	case "blocking", "blocker", "critical", "high", "major", "p0", "p1", "s0", "s1":
		return true
	}
	return false
}
func knownNonsevere(severity string) bool {
	switch normalize(severity) {
	case "medium", "moderate", "low", "minor", "info", "informational", "nit", "nits", "style", "suggestion", "non_blocking", "non-blocking", "nonblocking", "p2", "p3", "p4", "s2", "s3", "s4":
		return true
	}
	return false
}

// OutcomeClass recomputes the approved rule from fields, never a publisher's
// claimed evidence label. Initial red CI alone is not demotion evidence.
func OutcomeClass(r Rep) string {
	if !HasOutcomeFields(r) {
		return "legacy"
	}
	if strings.Contains(strings.ToLower(r.Notes), "[rollback]") || strings.Contains(strings.ToLower(r.Notes), "[post-merge-blocker]") {
		return "fail"
	}
	if r.RevertedWithin7d != nil && *r.RevertedWithin7d {
		return "fail"
	}
	unknownFinding := false
	for severity, count := range r.FindingsBySeverity {
		if count > 0 && severeFinding(severity) {
			return "fail"
		}
		if count > 0 && !knownNonsevere(severity) && !severeFinding(severity) {
			unknownFinding = true
		}
	}
	if r.OutcomeFinalized != nil && !*r.OutcomeFinalized {
		return "pending"
	}
	merged := r.TimeToMergeS != nil || (r.Completed != nil && *r.Completed == 1) || (r.OutcomeDecision == "landed" || r.OutcomeDecision == "merged")
	if r.OutcomeDecision != "" && r.OutcomeDecision != "landed" && r.OutcomeDecision != "merged" {
		merged = false
	}
	if !merged || r.FirstCIGreen == nil || r.RevertedWithin7d == nil || r.FindingsBySeverity == nil {
		return "pending"
	}
	if !*r.FirstCIGreen || unknownFinding {
		return "with_fixes"
	}
	return "pass"
}
func repCleanPass(r Rep) bool {
	if HasOutcomeFields(r) {
		return OutcomeClass(r) == "pass"
	}
	return r.Completed != nil && *r.Completed == 1 && r.BlockersFound == 0 && !RepFailure(r)
}
func repPostMergeFailure(r Rep) bool {
	return r.RevertedWithin7d != nil && *r.RevertedWithin7d || strings.Contains(strings.ToLower(r.Notes), "[rollback]") || strings.Contains(strings.ToLower(r.Notes), "[post-merge-blocker]")
}

type OutcomeMetrics struct {
	Records            int            `json:"records"`
	Passes             int            `json:"passes"`
	Failures           int            `json:"failures"`
	Pending            int            `json:"pending"`
	WithFixes          int            `json:"with_fixes"`
	FirstCIKnown       int            `json:"first_ci_known"`
	FirstCIGreen       int            `json:"first_ci_green"`
	FirstCIGreenRate   *float64       `json:"first_ci_green_rate"`
	FindingsBySeverity map[string]int `json:"findings_by_severity"`
	RevertKnown        int            `json:"revert_known"`
	RevertedWithin7d   int            `json:"reverted_within_7d"`
	RevertRate         *float64       `json:"revert_rate"`
	MergeTimeKnown     int            `json:"merge_time_known"`
	MeanTimeToMergeS   *float64       `json:"mean_time_to_merge_s"`
	MedianTimeToMergeS *float64       `json:"median_time_to_merge_s"`
	times              []float64
}

func (m *OutcomeMetrics) add(r Rep) {
	if !HasOutcomeFields(r) {
		return
	}
	m.Records++
	if m.FindingsBySeverity == nil {
		m.FindingsBySeverity = map[string]int{}
	}
	switch OutcomeClass(r) {
	case "pass":
		m.Passes++
	case "fail":
		m.Failures++
	case "pending":
		m.Pending++
	case "with_fixes":
		m.WithFixes++
	}
	if r.FirstCIGreen != nil {
		m.FirstCIKnown++
		if *r.FirstCIGreen {
			m.FirstCIGreen++
		}
	}
	for severity, n := range r.FindingsBySeverity {
		m.FindingsBySeverity[normalize(severity)] += n
	}
	if r.RevertedWithin7d != nil {
		m.RevertKnown++
		if *r.RevertedWithin7d {
			m.RevertedWithin7d++
		}
	}
	if r.TimeToMergeS != nil {
		m.times = append(m.times, *r.TimeToMergeS)
	}
}
func (m *OutcomeMetrics) finish() {
	if m.FirstCIKnown > 0 {
		m.FirstCIGreenRate = ptr(float64(m.FirstCIGreen) / float64(m.FirstCIKnown))
	}
	if m.RevertKnown > 0 {
		m.RevertRate = ptr(float64(m.RevertedWithin7d) / float64(m.RevertKnown))
	}
	m.MergeTimeKnown = len(m.times)
	if len(m.times) > 0 {
		sort.Float64s(m.times)
		mean := 0.0
		for i, n := range m.times {
			mean += (n - mean) / float64(i+1)
		}
		m.MeanTimeToMergeS = &mean
		middle := len(m.times) / 2
		median := m.times[middle]
		if len(m.times)%2 == 0 {
			median = m.times[middle-1] + (median-m.times[middle-1])/2
		}
		m.MedianTimeToMergeS = &median
	}
}

type ExperimentOutcomeMetrics struct {
	ExperimentID string `json:"experiment_id"`
	Arm          string `json:"experiment_arm"`
	OutcomeMetrics
}
type RungOutcomeMetrics struct {
	Profile string `json:"profile"`
	Effort  string `json:"effort"`
	ModelID string `json:"model_id"`
	OutcomeMetrics
	Experiments []ExperimentOutcomeMetrics `json:"experiments"`
}

func summarizeOutcomes(row CatalogRow, reps []Rep) RungOutcomeMetrics {
	out := RungOutcomeMetrics{Profile: row.Profile, Effort: row.Effort, ModelID: row.ModelID, Experiments: []ExperimentOutcomeMetrics{}}
	groups := map[string]*ExperimentOutcomeMetrics{}
	for _, r := range reps {
		out.add(r)
		if !HasOutcomeFields(r) || (r.ExperimentID == "" && r.ExperimentArm == "") {
			continue
		}
		key := r.ExperimentID + "\x00" + r.ExperimentArm
		group := groups[key]
		if group == nil {
			group = &ExperimentOutcomeMetrics{ExperimentID: r.ExperimentID, Arm: r.ExperimentArm}
			groups[key] = group
		}
		group.add(r)
	}
	out.finish()
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		groups[key].finish()
		out.Experiments = append(out.Experiments, *groups[key])
	}
	return out
}
