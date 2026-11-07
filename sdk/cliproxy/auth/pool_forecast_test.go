package auth

import (
	"context"
	"path/filepath"
	"strconv"
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

// poolInPoolSample is an in-pool sample whose seven-day window was read at at.
func poolInPoolSample(at time.Time, email string, used float64, reset time.Time) PoolSample {
	return PoolSample{At: at, Email: email, State: PoolSampleInPool, SevenDay: &PoolSampleWindow{Used: used, ResetsAt: reset, ObservedAt: at}}
}

// poolHistoryFrom returns one account's samples every 10 minutes from start to
// end, burning 0.1 a day with a reset far ahead.
func poolHistoryFrom(start, end time.Time) []PoolSample {
	var samples []PoolSample
	for at := start; !at.After(end); at = at.Add(PoolSampleInterval) {
		samples = append(samples, poolInPoolSample(at, "a@example.com", 0.1*at.Sub(start).Hours()/24, end.Add(24*time.Hour)))
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
	old := poolInPoolSample(poolNow.Add(-PoolHistoryRetention-48*time.Hour), "a@example.com", 0.1, time.Time{})
	first := poolInPoolSample(poolNow.Add(-26*time.Hour), "a@example.com", 0.2, poolNow.Add(time.Hour))
	if errAppend := history.Append(ctx, []PoolSample{old, first}, poolNow); errAppend != nil {
		t.Fatal(errAppend)
	}
	replaced := poolInPoolSample(first.At, first.Email, 0.3, first.SevenDay.ResetsAt)
	second := poolInPoolSample(poolNow.Add(-time.Hour), "a@example.com", 0.4, poolNow.Add(time.Hour))
	if errAppend := history.Append(ctx, []PoolSample{replaced, second}, poolNow); errAppend != nil {
		t.Fatal(errAppend)
	}
	got, errLoad := history.Load(ctx, poolNow)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if len(got) != 2 || got[0].SevenDay.Used != 0.3 || got[1].SevenDay.Used != 0.4 || got[1].State != PoolSampleInPool || !got[1].SevenDay.ObservedAt.Equal(second.At) {
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

func TestPoolDailyBurnsLeaveOutSamplingGaps(t *testing.T) {
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC).Add(-PoolSampleInterval)
	now := time.Date(2026, 11, 6, 18, 0, 0, 0, time.UTC)
	outageFrom := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	outageTo := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	var history []PoolSample
	for _, sample := range poolHistoryFrom(start, now) {
		if sample.At.After(outageFrom) && sample.At.Before(outageTo) {
			continue
		}
		history = append(history, sample)
	}
	account := PoolAccount{ID: "a", Email: "a@example.com", SevenDay: &ScopedWindow{Utilization: 0.5, ResetsAt: now.Add(5 * 24 * time.Hour)}}
	forecast := BuildPoolForecast(PoolForecastInput{Now: now, Accounts: []PoolAccount{account}, History: history})
	burn := forecast.Pool.BurnPerDay
	if burn.Quiet == nil || *burn.Quiet != 0.1 || *burn.Typical != 0.1 || *burn.Busy != 0.1 {
		t.Fatalf("burnPerDay = %v, %v, %v; want 0.1 for each kind from the 3 days without a gap", *burn.Quiet, *burn.Typical, *burn.Busy)
	}
	if forecast.Accuracy != nil {
		t.Fatalf("accuracy = %+v for a day without samples, want null", forecast.Accuracy)
	}
}

func TestClaudePoolSamplesSumTheAccountsInThePool(t *testing.T) {
	auth := func(email string, used float64, reset time.Time) *Auth {
		return &Auth{
			ID:       "claude-" + email + ".json",
			Provider: "claude",
			Metadata: map[string]any{"email": email},
			Quota: QuotaState{Signals: map[string]string{
				claudeSevenDayUtilizationSignal: strconv.FormatFloat(used, 'f', -1, 64),
				claudeSevenDayResetSignal:       strconv.FormatInt(reset.Unix(), 10),
			}},
		}
	}
	serving := auth("serving@example.com", 0.4, poolNow.Add(48*time.Hour))
	disabled := auth("disabled@example.com", 0.2, poolNow.Add(48*time.Hour))
	disabled.Disabled = true
	signIn := auth("sign-in@example.com", 0.2, poolNow.Add(48*time.Hour))
	signIn.RequiresSignIn = true
	unseen := &Auth{ID: "claude-unseen@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "unseen@example.com"}}
	auths := []*Auth{serving, disabled, signIn, unseen}

	samples := ClaudePoolSamples(auths, poolNow)
	states := map[string]string{}
	for _, sample := range samples {
		states[sample.Email] = sample.State
	}
	want := map[string]string{"serving@example.com": PoolSampleInPool, "disabled@example.com": PoolStateDisabled, "sign-in@example.com": PoolStateNeedsSignIn}
	if len(states) != len(want) {
		t.Fatalf("sample states = %v, want %v and no sample for the unseen account", states, want)
	}
	for email, state := range want {
		if states[email] != state {
			t.Fatalf("sample states = %v, want %v", states, want)
		}
	}
	forecast := BuildPoolForecast(PoolForecastInput{Now: poolNow, Accounts: ClaudePoolAccounts(auths, poolNow), History: samples})
	last := forecast.History[len(forecast.History)-1]
	if last.Remaining != forecast.Pool.Remaining {
		t.Fatalf("history remaining at now = %v, pool remaining = %v; want the same", last.Remaining, forecast.Pool.Remaining)
	}
}

func TestPoolCountsAPassedResetAsReset(t *testing.T) {
	reset := poolNow.Add(-time.Hour)
	observed := poolNow.Add(-8 * time.Hour)
	passed := &Auth{
		ID:       "claude-passed@example.com.json",
		Provider: "claude",
		Metadata: map[string]any{"email": "passed@example.com"},
		Quota: QuotaState{ObservedAt: observed, Signals: map[string]string{
			claudeSevenDayUtilizationSignal: "0.8",
			claudeSevenDayResetSignal:       strconv.FormatInt(reset.Unix(), 10),
		}},
	}
	SetClaudeUsageWindows(passed.ID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.6, ResetsAt: reset.Add(-time.Hour), ObservedAt: observed}})
	t.Cleanup(func() { ClearClaudeScopedWindows(passed.ID) })
	unseen := &Auth{ID: "claude-unseen@example.com.json", Provider: "claude", Metadata: map[string]any{"email": "unseen@example.com"}}
	auths := []*Auth{passed, unseen}
	nextReset := reset.Add(poolSevenDays)

	samples := ClaudePoolSamples(auths, poolNow)
	if len(samples) != 1 || samples[0].Email != "passed@example.com" || samples[0].State != PoolSampleInPool || samples[0].SevenDay.Used != 0.8 || !samples[0].SevenDay.ResetsAt.Equal(reset) {
		t.Fatalf("samples = %+v, want passed@example.com in the pool as observed: used 0.8, reset %v", samples, reset)
	}
	if samples[0].Fable == nil || samples[0].Fable.Used != 0.6 {
		t.Fatalf("sample Fable = %+v, want 0.6 as observed", samples[0].Fable)
	}

	forecast := BuildPoolForecast(PoolForecastInput{Now: poolNow, Accounts: ClaudePoolAccounts(auths, poolNow), History: samples})
	if last := forecast.History[len(forecast.History)-1]; last.Remaining != 1 || last.FableRemaining != 1 {
		t.Fatalf("history at now = %+v, want remaining 1 and fable 1 after the passed resets", last)
	}
	states := map[string]PoolForecastAccount{}
	for _, account := range forecast.Accounts {
		states[account.Email] = account
	}
	if got := states["passed@example.com"]; got.State != PoolStateNext || got.SevenDay == nil || got.SevenDay.Used != 0 || !got.SevenDay.ResetsAt.Equal(nextReset) {
		t.Fatalf("passed account = %+v, want next with used 0 and reset %v", got, nextReset)
	}
	if got := states["unseen@example.com"].State; got != PoolStateNoData {
		t.Fatalf("unseen account state = %s, want no-data", got)
	}
	if forecast.Pool.Accounts != 1 || forecast.Pool.Remaining != 1 || forecast.Pool.FableRemaining != 1 {
		t.Fatalf("pool = %d accounts, remaining %v, fable %v; want 1, 1, 1", forecast.Pool.Accounts, forecast.Pool.Remaining, forecast.Pool.FableRemaining)
	}
	if forecast.Pool.NextReset == nil || !forecast.Pool.NextReset.At.Equal(nextReset) {
		t.Fatalf("nextReset = %+v, want %v", forecast.Pool.NextReset, nextReset)
	}
}

func TestPoolAccountsFromHistoryKeepAnAccountWithoutRecentSamples(t *testing.T) {
	history := []PoolSample{
		poolInPoolSample(poolNow.Add(-30*time.Hour), "passed@example.com", 0.7, poolNow.Add(-2*time.Hour)),
		poolInPoolSample(poolNow.Add(-8*time.Hour), "quiet@example.com", 0.1, poolNow.Add(48*time.Hour)),
		poolInPoolSample(poolNow.Add(-10*time.Minute), "fresh@example.com", 0.4, poolNow.Add(24*time.Hour)),
	}
	accounts := PoolAccountsFromHistory(history, poolNow)
	if len(accounts) != 3 {
		t.Fatalf("accounts = %+v, want 3", accounts)
	}
	want := map[string]struct {
		used  float64
		reset time.Time
	}{
		"passed@example.com": {0, poolNow.Add(-2*time.Hour + poolSevenDays)},
		"quiet@example.com":  {0.1, poolNow.Add(48 * time.Hour)},
		"fresh@example.com":  {0.4, poolNow.Add(24 * time.Hour)},
	}
	for _, account := range accounts {
		w := want[account.Email]
		if account.SevenDay.Utilization != w.used || !account.SevenDay.ResetsAt.Equal(w.reset) {
			t.Fatalf("%s = used %v, reset %v; want %v, %v", account.Email, account.SevenDay.Utilization, account.SevenDay.ResetsAt, w.used, w.reset)
		}
	}
}

func TestPoolTotalsClampUsedToOne(t *testing.T) {
	reset := poolNow.Add(48 * time.Hour)
	fable := 1.2
	forecast := BuildPoolForecast(PoolForecastInput{
		Now: poolNow,
		Accounts: []PoolAccount{{
			ID:       "a",
			Email:    "a@example.com",
			SevenDay: &ScopedWindow{Utilization: 1.05, ResetsAt: reset},
			Models:   []ScopedWindow{{Name: "Fable", Utilization: fable, ResetsAt: reset}},
		}},
		History: []PoolSample{{
			At:       poolNow.Add(-10 * time.Minute),
			Email:    "a@example.com",
			State:    PoolSampleInPool,
			SevenDay: &PoolSampleWindow{Used: 1.05, ResetsAt: reset},
			Fable:    &PoolSampleWindow{Used: fable, ResetsAt: reset},
		}},
	})
	if forecast.Pool.Remaining != 0 || forecast.Pool.FableRemaining != 0 {
		t.Fatalf("pool remaining %v, fable %v; want 0, 0", forecast.Pool.Remaining, forecast.Pool.FableRemaining)
	}
	last := forecast.History[len(forecast.History)-1]
	if last.Remaining != 0 || last.FableRemaining != 0 {
		t.Fatalf("history remaining %v, fable %v; want 0, 0", last.Remaining, last.FableRemaining)
	}
}

// poolHeaderAuth is a Claude account whose seven-day window was last read from
// the response headers at observed.
func poolHeaderAuth(email string, used float64, reset, observed time.Time) *Auth {
	auth := &Auth{ID: "claude-" + email + ".json", Provider: "claude", Metadata: map[string]any{"email": email}}
	poolSetHeaderWindow(auth, used, reset, observed)
	return auth
}

func poolSetHeaderWindow(auth *Auth, used float64, reset, observed time.Time) {
	auth.Quota = QuotaState{ObservedAt: observed, Signals: map[string]string{
		claudeSevenDayUtilizationSignal: strconv.FormatFloat(used, 'f', -1, 64),
		claudeSevenDayResetSignal:       strconv.FormatInt(reset.Unix(), 10),
	}}
}

func TestPoolReadingBurn(t *testing.T) {
	ahead := poolNow.Add(48 * time.Hour)
	passed := poolNow.Add(-time.Hour)
	cases := []struct {
		name       string
		prev, next PoolSampleWindow
		want       float64
	}{
		{"a rise is its increase", PoolSampleWindow{Used: 0.4, ResetsAt: ahead}, PoolSampleWindow{Used: 0.45, ResetsAt: ahead}, 0.05},
		{"a small fall before the reset subtracts", PoolSampleWindow{Used: 0.91, ResetsAt: ahead}, PoolSampleWindow{Used: 0.9, ResetsAt: ahead}, -0.01},
		{"a fall of more than 0.3 is a reset", PoolSampleWindow{Used: 0.95, ResetsAt: ahead}, PoolSampleWindow{Used: 0.04, ResetsAt: ahead}, 0.04},
		{"a small fall after the reset is a reset", PoolSampleWindow{Used: 0.2, ResetsAt: passed}, PoolSampleWindow{Used: 0.02, ResetsAt: ahead}, 0.02},
		{"a rise after a moved reset is its increase", PoolSampleWindow{Used: 0.8, ResetsAt: passed}, PoolSampleWindow{Used: 0.85, ResetsAt: ahead}, 0.05},
	}
	for _, c := range cases {
		c.next.ObservedAt = poolNow
		if got := poolReadingBurn(c.prev, c.next, poolNow); round4(got) != c.want {
			t.Errorf("%s: burn = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPoolBurnCountsAMovedResetAsAnIncrease(t *testing.T) {
	// The hub reads 0.8 with a reset at 16:00. No reading comes until 17:00, when
	// Anthropic reports the same 0.8 with the reset moved 3 days later; then the
	// account burns 0.05.
	start := poolNow.Add(-6 * time.Hour)
	oldReset := poolNow.Add(-4 * time.Hour)
	moved := poolNow.Add(-3 * time.Hour)
	newReset := poolNow.Add(72 * time.Hour)
	auth := poolHeaderAuth("moved@example.com", 0.8, oldReset, start)
	var history []PoolSample
	for at := start; !at.After(poolNow); at = at.Add(PoolSampleInterval) {
		if !at.Before(moved) {
			poolSetHeaderWindow(auth, 0.8+0.05*at.Sub(moved).Hours()/3, newReset, at)
		}
		history = append(history, ClaudePoolSamples([]*Auth{auth}, at)...)
	}
	forecast := BuildPoolForecast(PoolForecastInput{Now: poolNow, Accounts: ClaudePoolAccounts([]*Auth{auth}, poolNow), History: history})
	if got := forecast.Pool.BurnPerDay.Last24h; got != 0.05 {
		t.Fatalf("last 24h burn = %v, want 0.05 from the rise after the moved reset", got)
	}
}

// poolHistoryRemainingAt returns the history point at at.
func poolHistoryRemainingAt(t *testing.T, forecast PoolForecast, at time.Time) float64 {
	t.Helper()
	for _, point := range forecast.History {
		if point.At.Equal(at) {
			return point.Remaining
		}
	}
	t.Fatalf("no history point at %v", at)
	return 0
}

func TestPoolHistoryCarriesAccountsAcrossAHubOutage(t *testing.T) {
	// The hub is down from 30 to 10 hours before now. a's reset falls inside the
	// outage; its first reading after it is 0.05.
	start := poolNow.Add(-48 * time.Hour)
	downFrom, downTo := poolNow.Add(-30*time.Hour), poolNow.Add(-10*time.Hour)
	aReset := poolNow.Add(-20 * time.Hour)
	bReset := poolNow.Add(72 * time.Hour)
	var history []PoolSample
	for at := start; !at.After(poolNow); at = at.Add(PoolSampleInterval) {
		if at.After(downFrom) && at.Before(downTo) {
			continue
		}
		if at.Before(aReset) {
			history = append(history, poolInPoolSample(at, "a@example.com", 0.4, aReset))
		} else {
			history = append(history, poolInPoolSample(at, "a@example.com", 0.05, aReset.Add(poolSevenDays)))
		}
		history = append(history, poolInPoolSample(at, "b@example.com", 0.5, bReset))
	}
	forecast := BuildPoolForecast(PoolForecastInput{Now: poolNow, Accounts: PoolAccountsFromHistory(history, poolNow), History: history})
	if len(forecast.History) != 49 {
		t.Fatalf("history has %d points, want 49 hourly points from the first sample", len(forecast.History))
	}
	for _, c := range []struct {
		at   time.Time
		want float64
	}{
		{downFrom.Add(time.Hour), 1.1},
		{aReset.Add(-time.Hour), 1.1},
		{aReset, 1.5},
		{downTo.Add(-time.Hour), 1.5},
		{downTo, 1.45},
	} {
		if got := poolHistoryRemainingAt(t, forecast, c.at); got != c.want {
			t.Fatalf("history at %v = %v, want %v", c.at, got, c.want)
		}
	}
}

func TestPoolHistoryDropsAnAccountThatNeedsSignIn(t *testing.T) {
	start := poolNow.Add(-6 * time.Hour)
	refused := poolNow.Add(-3 * time.Hour)
	reset := poolNow.Add(48 * time.Hour)
	serving := poolHeaderAuth("serving@example.com", 0.4, reset, start)
	leaving := poolHeaderAuth("leaving@example.com", 0.2, reset, start)
	auths := []*Auth{serving, leaving}
	var history []PoolSample
	for at := start; !at.After(poolNow); at = at.Add(PoolSampleInterval) {
		if !at.Before(refused) {
			leaving.RequiresSignIn = true
			leaving.RenewalEventAt = refused
		}
		history = append(history, ClaudePoolSamples(auths, at)...)
	}
	forecast := BuildPoolForecast(PoolForecastInput{Now: poolNow, Accounts: ClaudePoolAccounts(auths, poolNow), History: history})
	if got := poolHistoryRemainingAt(t, forecast, refused.Add(-time.Hour)); got != 1.4 {
		t.Fatalf("history before the refusal = %v, want 1.4 from both accounts", got)
	}
	for _, at := range []time.Time{refused, poolNow} {
		if got := poolHistoryRemainingAt(t, forecast, at); got != 0.6 {
			t.Fatalf("history at %v = %v, want 0.6 from the serving account only", at, got)
		}
	}
	if forecast.Pool.Remaining != 0.6 {
		t.Fatalf("pool remaining = %v, want 0.6", forecast.Pool.Remaining)
	}
}

func TestPoolAccountsFromHistoryKeepTheSampleState(t *testing.T) {
	reset := poolNow.Add(48 * time.Hour)
	var history []PoolSample
	for i, at := range []time.Time{poolNow.Add(-3 * time.Hour), poolNow.Add(-2 * time.Hour), poolNow.Add(-time.Hour)} {
		disabled := poolInPoolSample(at, "disabled@example.com", 0.2, reset)
		refused := poolInPoolSample(at, "refused@example.com", 0.3, reset)
		if i > 0 {
			disabled.State = PoolStateDisabled
			refused.State = PoolStateNeedsSignIn
		}
		history = append(history, disabled, refused)
	}
	accounts := PoolAccountsFromHistory(history, poolNow)
	if len(accounts) != 2 || !accounts[0].Disabled || accounts[1].Disabled {
		t.Fatalf("accounts = %+v, want disabled@example.com disabled", accounts)
	}
	if want := poolNow.Add(-2 * time.Hour); !accounts[1].SignInRefusedAt.Equal(want) {
		t.Fatalf("refused@example.com refused at %v, want %v, its first needs-sign-in sample", accounts[1].SignInRefusedAt, want)
	}
}
