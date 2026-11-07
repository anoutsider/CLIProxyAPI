package auth

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
)

var poolNow = time.Date(2026, 11, 7, 20, 0, 0, 0, time.UTC)

func poolLimits() resetLimits {
	return (&SoonestResetSelector{}).limits()
}

func TestProjectPoolResetRestoresToZero(t *testing.T) {
	run := projectPool([]poolSlot{{used: 0.5, reset: poolNow.Add(90 * time.Minute)}}, 0, poolLimits(), poolNow)
	if got := run.points[0].Remaining; got != 0.5 {
		t.Fatalf("remaining before the reset = %v, want 0.5", got)
	}
	if got := run.points[1].Remaining; got != 1 {
		t.Fatalf("remaining after the reset = %v, want 1", got)
	}
	if got := run.leftAtReset[0]; got != 0.5 {
		t.Fatalf("leftAtReset = %v, want 0.5", got)
	}
}

func TestProjectPoolMovesResetSevenDays(t *testing.T) {
	// A quarter of a week a day fills the account 4 days after its reset. Its next
	// reset is 7 days after the first, past the projection, so it stays full.
	run := projectPool([]poolSlot{{used: 0.5, reset: poolNow.Add(90 * time.Minute)}}, 0.25, poolLimits(), poolNow)
	if got := run.points[len(run.points)-1].Remaining; got != 0 {
		t.Fatalf("remaining at the end = %v, want 0", got)
	}
	if run.runsOutAt == nil || run.runsOutAt.Sub(poolNow) < 4*24*time.Hour || run.runsOutAt.Sub(poolNow) > 5*24*time.Hour {
		t.Fatalf("runsOutAt = %v, want 4 to 5 days ahead", run.runsOutAt)
	}
}

func TestProjectPoolDrainsAccountToFullBeforeItsReset(t *testing.T) {
	limits := poolLimits()
	slots := []poolSlot{
		{used: 0.9, reset: poolNow.Add(12 * time.Hour)},
		{used: 0.5, reset: poolNow.Add(5 * 24 * time.Hour)},
	}
	// 0.24 a day is 0.01 an hour: the draining account takes it all and is full
	// 10 hours later, before its reset.
	run := projectPool(slots, 0.24, limits, poolNow)
	if got := run.leftAtReset[0]; got > 1e-9 {
		t.Fatalf("draining account left %v at its reset, want 0", got)
	}
	states := poolAccountStates([]PoolAccount{
		{ID: "a", SevenDay: &ScopedWindow{Utilization: 0.9, ResetsAt: slots[0].reset}},
		{ID: "b", SevenDay: &ScopedWindow{Utilization: 0.95, ResetsAt: slots[1].reset}},
		{ID: "c", SevenDay: &ScopedWindow{Utilization: 0.5, ResetsAt: slots[1].reset}},
	}, limits, poolNow)
	want := []string{PoolStateNext, PoolStateAtLimit, PoolStateEligible}
	for i, state := range states {
		if state.view.State != want[i] {
			t.Fatalf("account %d state = %s, want %s", i, state.view.State, want[i])
		}
	}
	states = poolAccountStates([]PoolAccount{
		{ID: "a", SevenDay: &ScopedWindow{Utilization: 0.9, ResetsAt: slots[0].reset}},
		{ID: "c", SevenDay: &ScopedWindow{Utilization: 0.5, ResetsAt: poolNow.Add(6 * time.Hour)}},
	}, limits, poolNow)
	if states[0].view.State != PoolStateDraining || states[1].view.State != PoolStateNext {
		t.Fatalf("states = %s, %s; want draining, next", states[0].view.State, states[1].view.State)
	}
}

func TestProjectPoolRunsOutAtNullWhenCapacityOutlastsSevenDays(t *testing.T) {
	var slots []poolSlot
	for i := 0; i < 7; i++ {
		slots = append(slots, poolSlot{used: 0.2, reset: poolNow.Add(time.Duration(i+1) * 20 * time.Hour)})
	}
	if run := projectPool(slots, 0.9, poolLimits(), poolNow); run.runsOutAt != nil {
		t.Fatalf("runsOutAt = %v at 0.9 a day, want null", run.runsOutAt)
	}
	if run := projectPool(slots, 3, poolLimits(), poolNow); run.runsOutAt == nil {
		t.Fatal("runsOutAt = null at 3 a day, want a time")
	}
}

// poolHistoryFrom returns one account's samples every 10 minutes from start to
// end, burning 0.1 a day with a reset far ahead.
func poolHistoryFrom(start, end time.Time) []PoolSample {
	var samples []PoolSample
	for at := start; !at.After(end); at = at.Add(PoolSampleInterval) {
		samples = append(samples, PoolSample{At: at, Email: "a@example.com", Used: 0.1 * at.Sub(start).Hours() / 24, ResetsAt: end.Add(24 * time.Hour)})
	}
	return samples
}

func TestBuildPoolForecastNeedsThreeFullDays(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	account := PoolAccount{ID: "a", Email: "a@example.com", SevenDay: &ScopedWindow{Utilization: 0.5, ResetsAt: start.Add(5 * 24 * time.Hour)}}
	short := start.Add(3*24*time.Hour - time.Minute)
	forecast := BuildPoolForecast(PoolForecastInput{Now: short, Accounts: []PoolAccount{account}, History: poolHistoryFrom(start, short)})
	burn := forecast.Pool.BurnPerDay
	if burn.Quiet != nil || burn.Typical != nil || burn.Busy != nil {
		t.Fatalf("burnPerDay with 2 full days = %+v, want nulls", burn)
	}
	runsOut := forecast.Pool.RunsOutAt
	if runsOut.Quiet != nil || runsOut.Typical != nil || runsOut.Busy != nil {
		t.Fatalf("runsOutAt with 2 full days = %+v, want nulls", runsOut)
	}
	if forecast.Projection.Typical != nil || forecast.Accounts[0].LeftAtReset != nil {
		t.Fatal("projection or leftAtReset present with 2 full days")
	}
	full := start.Add(3*24*time.Hour + time.Hour)
	forecast = BuildPoolForecast(PoolForecastInput{Now: full, Accounts: []PoolAccount{account}, History: poolHistoryFrom(start, full)})
	if forecast.Pool.BurnPerDay.Typical == nil || len(forecast.Projection.Typical) != poolProjectionHours {
		t.Fatalf("forecast with 3 full days has no typical burn or projection: %+v", forecast.Pool.BurnPerDay)
	}
	if *forecast.Pool.BurnPerDay.Typical != 0.1 {
		t.Fatalf("typical burn = %v, want 0.1", *forecast.Pool.BurnPerDay.Typical)
	}
}

func TestPoolAlertKeysStayTheSameWithinADay(t *testing.T) {
	summary := func(now time.Time, runsOut time.Time) PoolSummary {
		busy := runsOut.Add(-3 * time.Hour)
		return PoolSummary{
			RunsOutAt: PoolTimes{Typical: &runsOut, Busy: &busy},
			NextReset: &PoolNextReset{Email: "a@example.com", At: runsOut.Add(10 * time.Hour), Restores: 0.8},
		}
	}
	morning := time.Date(2026, 11, 8, 6, 0, 0, 0, time.UTC)
	first, ok := poolRunsOutAlert(summary(morning, time.Date(2026, 11, 9, 6, 0, 0, 0, time.UTC)), nil, morning)
	if !ok {
		t.Fatal("no runs-out alert")
	}
	later := morning.Add(5 * time.Hour)
	second, ok := poolRunsOutAlert(summary(later, time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)), nil, later)
	if !ok || second.Key != first.Key || first.Key != "runs-out:2026-11-09" {
		t.Fatalf("runs-out keys = %q, %q; want both runs-out:2026-11-09", first.Key, second.Key)
	}

	auth := &Auth{ID: "claude-a.json", Provider: "claude", RequiresSignIn: true, Metadata: map[string]any{"email": "a@example.com"}}
	var keys []string
	for _, now := range []time.Time{morning, later} {
		accounts := ClaudePoolAccounts([]*Auth{auth}, now)
		forecast := BuildPoolForecast(PoolForecastInput{Now: now, Accounts: accounts})
		if len(forecast.Alerts) != 1 || forecast.Alerts[0].Kind != PoolAlertSignIn {
			t.Fatalf("alerts = %+v, want one sign-in alert", forecast.Alerts)
		}
		keys = append(keys, forecast.Alerts[0].Key)
	}
	if keys[0] != keys[1] {
		t.Fatalf("sign-in keys = %q, %q; want the same", keys[0], keys[1])
	}
}

func TestPoolHistoryRoundTripsThroughKVStore(t *testing.T) {
	store, errOpen := kvstore.OpenBoltStore(filepath.Join(t.TempDir(), "kv.db"))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() { _ = store.Close() })
	history := &PoolHistory{client: func() (kvstore.KVClient, bool, error) { return store, true, nil }}
	ctx := context.Background()
	old := PoolSample{At: poolNow.Add(-PoolHistoryRetention - 48*time.Hour), Email: "a@example.com", Used: 0.1}
	first := PoolSample{At: poolNow.Add(-26 * time.Hour), Email: "a@example.com", Used: 0.2, ResetsAt: poolNow.Add(time.Hour)}
	if errAppend := history.Append(ctx, []PoolSample{old, first}, poolNow); errAppend != nil {
		t.Fatal(errAppend)
	}
	replaced := first
	replaced.Used = 0.3
	second := PoolSample{At: poolNow.Add(-time.Hour), Email: "a@example.com", Used: 0.4, ResetsAt: poolNow.Add(time.Hour)}
	if errAppend := history.Append(ctx, []PoolSample{replaced, second}, poolNow); errAppend != nil {
		t.Fatal(errAppend)
	}
	got, errLoad := history.Load(ctx, poolNow)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(got) != 2 || got[0].Used != 0.3 || got[1].Used != 0.4 {
		t.Fatalf("loaded %+v, want the replaced and the second sample", got)
	}
	ttl, ok, errTTL := store.KVTTL(ctx, poolHistoryKey(second.At))
	if errTTL != nil || !ok || ttl <= PoolHistoryRetention-24*time.Hour || ttl > PoolHistoryRetention+24*time.Hour {
		t.Fatalf("day key ttl = %v (ok %v, err %v), want about %v", ttl, ok, errTTL, PoolHistoryRetention)
	}
}

func TestPoolRunsOutAlertFiresWhenAResetComesFirst(t *testing.T) {
	// a resets first, but its restored quota fills again before b's reset ends
	// the gap, so the typical day runs out after a's reset.
	slots := []poolSlot{
		{used: 0.5, reset: poolNow.Add(6 * time.Hour)},
		{used: 0.95, reset: poolNow.Add(50 * time.Hour)},
		{used: 0.95, reset: poolNow.Add(60 * time.Hour)},
	}
	resets := []PoolNextReset{
		{Email: "a@example.com", At: slots[0].reset},
		{Email: "b@example.com", At: slots[1].reset},
		{Email: "c@example.com", At: slots[2].reset},
	}
	quiet := projectPool(slots, 1.0, poolLimits(), poolNow)
	typical := projectPool(slots, 1.2, poolLimits(), poolNow)
	busy := projectPool(slots, 1.5, poolLimits(), poolNow)
	if typical.runsOutAt == nil || !typical.runsOutAt.After(slots[0].reset) {
		t.Fatalf("typical runsOutAt = %v, want after a's reset at %v", typical.runsOutAt, slots[0].reset)
	}
	pool := PoolSummary{
		RunsOutAt: PoolTimes{Quiet: quiet.runsOutAt, Typical: typical.runsOutAt, Busy: busy.runsOutAt},
		NextReset: &PoolNextReset{Email: "a@example.com", At: slots[0].reset, Restores: 0.8},
	}
	alert, ok := poolRunsOutAlert(pool, resets, poolNow)
	if !ok {
		t.Fatalf("no runs-out alert with typical runsOutAt %v", typical.runsOutAt)
	}
	if want := "The next reset is b@example.com in 50 hours, +100%."; !strings.Contains(alert.Message, want) {
		t.Fatalf("message = %q, want it to contain %q", alert.Message, want)
	}
}
