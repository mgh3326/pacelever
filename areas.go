package pacelever

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func TaskID(ref string) int64 {
	text := strings.TrimSpace(ref)
	if id, e := strconv.ParseInt(text, 10, 64); e == nil && id > 0 {
		return id
	}
	matches := regexp.MustCompile(`(?i)\bhk(?::task/|[\s:#-]*)([0-9]+)\b`).FindAllStringSubmatch(text, -1)
	ids := map[int64]bool{}
	for _, m := range matches {
		id, _ := strconv.ParseInt(m[1], 10, 64)
		if id > 0 {
			ids[id] = true
		}
	}
	if len(ids) != 1 {
		return 0
	}
	for id := range ids {
		return id
	}
	return 0
}

type TaskAreaLookup struct{ Areas map[int64]string }

func (b BenchClient) ReadTaskAreas(ctx context.Context, reps []Rep) (TaskAreaLookup, error) {
	lookup := TaskAreaLookup{Areas: map[int64]string{}}
	if !b.remote("catalog") {
		return lookup, fmt.Errorf("task-area metadata requires handoffkeep backend")
	}
	for _, r := range reps {
		id := TaskID(r.TaskRef)
		if id == 0 {
			continue
		}
		if _, exists := lookup.Areas[id]; exists {
			continue
		}
		var payload struct {
			Task struct {
				Area *string `json:"area"`
			} `json:"task"`
		}
		e := b.Send(ctx, "GET", "/v1/tasks/"+strconv.FormatInt(id, 10), "catalog", nil, &payload)
		if e != nil {
			if backend, ok := e.(*BackendError); ok && backend.Status == 404 {
				lookup.Areas[id] = "unlinked"
				continue
			}
			return lookup, e
		}
		area := "untagged"
		if payload.Task.Area != nil && strings.TrimSpace(*payload.Task.Area) != "" {
			area = *payload.Task.Area
		}
		lookup.Areas[id] = area
	}
	return lookup, nil
}
func (l TaskAreaLookup) Area(ref string) string {
	id := TaskID(ref)
	if id == 0 {
		return "unlinked"
	}
	if area, ok := l.Areas[id]; ok {
		return area
	}
	return "unlinked"
}

type AreaCell struct {
	Band       string   `json:"band"`
	Role       string   `json:"role"`
	Area       string   `json:"area"`
	Profile    string   `json:"profile"`
	Effort     string   `json:"effort"`
	ModelID    string   `json:"model_id"`
	N          int      `json:"n"`
	Clean      int      `json:"clean"`
	CleanPct   *float64 `json:"clean_pct"`
	MeanRounds *float64 `json:"mean_rounds"`
	Eligible   bool     `json:"eligible"`
	rounds     int
}

func GradeAreas(reps []Rep, lookup TaskAreaLookup) []AreaCell {
	cells := map[string]*AreaCell{}
	latest := map[string]Rep{}
	for _, r := range reps {
		if r.Pending || r.Completed == nil || r.ModelID == "" || r.Grade == "" {
			continue
		}
		role := r.Role
		if role == "impl" || role == "fix" {
			role = "builder"
		} else if role == "verify" {
			role = "tester"
		} else {
			continue
		}
		band := r.Grade
		if band == "S+" {
			band = "S"
		}
		if band == "A+" {
			band = "A"
		}
		key := band + "\x00" + role + "\x00" + r.Profile + "\x00" + r.Effort + "\x00" + r.ModelID + "\x00" + r.TaskRef
		old, ok := latest[key]
		if !ok || r.RecordedAt > old.RecordedAt {
			latest[key] = r
		}
	}
	for _, r := range latest {
		role := r.Role
		if role == "impl" || role == "fix" {
			role = "builder"
		} else {
			role = "tester"
		}
		band := r.Grade
		if band == "S+" {
			band = "S"
		}
		if band == "A+" {
			band = "A"
		}
		area := lookup.Area(r.TaskRef)
		key := band + "\x00" + role + "\x00" + area + "\x00" + r.Profile + "\x00" + r.Effort + "\x00" + r.ModelID
		cell := cells[key]
		if cell == nil {
			cell = &AreaCell{Band: band, Role: role, Area: area, Profile: r.Profile, Effort: r.Effort, ModelID: r.ModelID}
			cells[key] = cell
		}
		cell.N++
		cell.rounds += r.Rounds
		if *r.Completed == 1 && r.BlockersFound == 0 && !RepFailure(r) {
			cell.Clean++
		}
	}
	out := make([]AreaCell, 0, len(cells))
	for _, cell := range cells {
		cell.CleanPct = ptr(100 * float64(cell.Clean) / float64(cell.N))
		cell.MeanRounds = ptr(float64(cell.rounds) / float64(cell.N))
		cell.Eligible = cell.N >= 5
		out = append(out, *cell)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		return a.Band+"/"+a.Role+"/"+a.Area+"/"+a.Profile+"/"+a.Effort+"/"+a.ModelID < b.Band+"/"+b.Role+"/"+b.Area+"/"+b.Profile+"/"+b.Effort+"/"+b.ModelID
	})
	return out
}
func (b BenchClient) ReadAreaExceptions(ctx context.Context) ([]AreaException, error) {
	if b.localOnly("catalog") {
		// Exceptions are server-side only; a local-only host has none.
		return nil, nil
	}
	raw, _, e := b.ReadRows(ctx, "area-exceptions")
	if e != nil {
		if backend, ok := e.(*BackendError); ok && backend.Status == 404 {
			return nil, nil
		}
		return nil, e
	}
	var rows []AreaException
	for _, v := range raw {
		if v["retired_at"] != nil && str(v["retired_at"]) != "" {
			continue
		}
		action := str(v["action"])
		if action != "prefer" && action != "exclude" {
			return nil, fmt.Errorf("invalid area exception action")
		}
		rows = append(rows, AreaException{Profile: str(v["profile"]), Effort: str(v["effort"]), ModelID: str(v["expires_with_model_id"]), Area: str(v["area"]), Action: action, Reason: str(v["decision_ref"])})
	}
	return rows, nil
}
