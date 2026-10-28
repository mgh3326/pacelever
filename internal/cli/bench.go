package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func (a *App) bench(ctx context.Context, v args) (int, error) {
	switch v.at(1) {
	case "catalog":
		c := a.catalog(ctx)
		switch v.at(2) {
		case "list":
			if v.flags["json"] {
				return 0, a.json(map[string]any{"catalog": pl.CatalogWire(c.Rows), "source": c.Info})
			}
			for _, r := range c.Rows {
				fmt.Fprintf(a.Out, "%-3s %s@%s model=%s pool=%s gate=%s\n", r.Grade, r.Profile, r.Effort, r.ModelID, r.Pool, r.Gate)
			}
			fmt.Fprintf(a.Out, "catalog=%s stale=%t\n", c.Info.Source, c.Info.Stale)
			return 0, nil
		case "status":
			code := 0
			if v.flags["check"] && (c.Info.Source == "snapshot" || c.Info.Stale) {
				code = 2
			}
			return code, a.json(map[string]any{"catalog": c.Info, "rows": len(c.Rows), "credentials_configured": a.Bench.URL != "" && a.Bench.Token != ""})
		}
		return 2, fmt.Errorf("catalog list or status required")
	case "push-catalog":
		rows := []pl.CatalogRow{}
		if v.flags["emit-seed"] {
			rows = pl.BundledCatalog().Rows
			for i := range rows {
				rows[i].DecidedBy = v.get("decided-by")
				rows[i].DeviationRef = v.get("deviation-ref")
				rows[i].DecidedAt = time.Now().UTC().Format(time.RFC3339Nano)
			}
			return 0, a.json(map[string]any{"catalog": pl.CatalogWire(rows)})
		}
		path := v.at(2)
		if path == "" {
			return 2, fmt.Errorf("catalog JSON file required")
		}
		var payload struct {
			Catalog []pl.CatalogRow `json:"catalog"`
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return 2, fmt.Errorf("cannot read catalog file")
		}
		if json.Unmarshal(data, &payload) != nil {
			return 2, fmt.Errorf("invalid catalog JSON")
		}
		rows = payload.Catalog
		if len(rows) == 0 {
			return 2, fmt.Errorf("non-empty catalog required")
		}
		if e = pl.ValidateMonotonic(rows); e != nil {
			return 2, e
		}
		if v.flags["dry-run"] {
			return 0, a.json(map[string]any{"catalog": pl.CatalogWire(rows)})
		}
		return 0, a.Bench.PushCatalog(ctx, rows)
	case "grades":
		switch v.at(2) {
		case "list":
			rows, source, e := a.Bench.ReadRows(ctx, "grades")
			if e != nil {
				return 2, e
			}
			return 0, a.json(map[string]any{"grades": rows, "source": source})
		case "set":
			if e := v.required("profile", "grade", "deviation-ref"); e != nil {
				return 2, e
			}
			g := map[string]any{"profile": v.get("profile"), "grade": v.get("grade"), "deviation_ref": v.get("deviation-ref"), "boundary_version": v.get("boundary-version")}
			if v.flags["dry-run"] {
				return 0, a.json(g)
			}
			return 0, a.Bench.Send(ctx, "PUT", "/v1/bench/grades", "catalog", map[string]any{"grades": []any{g}}, nil)
		}
	case "show", "list", "coverage":
		rows, source, e := a.Bench.ReadRows(ctx, "scores")
		if e != nil {
			return 2, e
		}
		return 0, a.json(map[string]any{"scores": rows, "source": source})
	}
	return 2, fmt.Errorf("unknown bench command")
}
func (a *App) reps(ctx context.Context, v args) (int, error) {
	switch v.at(1) {
	case "add":
		if e := v.required("profile", "model", "task", "tier", "role", "grade", "rounds", "blockers-found", "completed"); e != nil {
			return 2, e
		}
		rounds, e := v.integer("rounds", 0)
		if e != nil {
			return 2, e
		}
		blockers, e := v.integer("blockers-found", 0)
		if e != nil {
			return 2, e
		}
		completed, e := v.integer("completed", -1)
		if e != nil {
			return 2, e
		}
		r := pl.Rep{Profile: v.get("profile"), ModelID: v.get("model"), TaskRef: v.get("task"), Tier: v.get("tier"), Role: v.get("role"), Grade: v.get("grade"), Rounds: rounds, BlockersFound: blockers, Completed: &completed, Effort: v.get("effort"), Notes: v.get("notes")}
		for _, key := range []string{"input-tokens", "output-tokens"} {
			if v.get(key) != "" {
				n, e := strconv.ParseInt(v.get(key), 10, 64)
				if e != nil || n < 0 {
					return 2, fmt.Errorf("invalid token count")
				}
				if key == "input-tokens" {
					r.InputTokens = &n
				} else {
					r.OutputTokens = &n
				}
			}
		}
		c := a.catalog(ctx)
		for _, row := range c.Rows {
			if row.Profile == r.Profile && row.Effort == r.Effort {
				r.TableGrade = row.Grade
				break
			}
		}
		if v.flags["dry-run"] {
			return 0, a.json(r)
		}
		record, e := a.Bench.AddRep(ctx, r)
		if e != nil {
			return 2, e
		}
		return 0, a.json(map[string]any{"ref": pl.RepRef(record), "rep": record})
	case "list", "compare":
		rows, source, e := a.Bench.ReadReps(ctx, v.get("profile"))
		if e != nil {
			return 2, e
		}
		filtered := []pl.Rep{}
		var areaLookup pl.TaskAreaLookup
		if v.get("area") != "" {
			areaLookup, e = a.Bench.ReadTaskAreas(ctx, rows)
			if e != nil {
				return 2, e
			}
		}
		for _, r := range rows {
			if v.get("area") != "" && areaLookup.Area(r.TaskRef) != v.get("area") {
				continue
			}
			if v.get("grade") != "" && v.get("grade") != r.Grade {
				continue
			}
			if v.get("effort") != "" && v.get("effort") != r.Effort {
				continue
			}
			filtered = append(filtered, r)
		}
		if v.at(1) == "compare" {
			stats := map[string]map[string]any{}
			for _, r := range filtered {
				key := r.Profile + "@" + r.Effort
				if stats[key] == nil {
					stats[key] = map[string]any{"count": 0, "completed": 0, "rounds": 0, "blockers": 0}
				}
				s := stats[key]
				s["count"] = s["count"].(int) + 1
				s["rounds"] = s["rounds"].(int) + r.Rounds
				s["blockers"] = s["blockers"].(int) + r.BlockersFound
				if r.Completed != nil && *r.Completed == 1 {
					s["completed"] = s["completed"].(int) + 1
				}
			}
			return 0, a.json(map[string]any{"profiles": stats, "source": source})
		}
		limit, e := v.integer("limit", len(filtered))
		if e != nil || limit < 0 {
			return 2, fmt.Errorf("invalid rep limit")
		}
		if len(filtered) > limit {
			filtered = filtered[:limit]
		}
		return 0, a.json(map[string]any{"reps": filtered, "source": source})
	case "pending":
		rows, _, e := a.Bench.ReadReps(ctx, "")
		if e != nil {
			return 2, e
		}
		pending := []pl.Rep{}
		for _, r := range rows {
			if r.ID == 0 && r.LocalID == 0 {
				pending = append(pending, r)
			}
		}
		if v.at(2) == "list" {
			return 0, a.json(pending)
		}
		if v.at(2) == "retry" {
			id, e := strconv.ParseInt(strings.TrimPrefix(v.at(3), "origin:"), 10, 64)
			if e != nil {
				return 2, e
			}
			for _, r := range pending {
				if r.OriginID == id {
					record, e := a.Bench.RetryRep(ctx, r)
					if e != nil {
						return 2, e
					}
					return 0, a.json(record)
				}
			}
			return 2, fmt.Errorf("pending origin not found")
		}
	}
	return 2, fmt.Errorf("unknown reps command")
}
func (a *App) grades(ctx context.Context, v args) (int, error) {
	c := a.catalog(ctx)
	reps, source, e := a.Bench.ReadReps(ctx, "")
	if e != nil {
		return 2, e
	}
	switch v.at(1) {
	case "propose":
		min, e := v.integer("min-passes", 2)
		if e != nil || min < 1 {
			return 2, fmt.Errorf("invalid --min-passes")
		}
		window, e := v.integer("demote-window", 5)
		if e != nil || window < 1 {
			return 2, fmt.Errorf("invalid --demote-window")
		}
		rate, e := v.float("demote-rate", 0.4)
		if e != nil || rate <= 0 || rate > 1 {
			return 2, fmt.Errorf("invalid --demote-rate")
		}
		p := pl.ProposeGrades(c, reps, source, min, window, rate)
		if v.get("out") != "" {
			if e = pl.WriteJSON(v.get("out"), p); e != nil {
				return 2, e
			}
		}
		return 0, a.json(p)
	case "apply":
		if e := v.required("out", "decided-by", "deviation-ref"); e != nil {
			return 2, e
		}
		var p pl.GradeProposal
		if e = pl.ReadJSON(v.at(2), &p); e != nil {
			return 2, e
		}
		only := map[string]bool{}
		if v.get("only") != "" {
			for _, key := range strings.Split(v.get("only"), ",") {
				only[key] = true
			}
		}
		rows, e := pl.ApplyGrades(p, c, reps, only, v.get("decided-by"), v.get("deviation-ref"), v.get("allow-degraded"), time.Now().UTC())
		if e != nil {
			return 2, e
		}
		payload := map[string]any{"catalog": pl.CatalogWire(rows)}
		if e = pl.WriteJSON(v.get("out"), payload); e != nil {
			return 2, e
		}
		fmt.Fprintln(a.Out, "wrote reviewed catalog artifact", v.get("out"))
		return 0, nil
	case "areas":
		lookup, e := a.Bench.ReadTaskAreas(ctx, reps)
		if e != nil {
			return 2, e
		}
		return 0, a.json(map[string]any{"cells": pl.GradeAreas(reps, lookup), "source": source})
	}
	return 2, fmt.Errorf("unknown grades command")
}
