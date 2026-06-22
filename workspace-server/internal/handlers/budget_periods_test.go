package handlers

import (
	"reflect"
	"testing"
)

// Pure-logic tests for the multi-period budget SSOT (budget_periods.go). The
// DB-touching helpers (spendByPeriod / recordSpendDelta) are exercised via the
// handler sqlmock tests; here we pin the parsing + the over-budget decision,
// which is where the per-period semantics actually live.

func TestParseBudgetLimits(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[BudgetPeriod]int64
	}{
		{"empty", "", map[BudgetPeriod]int64{}},
		{"empty-object", "{}", map[BudgetPeriod]int64{}},
		{"all-four", `{"hourly":100,"daily":200,"weekly":300,"monthly":400}`,
			map[BudgetPeriod]int64{PeriodHourly: 100, PeriodDaily: 200, PeriodWeekly: 300, PeriodMonthly: 400}},
		{"null-dropped-zero-kept", `{"hourly":null,"daily":0,"weekly":500}`,
			map[BudgetPeriod]int64{PeriodDaily: 0, PeriodWeekly: 500}}, // 0 = block-all, kept
		{"negative-dropped", `{"monthly":-5}`, map[BudgetPeriod]int64{}},
		{"unknown-key-ignored", `{"yearly":999,"daily":10}`, map[BudgetPeriod]int64{PeriodDaily: 10}},
		{"malformed-json", `{not json`, map[BudgetPeriod]int64{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseBudgetLimits([]byte(tc.raw))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseBudgetLimits(%q) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestEncodeBudgetLimits_RoundTrip(t *testing.T) {
	in := map[BudgetPeriod]int64{PeriodHourly: 100, PeriodMonthly: 400}
	enc := encodeBudgetLimits(in)
	got := parseBudgetLimits(enc)
	if !reflect.DeepEqual(got, in) {
		t.Errorf("round-trip: encode→parse = %v, want %v (enc=%s)", got, in, enc)
	}
	// unknown periods dropped; 0 (block-all) kept
	enc2 := encodeBudgetLimits(map[BudgetPeriod]int64{PeriodDaily: 0, "yearly": 9})
	if got := parseBudgetLimits(enc2); !reflect.DeepEqual(got, map[BudgetPeriod]int64{PeriodDaily: 0}) {
		t.Errorf("encode kept 0/dropped unknown: parse(%s) = %v, want {daily:0}", enc2, got)
	}
}

func TestExceededPeriods(t *testing.T) {
	cases := []struct {
		name   string
		limits map[BudgetPeriod]int64
		spend  map[BudgetPeriod]int64
		want   []BudgetPeriod
	}{
		{"no-limits", map[BudgetPeriod]int64{}, map[BudgetPeriod]int64{PeriodHourly: 999}, nil},
		{"zero-limit-blocks-all", map[BudgetPeriod]int64{PeriodHourly: 0}, map[BudgetPeriod]int64{PeriodHourly: 0}, []BudgetPeriod{PeriodHourly}},
		{"under-all", map[BudgetPeriod]int64{PeriodDaily: 100}, map[BudgetPeriod]int64{PeriodDaily: 50}, nil},
		{"at-limit-is-exceeded", map[BudgetPeriod]int64{PeriodDaily: 100}, map[BudgetPeriod]int64{PeriodDaily: 100}, []BudgetPeriod{PeriodDaily}},
		{"over-limit", map[BudgetPeriod]int64{PeriodHourly: 10}, map[BudgetPeriod]int64{PeriodHourly: 11}, []BudgetPeriod{PeriodHourly}},
		{"only-hourly-over", map[BudgetPeriod]int64{PeriodHourly: 10, PeriodMonthly: 1000},
			map[BudgetPeriod]int64{PeriodHourly: 50, PeriodMonthly: 200}, []BudgetPeriod{PeriodHourly}},
		{"multiple-over-in-order", map[BudgetPeriod]int64{PeriodHourly: 10, PeriodWeekly: 100},
			map[BudgetPeriod]int64{PeriodHourly: 99, PeriodWeekly: 100}, []BudgetPeriod{PeriodHourly, PeriodWeekly}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exceededPeriods(tc.limits, tc.spend)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("exceededPeriods(%v,%v) = %v, want %v", tc.limits, tc.spend, got, tc.want)
			}
		})
	}
}

// TestBudgetPeriods_AllReachable guards the SSOT list: every declared period has
// a positive window and a unique name (a typo'd duplicate would silently break
// per-period accounting).
func TestBudgetPeriods_Wellformed(t *testing.T) {
	seen := map[BudgetPeriod]bool{}
	for _, d := range budgetPeriods {
		if d.Window <= 0 {
			t.Errorf("period %s has non-positive window %v", d.Name, d.Window)
		}
		if seen[d.Name] {
			t.Errorf("duplicate period name %s", d.Name)
		}
		seen[d.Name] = true
	}
	for _, p := range []BudgetPeriod{PeriodHourly, PeriodDaily, PeriodWeekly, PeriodMonthly} {
		if !seen[p] {
			t.Errorf("period %s missing from budgetPeriods SSOT list", p)
		}
	}
}
