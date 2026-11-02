package pacelever

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

const AAAPIKeyName = "ARTIFICIAL_ANALYSIS_API_KEY"
const aaAPIURL = "https://artificialanalysis.ai/api/v2/data/llms/models"
const aaSourcePrefix = "artificialanalysis:v2:"

// BenchmarkAnnotation is a model prior for candidate choice, never fleet grade
// evidence. Catalog wire annotations are JSON text to match handoffkeep's TEXT
// field. Missing measurements remain null rather than becoming zero.
type BenchmarkAnnotation struct {
	IntelligenceIndex     *float64 `json:"intelligence_index"`
	CodingIndex           *float64 `json:"coding_index,omitempty"`
	Price1MInputTokens    *float64 `json:"price_1m_input_tokens"`
	Price1MOutputTokens   *float64 `json:"price_1m_output_tokens"`
	OutputTokensPerSecond *float64 `json:"median_output_tokens_per_second"`
	AAName                string   `json:"aa_name"`
	FetchedAt             string   `json:"fetched_at"`
}

type AAModel struct {
	ID                    string         `json:"id"`
	Name                  string         `json:"name"`
	Slug                  string         `json:"slug"`
	Evaluations           map[string]any `json:"evaluations"`
	Pricing               map[string]any `json:"pricing"`
	OutputTokensPerSecond any            `json:"median_output_tokens_per_second"`
}

type AAClient struct {
	Key    string
	Client HTTPDoer
}

func NewAAClient() (AAClient, error) {
	if key := FirstEnv(AAAPIKeyName); key != "" {
		return AAClient{Key: key}, nil
	}
	data, err := os.ReadFile(filepath.Join(configBase(), "pacelever", "aa.env"))
	if err != nil && !os.IsNotExist(err) {
		return AAClient{}, fmt.Errorf("cannot read %s from aa.env", AAAPIKeyName)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == AAAPIKeyName {
			value = strings.Trim(strings.TrimSpace(value), "\"'")
			if value != "" {
				return AAClient{Key: value}, nil
			}
		}
	}
	return AAClient{}, fmt.Errorf("%s is required", AAAPIKeyName)
}

// Fetch makes exactly one request, with no retries or redirects. Errors suppress
// transport details and response bodies, either of which could echo the key.
func (a AAClient) Fetch(ctx context.Context) ([]AAModel, error) {
	if strings.TrimSpace(a.Key) == "" {
		return nil, fmt.Errorf("%s is required", AAAPIKeyName)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, aaAPIURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid Artificial Analysis request")
	}
	req.Header.Set("x-api-key", a.Key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pacelever")
	client := a.Client
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("artificialanalysis request failed (%s withheld)", AAAPIKeyName)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artificialanalysis HTTP %d (%s and response withheld)", resp.StatusCode, AAAPIKeyName)
	}
	var payload struct {
		Data []AAModel `json:"data"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	dec.UseNumber()
	if dec.Decode(&payload) != nil || len(payload.Data) == 0 {
		return nil, fmt.Errorf("invalid Artificial Analysis model data")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("invalid Artificial Analysis model data")
	}
	for _, model := range payload.Data {
		if model.ID == "" || model.Name == "" {
			return nil, fmt.Errorf("invalid Artificial Analysis model identity")
		}
	}
	return payload.Data, nil
}

type aaRowKey struct{ Profile, ModelID, Effort string }

func aaKey(r CatalogRow) aaRowKey { return aaRowKey{r.Profile, r.ModelID, r.Effort} }

type AAUnmappedRow struct {
	Profile string `json:"profile"`
	ModelID string `json:"model_id"`
	Effort  string `json:"effort"`
	Reason  string `json:"reason"`
}

type AASyncResult struct {
	Rows      []CatalogRow    `json:"-"`
	Annotated int             `json:"annotated"`
	Unmapped  []AAUnmappedRow `json:"unmapped"`
	Changed   int             `json:"changed"`
}

func (r AASyncResult) Summary(dryRun bool) string {
	return fmt.Sprintf("aa-sync annotated=%d unmapped=%d changed=%d dry_run=%t", r.Annotated, len(r.Unmapped), r.Changed, dryRun)
}

// AnnotateAA only edits benchmark fields on a copy. A missing or ambiguous AA
// variant is reported and leaves the catalog row intact.
func AnnotateAA(rows []CatalogRow, models []AAModel, at time.Time) AASyncResult {
	byID := map[string][]AAModel{}
	for _, model := range models {
		byID[model.ID] = append(byID[model.ID], model)
	}
	out := AASyncResult{Rows: slices.Clone(rows), Unmapped: []AAUnmappedRow{}}
	for i, row := range out.Rows {
		id, known := aaMappings[aaKey(row)]
		reason := "no explicit mapping"
		if known {
			reason = "AA variant absent or ambiguous"
		}
		matches := byID[id]
		if !known || len(matches) != 1 {
			out.Unmapped = append(out.Unmapped, AAUnmappedRow{row.Profile, row.ModelID, row.Effort, reason})
			continue
		}
		m := matches[0]
		a := BenchmarkAnnotation{
			IntelligenceIndex:     aaNumber(m.Evaluations["artificial_analysis_intelligence_index"]),
			CodingIndex:           aaNumber(m.Evaluations["artificial_analysis_coding_index"]),
			Price1MInputTokens:    aaNumber(m.Pricing["price_1m_input_tokens"]),
			Price1MOutputTokens:   aaNumber(m.Pricing["price_1m_output_tokens"]),
			OutputTokensPerSecond: aaNumber(m.OutputTokensPerSecond),
			AAName:                m.Name, FetchedAt: at.UTC().Format(time.RFC3339Nano),
		}
		data, _ := json.Marshal(a)
		source := aaSourcePrefix + at.UTC().Format(time.DateOnly)
		// A refreshed timestamp alone is not a changed measurement.
		var old BenchmarkAnnotation
		if json.Unmarshal([]byte(row.BenchmarkAnnotation), &old) != nil {
			old = BenchmarkAnnotation{}
		}
		old.FetchedAt = a.FetchedAt
		if row.BenchmarkSource != source || !reflect.DeepEqual(old, a) {
			out.Changed++
		}
		out.Rows[i].BenchmarkAnnotation, out.Rows[i].BenchmarkSource = string(data), source
		out.Annotated++
	}
	return out
}

func aaNumber(v any) *float64 {
	if n, ok := finiteNumber(v); ok && n >= 0 {
		return &n
	}
	return nil
}

func aaPriorsPath() string { return filepath.Join(dataBase(), "pacelever", "aa-priors.json") }

// SaveAAPriors stores annotations only; a later catalog read still owns grades,
// gates and provenance. Model identity is part of the attachment key.
func SaveAAPriors(rows []CatalogRow) error {
	priors := make([]aaPrior, 0, len(rows))
	for _, r := range rows {
		if strings.HasPrefix(r.BenchmarkSource, aaSourcePrefix) && r.BenchmarkAnnotation != "" {
			priors = append(priors, aaPrior{r.Profile, r.ModelID, r.Effort, r.BenchmarkSource, r.BenchmarkAnnotation})
		}
	}
	return WithFileLock(aaPriorsPath()+".lock", false, func() error { return WriteJSON(aaPriorsPath(), priors) })
}

type aaPrior struct {
	Profile    string `json:"profile"`
	ModelID    string `json:"model_id"`
	Effort     string `json:"effort"`
	Source     string `json:"benchmark_source"`
	Annotation string `json:"benchmark_annotation"`
}

func AttachAAPriors(c *Catalog) error {
	var priors []aaPrior
	if err := ReadJSON(aaPriorsPath(), &priors); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read Artificial Analysis priors")
	}
	byKey := map[aaRowKey]aaPrior{}
	for _, p := range priors {
		byKey[aaRowKey{p.Profile, p.ModelID, p.Effort}] = p
	}
	for i, row := range c.Rows {
		if p, ok := byKey[aaKey(row)]; ok {
			var local, remote BenchmarkAnnotation
			if json.Unmarshal([]byte(p.Annotation), &local) != nil {
				continue
			}
			if json.Unmarshal([]byte(row.BenchmarkAnnotation), &remote) == nil {
				localTime, localErr := ParseTime(local.FetchedAt)
				remoteTime, remoteErr := ParseTime(remote.FetchedAt)
				if localErr != nil || (remoteErr == nil && remoteTime.After(localTime)) {
					continue
				}
			}
			c.Rows[i].BenchmarkSource, c.Rows[i].BenchmarkAnnotation = p.Source, p.Annotation
		}
	}
	return nil
}
