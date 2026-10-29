package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	pl "github.com/mgh3326/pacelever"
)

type App struct {
	Out       io.Writer
	Err       io.Writer
	In        io.Reader
	Version   string
	Commit    string
	Config    pl.Config
	Bench     pl.BenchClient
	Collector pl.Collector
}

func Run(ctx context.Context, argv []string, out, errOut io.Writer, in io.Reader, version, commit string) int {
	a, e := parse(argv)
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	if a.flags["help"] {
		fmt.Fprint(out, help)
		return 0
	}
	if a.flags["version"] || a.at(0) == "version" {
		fmt.Fprintf(out, "pacelever %s %s\n", version, commit)
		return 0
	}
	app := App{Out: out, Err: errOut, In: in, Version: version, Commit: commit}
	cfg, e := pl.LoadConfig()
	if e != nil {
		fmt.Fprintln(errOut, e)
		return 2
	}
	app.Config = cfg
	for _, w := range cfg.Warnings {
		fmt.Fprintln(errOut, "warning:", w)
	}
	if a.flags["allow-plaintext-http"] {
		app.Config.Bench.AllowPlaintextCatalog = true
		app.Config.Bench.AllowPlaintextReps = true
	}
	app.Bench = pl.NewBenchClient(app.Config)
	app.Collector = pl.Collector{Config: app.Config, Providers: pl.DefaultProviders(app.Config), NoCache: a.flags["no-cache"]}
	for k, p := range app.Collector.Providers {
		if probe, ok := p.(pl.PTYProvider); ok {
			probe.Guard = pl.GuardProcess
			app.Collector.Providers[k] = probe
		}
	}
	if a.get("cache-ttl") != "" {
		v, e := a.float("cache-ttl", 0)
		if e != nil || v < 0 || v > 1e7 {
			fmt.Fprintln(errOut, "invalid --cache-ttl")
			return 2
		}
		app.Collector.TTL = time.Duration(v * float64(time.Second))
		if v == 0 {
			app.Collector.NoCache = true
		}
	}
	code, e := app.run(ctx, a)
	if e != nil {
		fmt.Fprintln(errOut, "error:", e)
		if code == 0 {
			code = 2
		}
	}
	return code
}
func (a *App) json(v any) error {
	enc := json.NewEncoder(a.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}
func (a *App) catalog(ctx context.Context) pl.Catalog {
	c, e := a.Bench.ReadCatalog(ctx)
	if e != nil {
		fmt.Fprintln(a.Err, "warning: catalog unavailable; serving stale bundled snapshot")
	}
	return c
}
func request(a args) pl.Request {
	return pl.Request{TaskID: a.get("task"), RequiredGrade: a.get("grade"), Profile: a.get("profile"), Effort: a.get("effort"), Role: a.get("role"), Purpose: a.get("purpose"), OperatorRequest: a.get("operator-request"), RequestedBy: a.get("requested-by"), PaceOverride: a.get("pace-override"), E6Arm: firstEnv("PACELEVER_E6_ARM", "SCOPEFUEL_E6_ARM"), Area: a.get("area")}
}
func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := os.Getenv(k); v != "" {
			return v
		}
	}
	return ""
}
func (a *App) localManual() (pl.ManualStore, error) {
	if a.Config.Quota.URL != "" {
		return pl.ManualStore{}, nil
	}
	return pl.LoadManual()
}
func (a *App) inputs(ctx context.Context, c pl.Catalog, pools []string) (pl.Inputs, error) {
	if a.Config.Quota.URL != "" {
		found := false
		for _, pool := range pools {
			if pool == "anthropic-api" {
				found = true
			}
		}
		if !found {
			pools = append(append([]string(nil), pools...), "anthropic-api")
		}
	}
	observations, e := a.Collector.Collect(ctx, pools)
	if e != nil {
		return pl.Inputs{}, e
	}
	manual, e := a.localManual()
	if e != nil {
		return pl.Inputs{}, e
	}
	manual = a.Collector.ManualInputs(observations, manual)
	now := time.Now().UTC()
	in := pl.Inputs{Catalog: c, Config: a.Config, Observations: observations, Manual: manual, Now: now, Inflight: map[string]*int{}}
	for i, o := range in.Observations {
		if a.Config.Quota.URL == "" && (o.Error != "" || o.Stale) {
			in.Observations[i] = a.Bench.QuotaFallback(ctx, o, "")
		}
		if len(a.Config.Pools[o.ID].InflightReservePct) > 0 {
			fp := o.PublishFP
			if fp == "" {
				fp = o.AccountFP
			}
			local, e := pl.LocalInflight(ctx, o.ID, now)
			read := a.Bench.Inflight(ctx, o.ID, fp, local, e == nil)
			in.Inflight[o.ID] = read.N
			if e == nil && (o.AccountFPKind == "account" || o.PublishFPKind == "account") {
				_ = a.Bench.PublishInflight(ctx, o.ID, fp, local)
			}
		}
	}
	// Fusion lead billing uses the configured account independently of the
	// host's default login, with the same peer-snapshot identity checks.
	for _, r := range c.Rows {
		if strings.HasPrefix(r.Profile, "devin-fusion-sol61") {
			fp := a.Config.Pools[r.Pool].FusionBillingFP
			if fp != "" && a.Config.Quota.URL == "" {
				for i, o := range in.Observations {
					if o.ID == r.Pool && o.PublishFP != fp && o.AccountFP != fp {
						remote := a.Bench.QuotaFallback(ctx, o, fp)
						if remote.Source == "remote" {
							in.Observations[i] = remote
						}
					}
				}
			}
		}
	}
	return in, nil
}
func (a *App) run(ctx context.Context, v args) (int, error) {
	switch v.at(0) {
	case "gate":
		return a.gate(ctx, v)
	case "policy":
		return a.policy(ctx, v)
	case "manual":
		return a.manual(ctx, v)
	case "bench":
		return a.bench(ctx, v)
	case "reps":
		return a.reps(ctx, v)
	case "grades":
		return a.grades(ctx, v)
	case "refresh":
		pool := v.at(1)
		if v.flags["background"] {
			return 0, pl.StartRefresh(pool)
		}
		ctx, cancel := context.WithTimeout(ctx, 50*time.Second)
		defer cancel()
		o, e := a.Collector.Refresh(ctx, pool)
		if e != nil {
			return 2, e
		}
		if o.Error != "" {
			return 4, fmt.Errorf("%s", o.Error)
		}
		if a.Config.Quota.URL == "" {
			_ = a.Bench.PublishQuota(ctx, o)
		}
		return 0, a.json(o)
	case "observe":
		pool := "devin"
		if v.at(1) == "commandcode-limit" {
			pool = "commandcode"
		} else if v.at(1) != "devin-free-limit" {
			return 2, fmt.Errorf("unknown observation")
		}
		r := pl.LimitRecord{ResetAt: v.get("reset"), ObservedAt: v.get("observed"), Rung: v.get("rung"), Source: v.get("source"), Unparsed: v.flags["unparsed"]}
		return 0, pl.ObserveLimit(pool, r, v.flags["clear"], v.get("account-key"), time.Now().UTC())
	case "inflight":
		if v.at(1) != "show" || v.get("pool") == "" {
			return 2, fmt.Errorf("inflight show --pool required")
		}
		pool := v.get("pool")
		local, e := pl.LocalInflight(ctx, pool, time.Now().UTC())
		entries, _ := pl.LoadSnapshots()
		o := entries[pool].Result
		fp := o.PublishFP
		if fp == "" {
			fp = o.AccountFP
		}
		return 0, a.json(a.Bench.Inflight(ctx, pool, fp, local, e == nil))
	case "herdr-event":
		return a.herdr(ctx)
	case "models":
		return a.models(ctx, v)
	case "":
	default:
		return 2, fmt.Errorf("unknown command %s", v.at(0))
	}
	if v.flags["list-providers"] {
		for _, p := range pl.ProviderIDs {
			fmt.Fprintln(a.Out, p)
		}
		return 0, nil
	}
	c := a.catalog(ctx)
	if v.flags["list-recommend-profiles"] {
		seen := map[string]bool{}
		for _, r := range c.Rows {
			if r.Placed && !seen[r.Profile] {
				seen[r.Profile] = true
				suffix := ""
				if !a.Config.Subscribed(r.Profile, r.Pool) {
					suffix = " [unsubscribed]"
				}
				fmt.Fprintln(a.Out, r.Profile+suffix)
			}
		}
		return 0, nil
	}
	pools := append([]string(nil), pl.ProviderIDs...)
	if v.get("only") != "" {
		pools = strings.Split(v.get("only"), ",")
		for _, p := range pools {
			found := false
			for _, known := range pl.ProviderIDs {
				if known == p {
					found = true
				}
			}
			if !found {
				return 2, fmt.Errorf("unknown provider %s", p)
			}
		}
	}
	for {
		in, e := a.inputs(ctx, c, pools)
		if e != nil {
			return 2, e
		}
		if grade := v.get("recommend"); grade != "" {
			found := false
			for _, g := range pl.Grades {
				if g == grade {
					found = true
				}
			}
			if !found {
				return 2, fmt.Errorf("unknown grade")
			}
			if area := v.get("area"); area != "" {
				exceptions, e := a.Bench.ReadAreaExceptions(ctx)
				if e != nil {
					return 2, e
				}
				in.AreaExceptions = exceptions
			}
			rows := pl.RecommendFor(in, pl.RecommendationRequest{Grade: grade, Role: v.get("role"), Area: v.get("area"), TaskID: v.get("task")})
			if v.flags["json"] {
				return 0, a.json(map[string]any{"grade": grade, "rows": rows, "catalog": c.Info})
			}
			for _, r := range rows {
				if v.flags["hide-excluded"] && r.Kind == "excluded" {
					continue
				}
				fmt.Fprintf(a.Out, "%s %-3s %-32s pool=%s %s\n", r.Kind, r.Grade, r.LaunchSpelling(), r.Pool, r.Promotion)
			}
			a.warnStale(ctx)
			return 0, nil
		}
		if v.get("area") != "" {
			return 2, fmt.Errorf("--area requires --recommend")
		}
		if v.flags["json"] || v.flags["raw"] {
			if e = a.json(statusPayload(in)); e != nil {
				return 2, e
			}
		} else {
			present := map[string]bool{}
			for _, o := range in.Observations {
				present[o.ID] = true
				fmt.Fprintf(a.Out, "%s", o.ID)
				for _, b := range o.Buckets {
					if v.get("horizon") != "" && b.Horizon != v.get("horizon") {
						continue
					}
					fmt.Fprintf(a.Out, " %s=%s%%", b.Window, percentText(b.UsedPct))
				}
				if o.Error != "" {
					fmt.Fprint(a.Out, " unmeasurable")
				}
				if credit, ok := in.CreditStatus(o.ID); ok {
					fmt.Fprint(a.Out, " ", creditText(credit))
				}
				fmt.Fprintln(a.Out)
			}
			if !present["anthropic-api"] {
				if credit, ok := in.CreditStatus("anthropic-api"); ok {
					fmt.Fprintf(a.Out, "anthropic-api %s\n", creditText(credit))
				}
			}
		}
		if v.get("watch") == "" {
			a.warnStale(ctx)
			return statusExit(in, v.get("exit-code-on")), nil
		}
		seconds, e := v.float("watch", 60)
		if e != nil || seconds < 1 || seconds > 86400 {
			return 2, fmt.Errorf("watch interval must be at least one second")
		}
		timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return 0, nil
		case <-timer.C:
		}
	}
}
func percentText(p *float64) string {
	if p == nil {
		return "None"
	}
	s := strconv.FormatFloat(*p, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func creditText(c *pl.CreditStatus) string {
	s := fmt.Sprintf("credit=$%.2f/$%.2f expires=%s days_left=%.1f daily_budget=$%.2f", c.BalanceUSD, c.TotalUSD, c.ExpiresOn, c.DaysLeft, c.DailyBudgetUSD)
	if c.RecentDailyUSD != nil {
		s += fmt.Sprintf(" recent_daily=$%.2f", *c.RecentDailyUSD)
	}
	switch {
	case c.Expired:
		s += " EXPIRED"
	case c.Exhausted:
		s += " EXHAUSTED"
	}
	if c.Waste {
		s += " WASTE"
	}
	if c.Stale {
		s += " STALE"
	}
	return s
}
func statusPayload(in pl.Inputs) map[string]any {
	providers := append([]pl.Observation(nil), in.Observations...)
	present := map[string]bool{}
	for _, o := range providers {
		present[o.ID] = true
	}
	for _, pool := range []string{"anthropic-api", "commandcode"} {
		if !present[pool] {
			providers = append(providers, pl.Observation{ID: pool, PoolClass: in.Config.Pool(pool, in.Now).Class, Error: "manual observation unavailable"})
		}
	}
	rows := []map[string]any{}
	for _, observation := range providers {
		o := pl.DisplayObservation(observation, in.Config, in.Manual, in.Now)
		data, _ := json.Marshal(o)
		row := map[string]any{}
		_ = json.Unmarshal(data, &row)
		row["status"] = "ok"
		if o.Error != "" || o.Warning != "" || o.Stale {
			row["status"] = "degraded"
		}
		if credit, ok := in.CreditStatus(observation.ID); ok {
			row["credit"] = credit
			if credit.Expired || credit.Exhausted {
				row["status"] = "exhausted"
			} else if credit.Stale && row["status"] == "ok" {
				row["status"] = "degraded"
			}
		}
		rows = append(rows, row)
	}
	return map[string]any{"schema": "scopefuel.v1", "generated_at": in.Now.Format(time.RFC3339Nano), "providers": rows}
}

func statusExit(in pl.Inputs, on string) int {
	for _, o := range in.Observations {
		if on == "degraded" && o.Error != "" {
			return 1
		}
		for _, b := range o.Buckets {
			if b.UsedPct != nil && ((on == "crit" && *b.UsedPct >= 90) || (on == "warn" && *b.UsedPct >= 75)) {
				return 1
			}
		}
	}
	return 0
}
func (a *App) gate(ctx context.Context, v args) (int, error) {
	if e := v.required("profile"); e != nil {
		return 2, e
	}
	req := request(v)
	c := a.catalog(ctx)
	manual, e := a.localManual()
	if e != nil {
		return 2, e
	}
	in := pl.Inputs{Catalog: c, Config: a.Config, Manual: manual, Now: time.Now().UTC()}
	preliminaryInput := in
	preliminaryInput.Manual = pl.ManualStore{}
	preliminary := pl.Gate(preliminaryInput, req)
	d := preliminary
	if d.ExitCode == pl.Unmeasurable {
		pools := []string{}
		seen := map[string]bool{}
		for _, r := range c.Rows {
			if r.Profile == c.Canonical(req.Profile) || (preliminary.Grade != nil && r.Grade == *preliminary.Grade) {
				if !seen[r.Pool] {
					pools = append(pools, r.Pool)
					seen[r.Pool] = true
				}
			}
		}
		in, e = a.inputs(ctx, c, pools)
		if e != nil {
			return 2, e
		}
		d = pl.Gate(in, req)
	}
	if hold := pl.ActiveLimit("devin", in.Now); hold != nil && strings.HasPrefix(req.Profile, "devin-swe2") {
		d.Reason += "; last observed free_rate_limited until " + hold.ResetAt
	}
	if hold := pl.ActiveLimit("commandcode", in.Now); hold != nil && req.Profile == "cmd-dsflash41" && d.ExitCode != pl.RoleDenied && d.ExitCode != pl.Unsubscribed {
		d.OK = false
		d.ExitCode = pl.Blocked
		d.Unmeasurable = false
		d.Reason = "commandcode usage limit until " + hold.ResetAt
	}
	record := map[string]any{}
	data, _ := json.Marshal(d)
	_ = json.Unmarshal(data, &record)
	record["schema"] = "scopefuel.gate.v1"
	record["generated_at"] = in.Now.Format(time.RFC3339Nano)
	record["purpose"] = nil
	if req.Purpose != "" {
		record["purpose"] = req.Purpose
	}
	record["exhaust_notice"] = nil
	for _, o := range in.Observations {
		if o.ID == d.ProviderID {
			if notice := pl.ExhaustionNotice(ctx, a.Config, o, "", in.Now, nil); notice != "" {
				record["exhaust_notice"] = notice
				d.Reason += " [" + notice + "]"
			}
		}
	}
	if path := v.get("gate-output"); path != "" {
		if e = pl.WriteJSON(path, record); e != nil {
			return 2, fmt.Errorf("cannot write --gate-output")
		}
	}
	if v.flags["json"] {
		return d.ExitCode, a.json(record)
	}
	if d.OK {
		class := "None"
		if d.PoolClass != nil {
			class = *d.PoolClass
		}
		fmt.Fprintf(a.Out, "profile=%s pool=%s used_pct=%s class=%s", d.Profile, d.ProviderID, percentText(d.UsedPct), class)
		if d.GateBasis != "" {
			fmt.Fprintf(a.Out, " basis=%s free_tag=%s account_daily=%s account_weekly=%s", d.GateBasis, d.FreeTag, percentText(d.AccountDailyPct), percentText(d.AccountWeeklyPct))
		}
		if d.E6Arm != "" {
			fmt.Fprintf(a.Out, " [E6 arm %s]", d.E6Arm)
		}
		if d.StaleAccepted {
			fmt.Fprint(a.Out, " stale_accepted=true")
		}
		if d.OperatorRequestRef != nil {
			by := "unknown"
			if d.RequestedBy != nil {
				by = *d.RequestedBy
			}
			fmt.Fprintf(a.Out, " escalation_override=%t operator_request_ref=%s requested_by=%s ref_resolution=unverified", d.EscalationOverride, *d.OperatorRequestRef, by)
		}
		if d.PaceOverrideRef != nil {
			fmt.Fprintf(a.Out, " pace_override_ref=%s", *d.PaceOverrideRef)
		}
		if d.Credit != nil {
			fmt.Fprintf(a.Out, " credit_balance=$%.2f credit_expires=%s credit_days_left=%.1f credit_daily_budget=$%.2f", d.Credit.BalanceUSD, d.Credit.ExpiresOn, d.Credit.DaysLeft, d.Credit.DailyBudgetUSD)
			if d.Credit.Waste {
				fmt.Fprint(a.Out, " credit_waste=true")
			}
			if d.Credit.Stale {
				fmt.Fprint(a.Out, " credit_stale=true")
			}
		}
		if d.Source != nil {
			fmt.Fprintf(a.Out, " source=%s", *d.Source)
			if *d.Source == "operator" {
				fmt.Fprintf(a.Out, " source_verification=unverified source_claim=self_reported_unverified manual_observation_ids=%s", strings.Join(d.ManualObservationIDs, ","))
			}
		}
		fmt.Fprintln(a.Out)
		fmt.Fprintln(a.Out, d.Reason)
	} else {
		fmt.Fprintln(a.Err, d.Reason)
		if d.ExitCode != pl.RoleDenied && d.ExitCode != pl.Unsubscribed {
			fmt.Fprintln(a.Err, "alternatives:", strings.Join(d.Alternatives, ", "))
		}
	}
	a.warnStale(ctx)
	return d.ExitCode, nil
}
func (a *App) herdr(ctx context.Context) (int, error) {
	event, e := pl.ParseHerdrEvent(os.Getenv("HERDR_PLUGIN_EVENT_JSON"))
	if e != nil {
		return 2, e
	}
	if event.PaneID == "" || event.Provider == "" {
		return 0, nil
	}
	base := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if base == "" {
		base = filepath.Join(pl.CacheDir(), "herdr")
	}
	hash := sha256.Sum256([]byte(event.PaneID + "|" + event.Provider + "|" + event.Credential))
	path := filepath.Join(base, hex.EncodeToString(hash[:8])+".json")
	label := ""
	err := pl.WithFileLock(path+".lock", true, func() error {
		var state struct {
			At    string `json:"at"`
			Label string `json:"label"`
		}
		if pl.ReadJSON(path, &state) == nil {
			if at, e := pl.ParseTime(state.At); e == nil && time.Since(at) >= 0 && time.Since(at) < time.Minute {
				label = state.Label
				return nil
			}
		}
		collector := a.Collector
		collector.TTL = time.Minute
		if event.Credential != "default" {
			collector.CachePath = filepath.Join(base, "snapshot-"+event.Provider+"-"+event.Credential+".json")
			collector.LegacyCachePath = ""
			collector.Providers = pl.DefaultProvidersFor(a.Config, event.Selectors)
			collector.Selectors = event.Selectors
			for key, p := range collector.Providers {
				if probe, ok := p.(pl.PTYProvider); ok {
					probe.Guard = pl.GuardProcess
					collector.Providers[key] = probe
				}
			}
		}
		rows, e := collector.Collect(ctx, []string{event.Provider})
		if e != nil {
			return e
		}
		label = pl.HerdrLabel(rows[0], event.Credential)
		state.At = time.Now().UTC().Format(time.RFC3339Nano)
		state.Label = label
		_ = pl.ExhaustionNotice(ctx, a.Config, rows[0], event.Credential, time.Now().UTC(), nil)
		return pl.WriteJSON(path, state)
	})
	if err == pl.ErrBusy {
		return 0, nil
	}
	if err != nil {
		return 2, err
	}
	binary := os.Getenv("HERDR_BIN_PATH")
	if binary == "" {
		binary = "herdr"
	}
	cmd := exec.CommandContext(ctx, binary, "pane", "report-metadata", "--source", "scopefuel.gauge", event.PaneID, "--token", "scopefuel_quota="+label, "--ttl-ms", "60000")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return 0, cmd.Run()
}

const help = `pacelever — measure quota, resolve profiles, admit sustainable work

Usage:
  pacelever [--json|--brief] [--only claude,codex] [--watch SECONDS]
  pacelever gate -m PROFILE [--effort RUNG] [--role SEAT] [--gate-output PATH]
  pacelever policy launch PROFILE [--effort RUNG] [--role SEAT] [--json]
  pacelever policy list|set|clear|profile|setting
  pacelever manual set|list|clear
  pacelever bench catalog list|status
  pacelever bench push-catalog FILE [--dry-run] | --emit-seed
  pacelever bench grades list|set
  pacelever reps add|list|compare|pending
  pacelever grades propose|apply
  pacelever refresh POOL [--background]
  pacelever observe devin-free-limit|commandcode-limit
  pacelever inflight show --pool POOL [--json]
  pacelever herdr-event
  pacelever models list|verify
  pacelever --recommend GRADE [--role SEAT] [--area AREA] [--json]

Gate codes: 0 admit; 2 invalid/I/O; 3 quota/policy; 4 unmeasurable;
5 role-denied; 6 unsubscribed. Pace ranks and warns; it never denies.
Credit profiles require an explicit effort and seat. Placed default rungs
allow builder, worker and tester; other rungs remain worker/tester only.
See docs/config.md for configuration, provider paths and command examples.
`
