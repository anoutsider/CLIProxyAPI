// Command pool_forecast_replay runs the pool forecast over a cswap poll history,
// using at each point only the data before it, and reports how the predictions
// compare with what happened.
package main

import (
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/cmd"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// exhaustedAt is the utilization at which the replay counts an account as out:
// the history was recorded under routing that stopped each account at 90%.
const exhaustedAt = 0.9

func main() {
	path := flag.String("history", "", "cswap poll history JSONL file")
	threshold := flag.Float64("threshold", coreauth.DefaultSoonestResetThreshold, "soonest-reset threshold")
	drain := flag.Duration("drain-window", coreauth.DefaultSoonestResetDrainWindow, "soonest-reset drain window")
	flag.Parse()
	file, errOpen := os.Open(*path)
	if errOpen != nil {
		fmt.Fprintln(os.Stderr, errOpen)
		os.Exit(1)
	}
	samples, errParse := cmd.ParseCswapPollHistory(file, cmd.CswapSlotEmails)
	_ = file.Close()
	if errParse != nil {
		fmt.Fprintln(os.Stderr, errParse)
		os.Exit(1)
	}
	if len(samples) == 0 {
		fmt.Fprintln(os.Stderr, "no samples")
		os.Exit(1)
	}
	r := replay{samples: samples, threshold: *threshold, drain: *drain}
	r.run()
}

type replay struct {
	samples   []coreauth.PoolSample
	threshold float64
	drain     time.Duration
}

// forecastAt forecasts at now from the samples taken before now.
func (r replay) forecastAt(now time.Time) coreauth.PoolForecast {
	end := sort.Search(len(r.samples), func(i int) bool { return !r.samples[i].At.Before(now) })
	history := r.samples[:end]
	return coreauth.BuildPoolForecast(coreauth.PoolForecastInput{
		Now:         now,
		Threshold:   r.threshold,
		DrainWindow: r.drain,
		Accounts:    coreauth.PoolAccountsFromHistory(history, now),
		History:     history,
	})
}

// exhausted reports whether every account's latest reading at now is at
// exhaustedAt or more.
func (r replay) exhausted(now time.Time) bool {
	end := sort.Search(len(r.samples), func(i int) bool { return r.samples[i].At.After(now) })
	accounts := coreauth.PoolAccountsFromHistory(r.samples[:end], now)
	if len(accounts) == 0 {
		return false
	}
	for _, account := range accounts {
		if account.SevenDay.Utilization < exhaustedAt {
			return false
		}
	}
	return true
}

type interval struct{ from, to time.Time }

func (r replay) run() {
	first := r.samples[0].At
	last := r.samples[len(r.samples)-1].At
	fmt.Printf("History: %d samples, %s to %s, threshold %.2f, drain window %s\n\n", len(r.samples), first.Format(time.RFC3339), last.Format(time.RFC3339), r.threshold, r.drain)

	// Actual exhausted periods, at 10-minute resolution.
	var periods []interval
	for at := first; !at.After(last); at = at.Add(coreauth.PoolSampleInterval) {
		if !r.exhausted(at) {
			continue
		}
		if n := len(periods); n > 0 && periods[n-1].to.Equal(at.Add(-coreauth.PoolSampleInterval)) {
			periods[n-1].to = at
		} else {
			periods = append(periods, interval{at, at})
		}
	}
	fmt.Printf("Exhausted periods (every account at %.0f%% or more):\n", exhaustedAt*100)
	for _, p := range periods {
		fmt.Printf("  %s to %s\n", p.from.Format(time.RFC3339), p.to.Add(coreauth.PoolSampleInterval).Format(time.RFC3339))
	}
	if len(periods) == 0 {
		fmt.Println("  none")
	}
	fmt.Println()

	// Daily predictions at 00:00 UTC.
	fmt.Println("Daily forecast at 00:00 UTC (fractions of one account's week):")
	fmt.Println("  day         quiet  typical  busy   actual  in-band  typical runsOutAt     busy runsOutAt")
	var errors, signed, relative []float64
	inBand, days := 0, 0
	for day := first.Truncate(24 * time.Hour).Add(24 * time.Hour); !day.Add(24 * time.Hour).After(last); day = day.Add(24 * time.Hour) {
		forecast := r.forecastAt(day)
		burn := forecast.Pool.BurnPerDay
		if burn.Typical == nil {
			continue
		}
		next := r.forecastAt(day.Add(24 * time.Hour))
		if next.Accuracy == nil || !next.Accuracy.ForecastAt.Equal(day) {
			continue
		}
		actual := next.Accuracy.Actual
		days++
		contained := actual >= *burn.Quiet && actual <= *burn.Busy
		if contained {
			inBand++
		}
		errors = append(errors, math.Abs(*burn.Typical-actual))
		signed = append(signed, *burn.Typical-actual)
		if actual > 0 {
			relative = append(relative, math.Abs(*burn.Typical-actual)/actual)
		}
		fmt.Printf("  %s  %5.2f  %5.2f    %5.2f  %5.2f   %-7v  %-20s  %s\n", day.Format("2006-01-02"), *burn.Quiet, *burn.Typical, *burn.Busy, actual, contained,
			formatTime(forecast.Pool.RunsOutAt.Typical), formatTime(forecast.Pool.RunsOutAt.Busy))
	}
	fmt.Printf("\nBand (quiet to busy) contained the actual burn on %d of %d days (%.0f%%).\n", inBand, days, 100*float64(inBand)/math.Max(1, float64(days)))
	fmt.Printf("Typical prediction: median absolute error %.2f, median signed error (predicted minus actual) %.2f, median relative error %.0f%%.\n\n", median(errors), median(signed), 100*median(relative))

	// Hourly alert evaluation.
	type fired struct {
		key, message string
		at           time.Time
		runsOut      time.Time
	}
	var alerts []fired
	seen := make(map[string]bool)
	for at := first.Truncate(time.Hour).Add(time.Hour); !at.After(last); at = at.Add(time.Hour) {
		forecast := r.forecastAt(at)
		for _, alert := range forecast.Alerts {
			if alert.Kind != coreauth.PoolAlertRunsOut || seen[alert.Key] {
				continue
			}
			seen[alert.Key] = true
			alerts = append(alerts, fired{key: alert.Key, message: alert.Message, at: at, runsOut: *forecast.Pool.RunsOutAt.Typical})
		}
	}
	falseAlerts := 0
	fmt.Println("Runs-out alerts (evaluated hourly; true when an exhausted period starts or lasts within 72 hours of the alert):")
	for _, alert := range alerts {
		hit := time.Time{}
		for _, p := range periods {
			if !p.to.Before(alert.at) && !p.from.After(alert.at.Add(72*time.Hour)) {
				hit = p.from
				break
			}
		}
		verdict := "false"
		if !hit.IsZero() {
			verdict = fmt.Sprintf("true, exhausted from %s", hit.Format(time.RFC3339))
		} else {
			falseAlerts++
		}
		fmt.Printf("  %s fired %s, typical runsOutAt %s: %s\n    %s\n", alert.key, alert.at.Format(time.RFC3339), alert.runsOut.Format(time.RFC3339), verdict, alert.message)
	}
	fmt.Printf("\n%d runs-out alerts, %d false.\n", len(alerts), falseAlerts)
	for _, p := range periods {
		lead := "no alert before it"
		for _, alert := range alerts {
			if !alert.at.After(p.from) && !alert.at.Before(p.from.Add(-72*time.Hour)) {
				lead = fmt.Sprintf("first alert %s, %.0f hours before", alert.at.Format(time.RFC3339), p.from.Sub(alert.at).Hours())
				break
			}
		}
		fmt.Printf("Exhausted from %s: %s.\n", p.from.Format(time.RFC3339), lead)
	}
}

func formatTime(at *time.Time) string {
	if at == nil {
		return "null"
	}
	return at.UTC().Format("2006-01-02T15:04Z")
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	middle := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[middle]
	}
	return (sorted[middle-1] + sorted[middle]) / 2
}
