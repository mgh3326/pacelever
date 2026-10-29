package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	pl "github.com/mgh3326/pacelever"
)

func TestManualCreditFlags(t *testing.T) {
	isolated(t)
	now := time.Now().UTC()
	expires := now.Add(10 * 24 * time.Hour).Format(time.RFC3339)
	var out, err bytes.Buffer
	code := Run(context.Background(), []string{"manual", "set", "--pool", "anthropic-api", "--credit-balance", "150", "--credit-total", "200", "--expires", expires}, &out, &err, strings.NewReader(""), "dev", "unknown")
	if code != 0 {
		t.Fatalf("manual set %d: %s", code, err.String())
	}
	store, e := pl.LoadManual()
	if e != nil || len(store.History) != 1 {
		t.Fatalf("stored manual entry %v %+v", e, store.History)
	}
	m := store.History[0]
	if m.Credit == nil || m.Credit.RemainingUSD != 150 || m.Credit.TotalUSD != 200 || m.Credit.ExpiresOn != expires || m.UsedPct == nil || *m.UsedPct != 25 {
		t.Fatalf("credit entry %+v", m)
	}
	if m.EnteredAt == "" || m.Reason == "" || len(store.Valid("anthropic-api", now)) != 1 {
		t.Fatalf("credit entry audit fields %+v", m)
	}

	data, _ := json.Marshal(statusPayload(pl.Inputs{Manual: store, Now: now}))
	var payload struct {
		Providers []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Credit *struct {
				BalanceUSD     float64 `json:"balance_usd"`
				DaysLeft       float64 `json:"days_left"`
				DailyBudgetUSD float64 `json:"daily_budget_usd"`
				Waste          bool    `json:"waste"`
				Exhausted      bool    `json:"exhausted"`
				UpdatedAt      string  `json:"updated_at"`
			} `json:"credit"`
		} `json:"providers"`
	}
	if e = json.Unmarshal(data, &payload); e != nil {
		t.Fatal(e)
	}
	for _, p := range payload.Providers {
		if p.ID != "anthropic-api" {
			continue
		}
		if p.Credit == nil || p.Credit.BalanceUSD != 150 || p.Credit.DailyBudgetUSD <= 0 || p.Credit.UpdatedAt == "" {
			t.Fatalf("status credit block %s", data)
		}
		return
	}
	t.Fatalf("credit status missing: %s", data)
}

func TestManualCreditLegacyFlags(t *testing.T) {
	isolated(t)
	expires := time.Now().UTC().Add(5 * 24 * time.Hour).Format("2006-01-02")
	var out, err bytes.Buffer
	code := Run(context.Background(), []string{"manual", "set", "anthropic-api", "--window", "credit", "--remaining", "20", "--total", "50", "--expires-on", expires, "--measured-at", time.Now().UTC().Format(time.RFC3339), "--reason", "console"}, &out, &err, strings.NewReader(""), "dev", "unknown")
	if code != 0 {
		t.Fatalf("legacy credit form %d: %s", code, err.String())
	}
	store, e := pl.LoadManual()
	if e != nil || len(store.History) != 1 || store.History[0].Credit == nil || store.History[0].Credit.ExpiresOn != expires {
		t.Fatalf("legacy credit entry %v %+v", e, store.History)
	}
}
