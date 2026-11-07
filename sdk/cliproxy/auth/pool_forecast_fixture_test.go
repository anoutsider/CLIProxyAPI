package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// poolFixtureNow is off the hour, as a live request is.
var poolFixtureNow = time.Date(2026, 11, 7, 20, 17, 33, 0, time.UTC)

// poolFixtureDayFactors scales each UTC day's burn, so the percentiles differ.
var poolFixtureDayFactors = []float64{1.0, 1.35, 0.7, 1.15, 0.85, 1.5, 0.6, 1.1, 0.95}

// poolFixtureAccount is one account of a fixture pool. Its history rises at a
// rate that reaches used at now within the current window, scaled by the day's
// factor, and caps at 100% in earlier windows.
type poolFixtureAccount struct {
	email      string
	used       float64
	fableShare float64
	resetIn    time.Duration
	disabled   bool
	noData     bool
	// leftPoolAgo is how long before now the account left the pool. Its samples
	// from then on carry its disabled or needs-sign-in state.
	leftPoolAgo time.Duration
	refusedAgo  time.Duration
}

func poolFixtureFactor(day time.Time) float64 {
	index := int(day.Unix()/int64(24*time.Hour/time.Second)) % len(poolFixtureDayFactors)
	return poolFixtureDayFactors[index]
}

// poolFixtureLoad is the day-weighted time from a to b, in days.
func poolFixtureLoad(a, b time.Time) float64 {
	total := 0.0
	for day := a.Truncate(24 * time.Hour); day.Before(b); day = day.Add(24 * time.Hour) {
		from, to := day, day.Add(24*time.Hour)
		if from.Before(a) {
			from = a
		}
		if to.After(b) {
			to = b
		}
		if to.After(from) {
			total += poolFixtureFactor(day) * to.Sub(from).Hours() / 24
		}
	}
	return total
}

func (a poolFixtureAccount) reset(now time.Time) time.Time {
	return now.Add(a.resetIn).Truncate(time.Minute)
}

// usedAt is the account's seven-day used at t and the window's reset.
func (a poolFixtureAccount) usedAt(t, now time.Time) (float64, time.Time) {
	reset := a.reset(now)
	start := reset.Add(-poolSevenDays)
	rate := a.used / poolFixtureLoad(start, now)
	for start.After(t) {
		reset, start = start, start.Add(-poolSevenDays)
	}
	used := rate * poolFixtureLoad(start, t)
	if used > 1 {
		used = 1
	}
	return used, reset
}

func (a poolFixtureAccount) poolAccount(now time.Time) PoolAccount {
	account := PoolAccount{ID: "claude-" + a.email + ".json", Email: a.email, Disabled: a.disabled}
	if a.refusedAgo > 0 {
		account.SignInRefusedAt = now.Add(-a.refusedAgo).Truncate(time.Second)
	}
	if a.noData {
		return account
	}
	reset := a.reset(now)
	account.SevenDay = &ScopedWindow{Utilization: a.used, ResetsAt: reset, ObservedAt: now.Add(-2 * time.Minute)}
	account.Models = []ScopedWindow{{Name: "Fable", Utilization: a.used * a.fableShare, ResetsAt: reset}}
	return account
}

func (a poolFixtureAccount) samples(from, now time.Time) []PoolSample {
	if a.noData {
		return nil
	}
	left := now.Add(-a.leftPoolAgo)
	var samples []PoolSample
	var last PoolSample
	for at := from.Truncate(PoolSampleInterval); !at.After(now); at = at.Add(PoolSampleInterval) {
		if a.leftPoolAgo > 0 && !at.Before(left) {
			// Out of the pool, the account serves nothing: its last reading stands.
			sample := last
			sample.At = at
			sample.State = PoolStateNeedsSignIn
			if a.disabled {
				sample.State = PoolStateDisabled
			}
			samples = append(samples, sample)
			continue
		}
		used, reset := a.usedAt(at, now)
		last = PoolSample{
			At:       at,
			Email:    a.email,
			State:    PoolSampleInPool,
			SevenDay: &PoolSampleWindow{Used: used, ResetsAt: reset, ObservedAt: at},
			Fable:    &PoolSampleWindow{Used: used * a.fableShare, ResetsAt: reset, ObservedAt: at},
		}
		samples = append(samples, last)
	}
	return samples
}

func poolFixtureForecast(accounts []poolFixtureAccount, historyFor time.Duration) PoolForecast {
	in := PoolForecastInput{Now: poolFixtureNow}
	for _, account := range accounts {
		in.Accounts = append(in.Accounts, account.poolAccount(poolFixtureNow))
		in.History = append(in.History, account.samples(poolFixtureNow.Add(-historyFor), poolFixtureNow)...)
	}
	return BuildPoolForecast(in)
}

// poolFixtureMixed has one account in each state.
var poolFixtureMixed = []poolFixtureAccount{
	{email: "ana@example.com", used: 0.55, fableShare: 0.6, resetIn: 9 * time.Hour},
	{email: "ben@example.com", used: 0.96, fableShare: 0.5, resetIn: 18 * time.Hour},
	{email: "cal@example.com", used: 0.38, fableShare: 0.7, resetIn: 76 * time.Hour},
	{email: "dee@example.com", used: 0.93, fableShare: 0.4, resetIn: 106 * time.Hour},
	{email: "eli@example.com", used: 0.3, fableShare: 0.5, resetIn: 48 * time.Hour, leftPoolAgo: 20 * time.Hour, refusedAgo: 20 * time.Hour},
	{email: "fay@example.com", noData: true},
	{email: "gus@example.com", used: 0.2, fableShare: 0.5, resetIn: 120 * time.Hour, disabled: true, leftPoolAgo: 5 * 24 * time.Hour},
}

// TestPoolForecastFixtures builds the forecast the hub sends in four cases and
// checks the values the panel keys on. With POOL_FIXTURE_OUT set to a
// directory, it writes each response there as JSON, for the panel's tests.
func TestPoolForecastFixtures(t *testing.T) {
	var full, signIn []poolFixtureAccount
	for _, account := range poolFixtureMixed {
		if account.noData || account.disabled || account.refusedAgo > 0 {
			continue
		}
		account.used = 1
		full = append(full, account)
	}
	for i, account := range poolFixtureMixed {
		if account.noData {
			account = poolFixtureAccount{email: account.email, used: 0.45, fableShare: 0.5, resetIn: 30 * time.Hour}
		}
		account.disabled = false
		account.refusedAgo = time.Duration(18+i%3) * time.Hour
		account.leftPoolAgo = account.refusedAgo
		signIn = append(signIn, account)
	}
	fixtures := []struct {
		name     string
		forecast PoolForecast
		check    func(t *testing.T, forecast PoolForecast)
	}{
		{"pool-forecast-normal.json", poolFixtureForecast(poolFixtureMixed, 23*24*time.Hour), func(t *testing.T, forecast PoolForecast) {
			want := []string{PoolStateNext, PoolStateDraining, PoolStateEligible, PoolStateAtLimit, PoolStateNeedsSignIn, PoolStateNoData, PoolStateDisabled}
			for i, account := range forecast.Accounts {
				if account.State != want[i] {
					t.Fatalf("%s state = %s, want %s", account.Email, account.State, want[i])
				}
			}
			if forecast.Pool.Accounts != 4 || forecast.Pool.BurnPerDay.Typical == nil || forecast.Accuracy == nil {
				t.Fatalf("pool = %+v, accuracy = %+v; want 4 accounts, burn and accuracy", forecast.Pool, forecast.Accuracy)
			}
		}},
		{"pool-forecast-short-history.json", poolFixtureForecast(poolFixtureMixed, 60*time.Hour), func(t *testing.T, forecast PoolForecast) {
			if forecast.Pool.BurnPerDay.Typical != nil || forecast.Pool.RunsOutAt.Typical != nil || forecast.Projection.Typical != nil {
				t.Fatalf("short history has a forecast: %+v", forecast.Pool)
			}
		}},
		{"pool-forecast-all-full.json", poolFixtureForecast(full, 23*24*time.Hour), func(t *testing.T, forecast PoolForecast) {
			if forecast.Pool.Accounts != 4 || forecast.Pool.Remaining != 0 {
				t.Fatalf("pool accounts = %d, remaining = %v; want 4 and 0", forecast.Pool.Accounts, forecast.Pool.Remaining)
			}
		}},
		{"pool-forecast-all-need-sign-in.json", poolFixtureForecast(signIn, 23*24*time.Hour), func(t *testing.T, forecast PoolForecast) {
			if forecast.Pool.Accounts != 0 || forecast.Pool.Remaining != 0 || forecast.Pool.NextReset != nil {
				t.Fatalf("pool = %+v; want 0 accounts, 0 remaining and no next reset", forecast.Pool)
			}
			if len(forecast.Alerts) != len(signIn) {
				t.Fatalf("alerts = %d, want one sign-in alert per account", len(forecast.Alerts))
			}
			last := forecast.History[len(forecast.History)-1]
			if !last.At.Equal(poolFixtureNow.Truncate(time.Hour)) || last.Remaining != 0 {
				t.Fatalf("last history point = %+v, want 0 at the current hour", last)
			}
		}},
	}
	out := os.Getenv("POOL_FIXTURE_OUT")
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			fixture.check(t, fixture.forecast)
			if out == "" {
				return
			}
			body, errMarshal := json.MarshalIndent(fixture.forecast, "", "  ")
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			if errMkdir := os.MkdirAll(out, 0o755); errMkdir != nil {
				t.Fatal(errMkdir)
			}
			if errWrite := os.WriteFile(filepath.Join(out, fixture.name), append(body, '\n'), 0o644); errWrite != nil {
				t.Fatal(errWrite)
			}
		})
	}
}
