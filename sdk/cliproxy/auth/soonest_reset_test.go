package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

var soonestResetNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

func soonestResetAuth(id string, used string, resetIn time.Duration) *Auth {
	auth := &Auth{ID: id}
	if used != "" {
		auth.Quota.Signals = map[string]string{
			claudeSevenDayUtilizationSignal: used,
			claudeSevenDayResetSignal:       strconv.FormatInt(soonestResetNow.Add(resetIn).Unix(), 10),
		}
	}
	return auth
}

func pickSoonest(t *testing.T, selector *SoonestResetSelector, model string, auths ...*Auth) string {
	t.Helper()
	selector.now = func() time.Time { return soonestResetNow }
	got, err := selector.Pick(context.Background(), "claude", model, cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatalf("Pick() auth = nil")
	}
	return got.ID
}

// The account values on the box on 2026-11-07: the account at 71% resetting in
// two days goes first, ahead of fresher accounts that reset later.
func TestSoonestResetSelectorPicksSoonestResetWithRoom(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("outarts", "0.0", 6*24*time.Hour),
		soonestResetAuth("slot1", "0.71", 2*24*time.Hour),
		soonestResetAuth("slot4", "0.15", 4*24*time.Hour),
	)
	if got != "slot1" {
		t.Fatalf("picked %q, want slot1", got)
	}
}

func TestSoonestResetSelectorSkipsAccountAtThreshold(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("almost-full", "0.92", 1*24*time.Hour),
		soonestResetAuth("later", "0.20", 3*24*time.Hour),
	)
	if got != "later" {
		t.Fatalf("picked %q, want later", got)
	}
}

func TestSoonestResetSelectorHonoursConfiguredThreshold(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{Threshold: 0.5}, "claude-opus-5-5",
		soonestResetAuth("half", "0.6", 1*24*time.Hour),
		soonestResetAuth("later", "0.1", 3*24*time.Hour),
	)
	if got != "later" {
		t.Fatalf("picked %q, want later", got)
	}
}

func TestSoonestResetSelectorSkipsAccountWhoseModelWindowIsFull(t *testing.T) {
	soon := soonestResetAuth("fable-full", "0.30", 1*24*time.Hour)
	later := soonestResetAuth("fable-room", "0.40", 3*24*time.Hour)
	SetClaudeScopedWindows(soon.ID, []ScopedWindow{{Name: "Fable", Utilization: 0.95, ResetsAt: soonestResetNow.Add(24 * time.Hour)}})
	t.Cleanup(func() { ClearClaudeScopedWindows(soon.ID) })

	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-fable-5-1", soon, later); got != "fable-room" {
		t.Fatalf("Fable request picked %q, want fable-room", got)
	}
	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5", soon, later); got != "fable-full" {
		t.Fatalf("Opus request picked %q, want fable-full: a full Fable window must not bench Opus", got)
	}
}

func TestSoonestResetSelectorIgnoresExpiredModelWindow(t *testing.T) {
	soon := soonestResetAuth("fable-reset", "0.30", 1*24*time.Hour)
	later := soonestResetAuth("other", "0.40", 3*24*time.Hour)
	SetClaudeScopedWindows(soon.ID, []ScopedWindow{{Name: "Fable", Utilization: 0.99, ResetsAt: soonestResetNow.Add(-time.Hour)}})
	t.Cleanup(func() { ClearClaudeScopedWindows(soon.ID) })

	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-fable-5-1", soon, later); got != "fable-reset" {
		t.Fatalf("picked %q, want fable-reset: its Fable window has already reset", got)
	}
}

func TestSoonestResetSelectorBreaksTieByRemainingPerHour(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("a-busy", "0.60", 2*24*time.Hour),
		soonestResetAuth("b-idle", "0.10", 2*24*time.Hour),
	)
	if got != "b-idle" {
		t.Fatalf("picked %q, want b-idle", got)
	}
}

func TestSoonestResetSelectorRanksAccountsWithoutDataLast(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("a-unknown", "", 0),
		soonestResetAuth("b-known", "0.50", 5*24*time.Hour),
	)
	if got != "b-known" {
		t.Fatalf("picked %q, want b-known", got)
	}
	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5", soonestResetAuth("only-unknown", "", 0)); got != "only-unknown" {
		t.Fatalf("picked %q, want only-unknown", got)
	}
}

func TestSoonestResetSelectorTreatsPassedResetAsUnknown(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("a-stale", "0.10", -time.Hour),
		soonestResetAuth("b-current", "0.50", 5*24*time.Hour),
	)
	if got != "b-current" {
		t.Fatalf("picked %q, want b-current", got)
	}
}

func TestSoonestResetSelectorUsesLeastUsedWhenAllAreFull(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("a-fullest", "0.99", 1*24*time.Hour),
		soonestResetAuth("b-least", "0.91", 5*24*time.Hour),
	)
	if got != "b-least" {
		t.Fatalf("picked %q, want b-least", got)
	}
}

func TestParseUtilizationAcceptsFractionAndPercent(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0.71", 0.71, true},
		{"71", 0.71, true},
		{"1", 1, true},
		{"", 0, false},
		{"-1", 0, false},
		{"NaN", 0, false},
	} {
		got, ok := parseUtilization(tt.in)
		if ok != tt.ok || (ok && (got < tt.want-1e-9 || got > tt.want+1e-9)) {
			t.Fatalf("parseUtilization(%q) = %v, %v; want %v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
