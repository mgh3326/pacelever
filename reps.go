package pacelever

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Rep struct {
	FirstCIGreen       *bool          `json:"first_ci_green,omitempty"`
	FindingsBySeverity map[string]int `json:"findings_by_severity"`
	RevertedWithin7d   *bool          `json:"reverted_within_7d,omitempty"`
	TimeToMergeS       *float64       `json:"time_to_merge_s,omitempty"`
	ExperimentID       string         `json:"experiment_id,omitempty"`
	ExperimentArm      string         `json:"experiment_arm,omitempty"`
	OutcomeFinalized   *bool          `json:"outcome_finalized,omitempty"`
	OutcomeDecision    string         `json:"outcome_decision,omitempty"`
	LocalID            int64          `json:"local_id,omitempty"`
	ID                 int64          `json:"id,omitempty"`
	OriginID           int64          `json:"origin_id"`
	Profile            string         `json:"profile"`
	ModelID            string         `json:"model_id"`
	TaskRef            string         `json:"task_ref"`
	Tier               string         `json:"tier"`
	Role               string         `json:"role"`
	Rounds             int            `json:"rounds"`
	BlockersFound      int            `json:"blockers_found"`
	Completed          *int           `json:"completed"`
	InputTokens        *int64         `json:"input_tokens"`
	OutputTokens       *int64         `json:"output_tokens"`
	Notes              string         `json:"notes"`
	RecordedAt         string         `json:"recorded_at"`
	Effort             string         `json:"effort"`
	Grade              string         `json:"grade"`
	TableGrade         string         `json:"table_grade"`
	Pending            bool           `json:"pending,omitempty"`
}

func repsPath() string { return filepath.Join(CacheDir(), "reps.json") }
func localReps() ([]Rep, error) {
	var rows []Rep
	e := ReadJSON(repsPath(), &rows)
	if os.IsNotExist(e) {
		return []Rep{}, nil
	}
	return rows, e
}
func repWire(r Rep) map[string]any {
	return map[string]any{"origin_id": r.OriginID, "profile": r.Profile, "model_id": r.ModelID, "task_ref": r.TaskRef, "tier": r.Tier, "role": r.Role, "rounds": r.Rounds, "blockers_found": r.BlockersFound, "completed": r.Completed, "input_tokens": r.InputTokens, "output_tokens": r.OutputTokens, "notes": NotesForWire(r), "recorded_at": r.RecordedAt, "effort": r.Effort, "grade": r.Grade, "table_grade": r.TableGrade}
}
func saveRep(r Rep) error {
	return WithFileLock(repsPath()+".lock", false, func() error {
		rows, e := localReps()
		if e != nil {
			return e
		}
		found := false
		for i := range rows {
			if rows[i].OriginID == r.OriginID {
				rows[i] = r
				found = true
			}
		}
		if !found {
			rows = append(rows, r)
		}
		return WriteJSON(repsPath(), rows)
	})
}

// AddRep writes a durable intent before sending it. A failed/uncertain request
// retains the exact origin id so RetryRep cannot duplicate successful writes.
func (b BenchClient) AddRep(ctx context.Context, r Rep) (Rep, error) {
	if e := HydrateRepOutcomes(&r); e != nil {
		return r, e
	}
	if r.Profile == "" || r.ModelID == "" || r.TaskRef == "" || r.Rounds < 0 || r.BlockersFound < 0 || (!HasOutcomeFields(r) && r.Completed == nil) || (r.Completed != nil && *r.Completed != 0 && *r.Completed != 1) {
		return r, fmt.Errorf("rep requires profile, model, task, valid rounds/blockers and completed 0/1")
	}
	if r.Grade != "" && gradeRank(r.Grade) == len(Grades) {
		return r, fmt.Errorf("invalid rep grade")
	}
	if r.Tier != "T0" && r.Tier != "T1" && r.Tier != "T2" && r.Tier != "T3" {
		return r, fmt.Errorf("invalid rep tier")
	}
	if r.Role != "impl" && r.Role != "verify" && r.Role != "fix" && r.Role != "orch" {
		return r, fmt.Errorf("invalid rep role")
	}
	if r.OriginID == 0 {
		var v [8]byte
		if _, e := rand.Read(v[:]); e != nil {
			return r, e
		}
		r.OriginID = int64(binary.BigEndian.Uint64(v[:]) & ((1 << 53) - 1))
		if r.OriginID == 0 {
			r.OriginID = 1
		}
	}
	if r.RecordedAt == "" {
		r.RecordedAt = b.now().Format(time.RFC3339Nano)
	}
	if _, e := ParseTime(r.RecordedAt); e != nil {
		return r, fmt.Errorf("invalid rep timestamp")
	}
	r.Pending = b.remote("reps") || b.Config.Bench.Backend == "handoffkeep"
	if e := saveRep(r); e != nil {
		return r, e
	}
	return b.RetryRep(ctx, r)
}
func (b BenchClient) RetryRep(ctx context.Context, r Rep) (Rep, error) {
	if !b.remote("reps") {
		if b.Config.Bench.Backend == "handoffkeep" {
			return r, fmt.Errorf("rep saved pending; backend unavailable")
		}
		return r, nil
	}
	var result struct {
		IDs []int64 `json:"ids"`
	}
	e := b.Send(ctx, "PUT", "/v1/bench/reps", "reps", map[string]any{"reps": []map[string]any{repWire(r)}}, &result)
	if e != nil {
		return r, fmt.Errorf("rep saved pending: %w", e)
	}
	if len(result.IDs) != 1 || result.IDs[0] <= 0 {
		return r, fmt.Errorf("rep saved pending: server id missing")
	}
	r.ID = result.IDs[0]
	r.Pending = false
	if e = saveRep(r); e != nil {
		return r, e
	}
	return r, nil
}
func (b BenchClient) ReadReps(ctx context.Context, profile string) ([]Rep, string, error) {
	raw, source, e := b.ReadRows(ctx, "reps")
	if e != nil && source != BenchLocal {
		return nil, source, e
	}
	var rows []Rep
	seen := map[string]bool{}
	for _, v := range raw {

		data, _ := json.Marshal(v)
		var r Rep
		if json.Unmarshal(data, &r) != nil {
			return nil, source, fmt.Errorf("invalid rep field types")
		}
		if err := HydrateRepOutcomes(&r); err != nil {
			return nil, source, err
		}

		if profile == "" || r.Profile == profile {
			rows = append(rows, r)
			seen[fmt.Sprintf("%d", r.ID)] = true
		}
	}
	local, err := localReps()
	if err != nil {
		return nil, source, err
	}
	for _, r := range local {
		if err := HydrateRepOutcomes(&r); err != nil {
			return nil, source, err
		}
		if profile != "" && r.Profile != profile {
			continue
		}
		if r.ID > 0 && seen[fmt.Sprintf("%d", r.ID)] {
			continue
		}
		r.Pending = r.ID == 0 && b.remote("reps")
		rows = append(rows, r)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].RecordedAt > rows[j].RecordedAt })
	if len(raw) >= 5000 && source != BenchLocal {
		source += "-truncated"
	}
	return rows, source, e
}
func RepRef(r Rep) string {
	if r.LocalID > 0 {
		return fmt.Sprintf("local:%d", r.LocalID)
	}
	if r.ID > 0 {
		return fmt.Sprintf("srv:%d", r.ID)
	}
	return fmt.Sprintf("origin:%d", r.OriginID)
}
func RepFailure(r Rep) bool {
	if HasOutcomeFields(r) {
		return OutcomeClass(r) == "fail"
	}
	return r.Completed != nil && *r.Completed == 0 || strings.Contains(strings.ToLower(r.Notes), "[rollback]") || strings.Contains(strings.ToLower(r.Notes), "[post-merge-blocker]")
}

func PendingReps() ([]Rep, error) {
	rows, e := localReps()
	if e != nil {
		return nil, e
	}
	out := []Rep{}
	for _, r := range rows {
		if r.Pending && r.ID == 0 {
			out = append(out, r)
		}
	}
	return out, nil
}
func DropPendingRep(origin int64) error {
	return WithFileLock(repsPath()+".lock", false, func() error {
		rows, e := localReps()
		if e != nil {
			return e
		}
		out := []Rep{}
		found := false
		for _, r := range rows {
			if r.OriginID == origin && r.Pending && r.ID == 0 {
				found = true
				continue
			}
			out = append(out, r)
		}
		if !found {
			return fmt.Errorf("pending origin not found")
		}
		return WriteJSON(repsPath(), out)
	})
}
