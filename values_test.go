package pacelever

import (
	"encoding/json"
	"math"
	"net/http"
	"testing"
)

func TestLooseNumberAcceptsOnlyFiniteValues(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want float64
		ok   bool
	}{
		{12.5, 12.5, true},
		{json.Number("3"), 3, true},
		{int64(4), 4, true},
		{7, 7, true},
		{" 0.25 ", 0.25, true},
		{"", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"-inf", 0, false},
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
		{true, 0, false},
		{nil, 0, false},
	} {
		got, ok := looseNumber(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Errorf("looseNumber(%#v) = %v, %t; want %v, %t", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPointerNumbersRefuseNonFinite(t *testing.T) {
	if anyFloat("NaN") != nil || anyFloat("Inf") != nil {
		t.Fatal("anyFloat accepted a non-finite string")
	}
	if p := anyFloat("0"); p == nil || *p != 0 {
		t.Fatal("anyFloat refused a numeric string")
	}
	header := http.Header{}
	header.Set("X-Balance", "Inf")
	if headerFloat(header, "X-Balance") != nil {
		t.Fatal("headerFloat accepted a non-finite value")
	}
	header.Set("X-Balance", " 42 ")
	if p := headerFloat(header, "X-Balance"); p == nil || *p != 42 {
		t.Fatal("headerFloat refused a numeric value")
	}
}

func TestResetAtValueUnits(t *testing.T) {
	want := "2026-01-02T03:04:05Z"
	for _, in := range []any{1767323045.0, json.Number("1767323045000"), "1767323045", int64(1767323045000000), want, "Fri, 02 Jan 2026 03:04:05 GMT"} {
		if got := resetAtValue(in); got != want {
			t.Errorf("resetAtValue(%#v) = %q", in, got)
		}
	}
	for _, in := range []any{"", "NaN", math.NaN(), 0.0, -5.0, "soon"} {
		if got := resetAtValue(in); got != "" {
			t.Errorf("resetAtValue(%#v) = %q, want empty", in, got)
		}
	}
}
