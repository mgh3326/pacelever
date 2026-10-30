package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func table(m map[string]any, name string) map[string]any {
	if t, ok := m[name].(map[string]any); ok {
		return t
	}
	t := map[string]any{}
	m[name] = t
	return t
}
func (a *App) policy(ctx context.Context, v args) (int, error) {
	switch v.at(1) {
	case "launch":
		req := request(v)
		if req.Profile == "" {
			req.Profile = v.at(2)
		}
		if req.Profile == "" {
			return pl.Invalid, fmt.Errorf("profile required")
		}
		catalog := a.catalog(ctx)
		d, e := pl.ResolveLaunch(catalog, a.Config, req)
		if e == nil && a.Config.Pools[d.Pool].Exploration.Active() {
			inputs := pl.Inputs{Catalog: catalog, Config: a.Config, Now: a.now()}
			inputs.FreeLimit = pl.ActiveFreeLimit(inputs.Now)
			if strings.TrimSpace(req.TaskID) != "" && req.RequiredGrade != "" {
				var inputErr error
				inputs, inputErr = a.inputs(ctx, catalog, []string{d.Pool})
				if inputErr != nil {
					return pl.Invalid, inputErr
				}
			}
			d, e = pl.ResolveLaunchWithInputs(inputs, req)
		}
		if e != nil {
			return pl.Blocked, e
		}
		if v.flags["json"] {
			return 0, a.json(d)
		}
		fmt.Fprintf(a.Out, "profile %s\nmodel_id %s\neffort %s\npool %s\nfamily %s\nbilling %s\ngate %s\ngrade %s\ncatalog %s\n", d.Profile, d.ModelID, d.Effort, d.Pool, d.Family, d.Billing, d.Gate, d.Grade, d.Catalog.Source)
		fmt.Fprintf(a.Out, "arm %s\nexperiment_id %s\n", d.Label.Arm, labelID(d.Label))
		return 0, nil
	case "list":
		if v.flags["json"] {
			return 0, a.json(map[string]any{"pools": a.Config.Pools, "profiles": a.Config.Profiles, "settings": a.Config.Settings})
		}
		for _, p := range append(append([]string{}, pl.ProviderIDs...), "anthropic-api") {
			policy := a.Config.Pool(p, a.now())
			fmt.Fprintf(a.Out, "%s class=%s boost=%g cutoff=%g subscribed=%t\n", p, policy.Class, policy.Boost, policy.Cutoff, a.Config.Subscribed("", p))
		}
		return 0, nil
	case "set", "clear":
		pool := v.at(2)
		known := pool == "anthropic-api"
		for _, p := range pl.ProviderIDs {
			if pool == p {
				known = true
			}
		}
		if !known {
			return pl.Invalid, fmt.Errorf("unknown pool")
		}
		e := pl.EditConfig(func(raw map[string]any) error {
			entry := table(table(raw, "pools"), pool)
			if v.at(1) == "clear" {
				for _, k := range []string{"class", "until", "boost", "note"} {
					delete(entry, k)
				}
				return nil
			}
			class := v.at(3)
			if class != "" && class != "spend" && class != "preserve" && class != "exclude" {
				return fmt.Errorf("class must be spend, preserve or exclude")
			}
			if class != "" || v.get("boost") != "" && v.get("boost") != "none" {
				if _, e := time.Parse("2006-01-02", v.get("until")); e != nil {
					return fmt.Errorf("--until YYYY-MM-DD required")
				}
				entry["until"] = v.get("until")
			}
			changed := false
			if class != "" {
				entry["class"] = class
				changed = true
			}
			if v.get("note") != "" {
				entry["note"] = v.get("note")
				changed = true
			}
			if value := v.get("boost"); value != "" {
				changed = true
				if value == "none" {
					delete(entry, "boost")
				} else {
					boost, ok := pl.ParseFinite(value)
					if !ok || boost < 0 {
						return fmt.Errorf("invalid boost")
					}
					entry["boost"] = boost
				}
			}
			if value := v.get("subscribed"); value != "" {
				changed = true
				switch value {
				case "on":
					entry["subscribed"] = true
				case "off":
					entry["subscribed"] = false
				case "none":
					delete(entry, "subscribed")
				default:
					return fmt.Errorf("subscribed must be on, off or none")
				}
			}
			if value := v.get("fusion-billing-fp"); value != "" {
				changed = true
				if value == "none" {
					delete(entry, "fusion_billing_fp")
				} else {
					entry["fusion_billing_fp"] = value
				}
			}
			if reserve := v.values["inflight-reserve"]; len(reserve) > 0 {
				changed = true
				if len(reserve) == 1 && reserve[0] == "none" {
					delete(entry, "inflight_reserve_pct")
				} else {
					r := map[string]any{}
					for _, item := range reserve {
						window, pct, ok := strings.Cut(item, "=")
						n, finite := pl.ParseFinite(pct)
						if !ok || !finite || n < 0 || n > 100 {
							return fmt.Errorf("invalid WINDOW=PCT reserve")
						}
						r[window] = n
					}
					entry["inflight_reserve_pct"] = r
				}
			}
			if !changed {
				return fmt.Errorf("no policy change specified")
			}
			return nil
		})
		if e == nil {
			fmt.Fprintln(a.Out, "policy updated")
		}
		return 0, e
	case "profile":
		name, state := v.at(2), v.at(3)
		if name == "" {
			return pl.Invalid, fmt.Errorf("profile name required")
		}
		e := pl.EditConfig(func(raw map[string]any) error {
			entry := table(table(raw, "profiles"), name)
			switch state {
			case "on":
				entry["subscribed"] = true
			case "off":
				entry["subscribed"] = false
			case "clear", "none":
				delete(entry, "subscribed")
			default:
				return fmt.Errorf("profile state must be on, off or clear")
			}
			return nil
		})
		if e == nil {
			fmt.Fprintln(a.Out, "profile policy updated")
		}
		return 0, e
	case "setting":
		name, value := v.at(2), v.at(3)
		if name == "" {
			return 0, a.json(map[string]any{"altlane_pace_multiple": a.Config.PaceThreshold()})
		}
		if name != "altlane_pace_multiple" {
			return pl.Invalid, fmt.Errorf("unknown policy setting")
		}
		if value == "" {
			fmt.Fprintln(a.Out, a.Config.PaceThreshold())
			return 0, nil
		}
		return 0, pl.EditConfig(func(raw map[string]any) error {
			settings := table(raw, "settings")
			if value == "none" {
				delete(settings, name)
				return nil
			}
			n, ok := pl.ParseFinite(value)
			if !ok || n <= 0 {
				return fmt.Errorf("setting must be a finite positive number")
			}
			settings[name] = n
			return nil
		})
	}
	return pl.Invalid, fmt.Errorf("unknown policy command")
}
func (a *App) manual(ctx context.Context, v args) (int, error) {
	pool := v.at(2)
	if pool == "" {
		pool = v.get("pool")
	}
	switch v.at(1) {
	case "list":
		s, e := a.Collector.ReadManual(ctx)
		if e != nil {
			return pl.Invalid, e
		}
		if pool == "" {
			return 0, a.json(s)
		}
		return 0, a.json(s.Valid(pool, a.now()))
	case "clear":
		if pool == "" {
			return pl.Invalid, fmt.Errorf("pool required")
		}
		return 0, a.Collector.ClearManual(ctx, pool)
	case "set":
		creditForm := firstValue(v.get("remaining"), v.get("total"), v.get("expires-on"), v.get("credit-balance"), v.get("credit-total"), v.get("expires")) != ""
		if !creditForm {
			if e := v.required("measured-at", "reason"); e != nil {
				return pl.Invalid, e
			}
		} else if pool == "" {
			return pl.Invalid, fmt.Errorf("pool required")
		}
		measured, reason := v.get("measured-at"), v.get("reason")
		if creditForm {
			if measured == "" {
				measured = a.now().Format(time.RFC3339Nano)
			}
			if reason == "" {
				reason = "credit balance observed"
			}
		}
		m := pl.ManualObservation{Pool: pool, Window: v.get("window"), MeasuredAt: measured, Reason: reason, ResetsAt: v.get("resets-at")}
		if v.get("used") != "" {
			used, e := v.float("used", 0)
			if e != nil {
				return pl.Invalid, e
			}
			m.UsedPct = pl.Pct(used)
		}
		balance, total, expires := v.get("credit-balance"), v.get("credit-total"), v.get("expires")
		balanceFlag, totalFlag := "credit-balance", "credit-total"
		if balance == "" {
			balance, balanceFlag = v.get("remaining"), "remaining"
		}
		if total == "" {
			total, totalFlag = v.get("total"), "total"
		}
		if expires == "" {
			expires = v.get("expires-on")
		}
		if balance != "" || total != "" || expires != "" {
			remaining, e := v.float(balanceFlag, -1)
			if e != nil {
				return pl.Invalid, e
			}
			totalUSD, e := v.float(totalFlag, 0)
			if e != nil {
				return pl.Invalid, e
			}
			m.Credit = &pl.Credit{RemainingUSD: remaining, TotalUSD: totalUSD, ExpiresOn: expires}
		}
		ttl := time.Duration(0)
		if v.get("ttl") != "" {
			var e error
			ttl, e = pl.Duration(v.get("ttl"))
			if e != nil {
				return pl.Invalid, e
			}
		}
		if v.get("resets-in") != "" {
			if m.ResetsAt != "" {
				return pl.Invalid, fmt.Errorf("reset options are mutually exclusive")
			}
			duration, e := pl.Duration(v.get("resets-in"))
			if e != nil {
				return pl.Invalid, e
			}
			at, e := pl.ParseTime(m.MeasuredAt)
			if e != nil {
				return pl.Invalid, e
			}
			m.ResetsAt = at.Add(duration).Format(time.RFC3339Nano)
		}
		r, e := a.Collector.AddManual(ctx, m, ttl, a.now())
		if e != nil {
			return pl.Invalid, e
		}
		return 0, a.json(r)
	}
	return pl.Invalid, fmt.Errorf("unknown manual command")
}

func firstValue(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func labelID(label pl.ExperimentLabel) string {
	if label.ExperimentID == nil {
		return "-"
	}
	return *label.ExperimentID
}
