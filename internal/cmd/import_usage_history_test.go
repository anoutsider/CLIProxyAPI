package cmd

import (
	"fmt"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// cswapLines writes one poll line per reading time, each with the slots'
// seven-day percent; a slot left out of a line was not read.
func cswapLines(readings map[time.Time]map[string]float64) string {
	var b strings.Builder
	for at, slots := range readings {
		var windows []string
		for slot, used := range slots {
			windows = append(windows, fmt.Sprintf("%q: {\"7d\": %v}", slot, used))
		}
		fmt.Fprintf(&b, "{\"ts\": %q, \"windowsPct\": {%s}}\n", at.Format(time.RFC3339), strings.Join(windows, ", "))
	}
	return b.String()
}

func TestImportedGapCarriesTheAccountForward(t *testing.T) {
	// Slot 1 has no reading for 2 days from 2026-11-04 00:30, as amrenator@gmail.com
	// had in the cswap history; slot 2 is read throughout.
	start := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	gapFrom, gapTo := time.Date(2026, 11, 4, 0, 30, 0, 0, time.UTC), time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	readings := make(map[time.Time]map[string]float64)
	for at := start; !at.After(end); at = at.Add(5 * time.Minute) {
		slots := map[string]float64{"2": 40}
		if at.Before(gapFrom) || !at.Before(gapTo) {
			slots["1"] = 50
		}
		readings[at] = slots
	}
	samples, errParse := ParseCswapPollHistory(strings.NewReader(cswapLines(readings)), map[string]string{"1": "one@example.com", "2": "two@example.com"})
	if errParse != nil {
		t.Fatal(errParse)
	}
	forecast := coreauth.BuildPoolForecast(coreauth.PoolForecastInput{Now: end, Accounts: coreauth.PoolAccountsFromHistory(samples, end), History: samples})
	for _, point := range forecast.History {
		if point.Remaining != 1.1 {
			t.Fatalf("history at %v = %v, want 1.1 from both accounts", point.At, point.Remaining)
		}
	}
}

func TestImportedReadingBeforeTheDropBelongsToTheEndingWindow(t *testing.T) {
	// The drop is read at 14:12 and placed at 14:00. The 14:08 reading still shows
	// the old value, so it belongs to the window that ends at 14:00.
	reset := time.Date(2026, 11, 4, 14, 0, 0, 0, time.UTC)
	readings := map[time.Time]map[string]float64{
		reset.Add(-10 * time.Minute): {"1": 95},
		reset.Add(8 * time.Minute):   {"1": 95},
		reset.Add(12 * time.Minute):  {"1": 2},
	}
	samples, errParse := ParseCswapPollHistory(strings.NewReader(cswapLines(readings)), map[string]string{"1": "one@example.com"})
	if errParse != nil {
		t.Fatal(errParse)
	}
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	stale := samples[1].SevenDay
	if stale.Used != 0.95 || !stale.ResetsAt.Equal(reset) {
		t.Fatalf("14:08 reading = %+v, want 95%% with reset %v", stale, reset)
	}
	if got := samples[2].SevenDay; got.Used != 0.02 || !got.ResetsAt.Equal(reset.Add(7*24*time.Hour)) {
		t.Fatalf("14:12 reading = %+v, want 2%% with the next reset", got)
	}
}
