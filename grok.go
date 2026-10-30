package pacelever

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type grokRecord struct {
	Token        string
	RefreshToken string
	Issuer       string
	ClientID     string
	Fingerprint  string
	Expires      time.Time
	HasExpiry    bool
	Created      time.Time
}

// grokTrustedIssuer is the only OIDC issuer a refresh token may be sent to.
// The auth file is writable local data, so an issuer it names is never trusted
// implicitly: a modified file could otherwise aim the refresh token at an
// attacker host. A new xAI issuer is added here deliberately.
const grokTrustedIssuer = "auth.x.ai"

// refreshable is true when the login file holds everything the OIDC token
// endpoint needs. A key without both fields is a setup or API-key login and
// stays unusable once expired. A refresh token is sent only to the trusted
// issuer over HTTPS, never to another host or over plain HTTP.
func (r grokRecord) refreshable() bool {
	if r.RefreshToken == "" || r.Issuer == "" {
		return false
	}
	issuer, e := url.Parse(r.Issuer)
	return e == nil && issuer.Scheme == "https" && strings.EqualFold(issuer.Host, grokTrustedIssuer)
}

func grokAuthPath(home string, selectors map[string]string) string {
	dir := selectors["GROK_HOME"]
	if dir == "" {
		dir = os.Getenv("GROK_HOME")
	}
	if dir == "" {
		dir = filepath.Join(home, ".grok")
	}
	return filepath.Join(dir, "auth.json")
}

func grokRecords(home string, selectors map[string]string) []grokRecord {
	data := readObject(grokAuthPath(home, selectors))
	var out []grokRecord
	add := func(token string, fields map[string]any) {
		token = strings.TrimSpace(token)
		if token == "" {
			return
		}
		for _, existing := range out {
			if existing.Token == token {
				return
			}
		}
		record := grokRecord{
			Token:        token,
			RefreshToken: strings.TrimSpace(str(fields["refresh_token"])),
			Issuer:       strings.TrimRight(strings.TrimSpace(str(fields["oidc_issuer"])), "/"),
			ClientID:     strings.TrimSpace(str(fields["oidc_client_id"])),
		}
		if user := strings.TrimSpace(str(fields["user_id"])); user != "" {
			record.Fingerprint = fingerprint("grok-account", user)
		}
		if expires := parseOptionalTime(str(fields["expires_at"])); !expires.IsZero() {
			record.Expires, record.HasExpiry = expires, true
		}
		record.Created = parseOptionalTime(str(fields["create_time"]))
		out = append(out, record)
	}
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := data[key]
		record := object(value)
		token := strings.TrimSpace(str(record["key"]))
		if token == "" && strings.HasSuffix(key, ".key") {
			token = strings.TrimSpace(str(value))
		}
		add(token, record)
	}
	// A flat $.key shape is the fallback when no nested records exist.
	if len(out) == 0 {
		add(str(data["key"]), data)
	}
	return out
}

func parseOptionalTime(text string) time.Time {
	text = strings.TrimSpace(text)
	if text == "" {
		return time.Time{}
	}
	if at, e := time.Parse(time.RFC3339Nano, text); e == nil {
		return at.UTC()
	}
	if at, e := http.ParseTime(text); e == nil {
		return at.UTC()
	}
	return time.Time{}
}

// selectGrokRecord is deterministic: latest valid expiry, then latest creation,
// then token. An expired record survives only when it can be refreshed, and it
// ranks below every unexpired one, so a live login is never displaced by a
// stale record. A configured account fingerprint must match exactly.
func selectGrokRecord(records []grokRecord, wantFP string, now time.Time) (grokRecord, bool) {
	valid := make([]grokRecord, 0, len(records))
	for _, record := range records {
		if record.HasExpiry && !record.Expires.After(now) && !record.refreshable() {
			continue
		}
		valid = append(valid, record)
	}
	sort.SliceStable(valid, func(i, j int) bool {
		a, b := valid[i], valid[j]
		aExpired := a.HasExpiry && !a.Expires.After(now)
		bExpired := b.HasExpiry && !b.Expires.After(now)
		if aExpired != bExpired {
			return !aExpired
		}
		if a.HasExpiry != b.HasExpiry {
			return a.HasExpiry
		}
		if !a.Expires.Equal(b.Expires) {
			return a.Expires.After(b.Expires)
		}
		if !a.Created.Equal(b.Created) {
			return a.Created.After(b.Created)
		}
		return a.Token < b.Token
	})
	if wantFP != "" {
		for _, record := range valid {
			if record.Fingerprint == wantFP {
				return record, true
			}
		}
		return grokRecord{}, false
	}
	if len(valid) == 0 {
		return grokRecord{}, false
	}
	return valid[0], true
}

// grokRefreshSkew is the window in which a login is treated as due before it
// actually expires (ClaudeBar's `dueWhen.skew`).
const grokRefreshSkew = 300 * time.Second

func fetchGrok(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	return fetchGrokRecord(ctx, grokRecord{Token: token}, client, now)
}

// fetchGrokRecord reads Grok's credits with the login selected from auth.json.
// Its OAuth2 refresh is ported from ClaudeBar (MIT, tddworks/ClaudeBar —
// Resources/Providers/grok.json and Internal/Lookup/OAuth2Refresher.swift): a
// login that has expired or expires within the 300 s skew, or that the API
// refuses, is traded for a new one at the login's own OIDC issuer. The renewed
// token lives in memory only: this never writes the CLI's auth file, so a
// concurrent `grok` process is not raced and a refresh it performs is kept.
func fetchGrokRecord(ctx context.Context, record grokRecord, client HTTPDoer, now func() time.Time) Observation {
	if strings.TrimSpace(record.Token) == "" {
		return Observation{ID: "grok", Error: "credentials unavailable", ErrorKind: ErrKindCredentials}
	}
	refreshed, refreshTried := false, false
	if grokRefreshDue(record, now()) {
		refreshTried = true
		next, failure, ok := grokRefresh(ctx, record, client, now)
		switch {
		case ok:
			record, refreshed = next, true
		case record.Expires.After(now()):
			// Due by the skew but still valid: a token endpoint that cannot
			// answer must not cost a reading the current login can still give.
		default:
			return failure
		}
	}
	o := grokBilling(ctx, record.Token, client, now)
	if o.ErrorKind == ErrKindAuthFailed && !refreshed && !refreshTried && record.refreshable() {
		next, failure, ok := grokRefresh(ctx, record, client, now)
		if !ok {
			return failure
		}
		record = next
		o = grokBilling(ctx, record.Token, client, now)
	}
	if o.Error != "" {
		return o
	}
	// The tier is optional and its own short step: any failure or absence keeps
	// the billing plan and never turns a measured reading into an error.
	if plan := grokSettingsPlan(ctx, record.Token, client); plan != "" {
		o.Plan = plan
	}
	return o
}

// grokRefreshDue mirrors ClaudeBar's `dueWhen` (iso8601, 300 s skew). A login
// with no expiry is not due by itself — `missingIsDue` is false.
func grokRefreshDue(record grokRecord, now time.Time) bool {
	return record.refreshable() && record.HasExpiry && !record.Expires.After(now.Add(grokRefreshSkew))
}

func grokBilling(ctx context.Context, token string, client HTTPDoer, now func() time.Time) Observation {
	resp, e := doJSON(ctx, client, "GET", "https://cli-chat-proxy.grok.com/v1/billing?format=credits", map[string]string{"Authorization": "Bearer " + token, "x-grok-client-mode": "build"}, nil)
	if e != nil {
		return Observation{ID: "grok", Error: "usage request failed", ErrorKind: ErrKindNetwork}
	}
	if resp.Status != 200 {
		return statusObservation("grok", resp.Status, resp.Header, now(), ErrKindAuthFailed, "grok authentication failed; run grok login", 0)
	}
	if resp.ParseError {
		return Observation{ID: "grok", Error: "invalid usage JSON", ErrorKind: ErrKindParse}
	}
	return parseGrok(resp.Body)
}

func grokSettingsPlan(ctx context.Context, token string, client HTTPDoer) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, e := doJSON(ctx, client, "GET", "https://cli-chat-proxy.grok.com/v1/settings", map[string]string{"Authorization": "Bearer " + token, "x-grok-client-mode": "build"}, nil)
	if e != nil || resp.Status != 200 || resp.ParseError {
		return ""
	}
	return grokPlanName(str(resp.Body["subscription_tier_display"]))
}

// grokRefresh trades the refresh token for a new access token (RFC 6749 §6).
// It returns the renewed record in memory; nothing is written back to the
// login file. That is deliberate (#18): writing another program's credential
// file would race a concurrent `grok` process and could drop a refresh it
// performed. The cost is token rotation: a renewed refresh token lives only
// for this process, so when the issuer rotates it the stored one goes stale,
// a later run's refresh is refused, and the user is told to run `grok login`
// again. Failures carry no response body, URL or credential.
func grokRefresh(ctx context.Context, record grokRecord, client HTTPDoer, now func() time.Time) (grokRecord, Observation, bool) {
	fail := func(message, kind string) (grokRecord, Observation, bool) {
		return record, Observation{ID: "grok", Error: message, ErrorKind: kind}, false
	}
	if !record.refreshable() {
		return fail("grok authentication failed; run grok login", ErrKindAuthFailed)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {record.RefreshToken}}
	if record.ClientID != "" {
		form.Set("client_id", record.ClientID)
	}
	resp, e := doJSON(ctx, client, "POST", record.Issuer+"/oauth2/token", map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, []byte(form.Encode()))
	if e != nil {
		return fail("grok token refresh failed", ErrKindNetwork)
	}
	if resp.Status == 400 || resp.Status == 401 {
		return fail("grok authentication failed; run grok login", ErrKindAuthFailed)
	}
	if resp.Status != 200 {
		return fail("grok token refresh failed", ErrKindHTTP)
	}
	token := strings.TrimSpace(str(resp.Body["access_token"]))
	if resp.ParseError || token == "" {
		return fail("grok token refresh returned no access token", ErrKindParse)
	}
	renewed := record
	renewed.Token = token
	// An empty refresh token in the answer never replaces the saved one.
	if next := strings.TrimSpace(str(resp.Body["refresh_token"])); next != "" {
		renewed.RefreshToken = next
	}
	if seconds, ok := looseNumber(resp.Body["expires_in"]); ok && seconds > 0 && seconds <= 10*365*24*3600 {
		renewed.Expires, renewed.HasExpiry = now().Add(time.Duration(seconds*float64(time.Second))), true
	} else {
		// A renewed token with no stated lifetime is used until the API refuses
		// it; keeping the old expiry would make every probe refresh again.
		renewed.Expires, renewed.HasExpiry = time.Time{}, false
	}
	return renewed, Observation{}, true
}

// grokPlanName names the tier the way the dashboard does; an unknown string
// stays as written and an empty one stays empty.
func grokPlanName(raw string) string {
	text := strings.TrimSpace(raw)
	if text == "" {
		return ""
	}
	var compact strings.Builder
	for _, r := range strings.ToLower(text) {
		if r >= 'a' && r <= 'z' {
			compact.WriteRune(r)
		}
	}
	switch compact.String() {
	case "supergrokheavy", "heavy":
		return "SuperGrok Heavy"
	case "supergrok":
		return "SuperGrok"
	}
	return text
}

// grokProductName drops the shared Grok prefix and splits camel case, so
// GrokBuild reads Build and SomeNewProduct reads Some New Product.
func grokProductName(product string) string {
	name := product
	if strings.HasPrefix(name, "Grok") && len(name) > 4 {
		name = name[4:]
	}
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c >= 'A' && c <= 'Z' && i > 0 && name[i-1] >= 'a' && name[i-1] <= 'z' {
			out.WriteByte(' ')
		}
		out.WriteByte(c)
	}
	return out.String()
}

func parseGrok(raw map[string]any) Observation {
	o := Observation{ID: "grok", Source: SourceUsageAPI}
	config := object(raw["config"])
	if len(config) == 0 {
		config = raw
	}
	used, ok := finiteNumber(config["creditUsagePercent"])
	if !ok || !validPct(&used) {
		return Observation{ID: "grok", Error: "usage response has no valid credit percentage", ErrorKind: ErrKindParse}
	}
	period := object(config["currentPeriod"])
	kind := strings.ToUpper(str(period["type"]))
	window, label := "", "usage"
	switch {
	case strings.Contains(kind, "WEEKLY"):
		window, label = "7d", "7d"
	case strings.Contains(kind, "MONTHLY"):
		window, label = "30d", "30d"
	case strings.Contains(kind, "DAILY"):
		window, label = "1d", "1d"
	default:
		return Observation{ID: "grok", Error: "usage response has an unrecognized billing period", ErrorKind: ErrKindParse}
	}
	// The current period's end, or else the billing period's end — never one
	// shape's start with the other's end.
	reset := resetAtValue(period["end"])
	if reset == "" {
		reset = resetAtValue(config["billingPeriodEnd"])
	}
	o.Plan = grokPlanName(str(config["subscriptionTier"]))
	if o.Plan == "" {
		o.Plan = grokPlanName(str(raw["subscriptionTier"]))
	}
	bucket := Bucket{Label: label, Window: window, UsedPct: Pct(used), ResetsAt: reset, Scope: Scope{Kind: ScopeAccount}, Horizon: horizon(window)}
	note := productNote(config["productUsage"])
	// Prepaid balance is money in cents, not a percentage; it rides the account
	// note so every bucket stays percentage-bearing and publishable shared.
	if balance, ok := looseNumber(object(config["prepaidBalance"])["val"]); ok && balance > 0 {
		if note != "" {
			note += "; "
		}
		note += fmt.Sprintf("prepaid balance %.2f USD", balance/100)
	}
	if note != "" {
		bucket.Note = note
	}
	o.Buckets = []Bucket{bucket}
	// A product's own quota is its own bucket: informational for the gate but
	// visible instead of folded into the account note.
	if items, ok := config["productUsage"].([]any); ok {
		for _, item := range items {
			entry := object(item)
			name := grokProductName(strings.TrimSpace(str(entry["product"])))
			share, shareOK := finiteNumber(entry["usagePercent"])
			if name == "" || !shareOK || !validPct(&share) {
				continue
			}
			o.Buckets = append(o.Buckets, modelBucket(name, window, Pct(share), reset))
		}
	}
	// The cap's unit isn't stated (dollars or cents), so it stays a share. It
	// is not the account window, so it carries a model scope and never gates.
	if capacity, ok := finiteNumber(object(config["onDemandCap"])["val"]); ok && capacity > 0 {
		spent, spentOK := finiteNumber(object(config["onDemandUsed"])["val"])
		if !spentOK || spent < 0 {
			spent = 0
		}
		share := 100 * spent / capacity
		locked := share >= 100
		if share < 0 {
			share = 0
		}
		if share > 100 {
			share = 100
		}
		b := modelBucket("On-Demand", window, Pct(share), reset)
		b.Locked = locked
		o.Buckets = append(o.Buckets, b)
	}
	return o
}

func productNote(v any) string {
	items, ok := v.([]any)
	if !ok || len(items) == 0 {
		return ""
	}
	parts := make([]string, 0, len(items))
	for _, item := range items {
		entry := object(item)
		name := str(entry["product"])
		if name == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.1f%%", name, numericValue(entry["usagePercent"])))
	}
	if len(parts) == 0 {
		return ""
	}
	return "products: " + strings.Join(parts, ", ")
}
