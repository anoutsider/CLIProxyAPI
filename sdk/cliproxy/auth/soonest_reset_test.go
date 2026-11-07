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
	SetClaudeUsageWindows(soon.ID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.95, ResetsAt: soonestResetNow.Add(24 * time.Hour)}})
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
	SetClaudeUsageWindows(soon.ID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.99, ResetsAt: soonestResetNow.Add(-time.Hour)}})
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

// One response teaches an account's window, so an account without data is
// tried before the known ones instead of waiting for a poll.
func TestSoonestResetSelectorTriesAccountsWithoutDataFirst(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("b-known", "0.50", 1*24*time.Hour),
		soonestResetAuth("z-unknown", "", 0),
	)
	if got != "z-unknown" {
		t.Fatalf("picked %q, want z-unknown", got)
	}
	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5", soonestResetAuth("only-unknown", "", 0)); got != "only-unknown" {
		t.Fatalf("picked %q, want only-unknown", got)
	}
}

func TestSoonestResetSelectorTreatsPassedResetAsUnknown(t *testing.T) {
	t.Parallel()
	got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5",
		soonestResetAuth("a-current", "0.50", 1*24*time.Hour),
		soonestResetAuth("z-stale", "0.10", -time.Hour),
	)
	if got != "z-stale" {
		t.Fatalf("picked %q, want z-stale: its window has reset, so it is tried for fresh data", got)
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

// After a restart no account has header signals. The usage endpoint's
// account-wide window still ranks them, so routing does not fall back to ID order.
func TestSoonestResetSelectorUsesPolledAccountWindowWithoutHeaders(t *testing.T) {
	early, late := &Auth{ID: "a-late"}, &Auth{ID: "b-early"}
	SetClaudeUsageWindows(early.ID, &ScopedWindow{Utilization: 0.1, ResetsAt: soonestResetNow.Add(5 * 24 * time.Hour)}, nil)
	SetClaudeUsageWindows(late.ID, &ScopedWindow{Utilization: 0.6, ResetsAt: soonestResetNow.Add(1 * 24 * time.Hour)}, nil)
	t.Cleanup(func() { ClearClaudeScopedWindows(early.ID); ClearClaudeScopedWindows(late.ID) })

	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-opus-5-5", early, late); got != "b-early" {
		t.Fatalf("picked %q, want b-early from the polled account window", got)
	}
}

// Accounts with no window data at all, such as Codex accounts, rotate rather
// than all landing on the first account by ID.
func TestSoonestResetSelectorRotatesWhenNoAccountHasData(t *testing.T) {
	t.Parallel()
	selector := &SoonestResetSelector{}
	auths := []*Auth{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	seen := map[string]bool{}
	for i := 0; i < 3; i++ {
		seen[pickSoonest(t, selector, "gpt-6-astra", auths...)] = true
	}
	if len(seen) != 3 {
		t.Fatalf("picked %v over three requests, want all three accounts", seen)
	}
}

func TestSoonestResetSelectorAllFullFallbackCountsModelWindow(t *testing.T) {
	fableFull := soonestResetAuth("a-fable-full", "0.10", 1*24*time.Hour)
	other := soonestResetAuth("b-other", "0.50", 3*24*time.Hour)
	SetClaudeUsageWindows(fableFull.ID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.99, ResetsAt: soonestResetNow.Add(24 * time.Hour)}})
	SetClaudeUsageWindows(other.ID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.91, ResetsAt: soonestResetNow.Add(3 * 24 * time.Hour)}})
	t.Cleanup(func() { ClearClaudeScopedWindows(fableFull.ID); ClearClaudeScopedWindows(other.ID) })

	if got := pickSoonest(t, &SoonestResetSelector{}, "claude-fable-5-1", fableFull, other); got != "b-other" {
		t.Fatalf("picked %q, want b-other: its Fable window has more room", got)
	}
}

func TestRetainClaudeScopedWindowsForgetsRemovedAccounts(t *testing.T) {
	SetClaudeUsageWindows("kept", nil, []ScopedWindow{{Name: "Fable", Utilization: 0.5, ResetsAt: soonestResetNow.Add(time.Hour)}})
	SetClaudeUsageWindows("gone", nil, []ScopedWindow{{Name: "Fable", Utilization: 0.5, ResetsAt: soonestResetNow.Add(time.Hour)}})
	t.Cleanup(func() { ClearClaudeScopedWindows("kept"); ClearClaudeScopedWindows("gone") })
	RetainClaudeScopedWindows(map[string]struct{}{"kept": {}})
	if _, ok := claudeScopedWindowFor("gone", "claude-fable-5-1"); ok {
		t.Fatalf("removed account's window was kept")
	}
	if _, ok := claudeScopedWindowFor("kept", "claude-fable-5-1"); !ok {
		t.Fatalf("kept account's window was dropped")
	}
}
