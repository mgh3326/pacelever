package cli

import (
	"context"
	"encoding/json"
	"fmt"

	pl "github.com/mgh3326/pacelever"
)

func (a *App) aaSync(ctx context.Context, v args) (int, error) {
	client := a.AA
	if client == nil {
		loaded, err := pl.NewAAClient()
		if err != nil {
			return pl.Invalid, err
		}
		client = &loaded
	}
	bench := a.Bench
	if v.get("out") != "" {
		bench.NoCache = true
	}
	c, err := bench.ReadCatalog(ctx)
	if err != nil {
		return pl.Invalid, fmt.Errorf("aa-sync requires a readable catalog")
	}
	for _, warning := range c.Warnings {
		fmt.Fprintln(a.Err, "warning:", warning)
	}
	if v.get("out") != "" && (c.Info.Source != pl.BenchServer || c.Info.Stale) {
		return pl.Invalid, fmt.Errorf("aa-sync --out requires a fresh server catalog")
	}
	models, err := client.Fetch(ctx)
	if err != nil {
		return pl.Invalid, err
	}
	result := pl.AnnotateAA(c.Rows, models, a.now())
	if !v.flags["dry-run"] {
		if err := pl.SaveAAPriors(result.Rows); err != nil {
			return pl.Invalid, fmt.Errorf("cannot save Artificial Analysis priors")
		}
	}
	if path := v.get("out"); path != "" {
		if err := pl.WriteJSON(path, map[string]any{"catalog": pl.CatalogWire(result.Rows)}); err != nil {
			return pl.Invalid, fmt.Errorf("cannot write annotated catalog artifact")
		}
	}
	summary := result.Summary(v.flags["dry-run"])
	if v.flags["json"] {
		fmt.Fprintln(a.Err, summary)
		return 0, a.json(map[string]any{"catalog": pl.CatalogWire(result.Rows), "annotated": result.Annotated, "unmapped": result.Unmapped, "changed": result.Changed, "dry_run": v.flags["dry-run"]})
	}
	for _, r := range result.Rows {
		if r.BenchmarkSource != "" {
			fmt.Fprintf(a.Out, "%s@%s model=%s%s\n", r.Profile, r.Effort, r.ModelID, benchmarkText(r))
		}
	}
	for _, r := range result.Unmapped {
		fmt.Fprintf(a.Out, "unmapped %s@%s model=%s reason=%s\n", r.Profile, r.Effort, r.ModelID, r.Reason)
	}
	fmt.Fprintln(a.Out, summary)
	return 0, nil
}

func benchmarkText(r pl.CatalogRow) string {
	if r.BenchmarkSource == "" && r.BenchmarkAnnotation == "" {
		return ""
	}
	var b pl.BenchmarkAnnotation
	if json.Unmarshal([]byte(r.BenchmarkAnnotation), &b) != nil {
		return fmt.Sprintf(" benchmark_source=%s benchmark_annotation=%q", r.BenchmarkSource, r.BenchmarkAnnotation)
	}
	metric := func(n *float64) string {
		if n == nil {
			return "n/a"
		}
		return fmt.Sprintf("%g", *n)
	}
	return fmt.Sprintf(" benchmark_source=%s aa_name=%q intelligence_index=%s coding_index=%s price_1m_input_tokens=%s price_1m_output_tokens=%s output_tok_s=%s fetched_at=%s", r.BenchmarkSource, b.AAName, metric(b.IntelligenceIndex), metric(b.CodingIndex), metric(b.Price1MInputTokens), metric(b.Price1MOutputTokens), metric(b.OutputTokensPerSecond), b.FetchedAt)
}
