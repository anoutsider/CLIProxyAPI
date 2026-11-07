// This file implements importing a cswap poll history into the pool forecast history.
package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// CswapSlotEmails maps the account slots of the cswap poll history to account
// emails. Slots not listed are skipped.
var CswapSlotEmails = map[string]string{
	"1":  "amrenator@gmail.com",
	"2":  "amr@digitalbridges.ae",
	"3":  "outarts@gmail.com",
	"4":  "amr.ali@digitalzone.app",
	"5":  "outsidersos23@gmail.com",
	"9":  "outsidersos25@gmail.com",
	"10": "digitalbridgestech@gmail.com",
}

// cswapResetDrop is the fall in seven-day percent between two readings taken as
// a reset, the fall the forecast's burn takes as a reset.
const cswapResetDrop = coreauth.PoolResetDrop * 100

const cswapWeek = 7 * 24 * time.Hour

// cswapResetHourLag is how long after the hour a drop may be first read and
// still be placed at the hour. Resets fall on the hour, and a poll can read the
// old value for a few minutes after one.
const cswapResetHourLag = 15 * time.Minute

// DoImportUsageHistory imports a cswap poll history file into the pool forecast
// history in the configured key-value store. The hub must be stopped first,
// because it holds the store's file lock.
func DoImportUsageHistory(cfg *config.Config, path string) {
	if cfg == nil || cfg.KVStore.Disabled || cfg.Home.Enabled {
		log.Errorf("import-usage-history: needs the local kv-store, which this config disables")
		return
	}
	file, errOpen := os.Open(strings.TrimSpace(path))
	if errOpen != nil {
		log.Errorf("import-usage-history: %v", errOpen)
		return
	}
	defer func() {
		if errClose := file.Close(); errClose != nil {
			log.Errorf("import-usage-history: close: %v", errClose)
		}
	}()
	samples, errParse := ParseCswapPollHistory(file, CswapSlotEmails)
	if errParse != nil {
		log.Errorf("import-usage-history: %v", errParse)
		return
	}
	if errConfigure := kvstore.ConfigureLocal(true, cfg.EffectiveKVStorePath()); errConfigure != nil {
		log.Errorf("import-usage-history: open kv-store (stop the hub first): %v", errConfigure)
		return
	}
	defer func() {
		if errShutdown := kvstore.ShutdownLocal(); errShutdown != nil {
			log.Errorf("import-usage-history: close kv-store: %v", errShutdown)
		}
	}()
	if errAppend := coreauth.DefaultPoolHistory().Append(context.Background(), samples, time.Now()); errAppend != nil {
		log.Errorf("import-usage-history: %v", errAppend)
		return
	}
	fmt.Printf("Imported %d samples into %s\n", len(samples), cfg.EffectiveKVStorePath())
}

type cswapReading struct {
	at    time.Time
	used  float64
	fable *float64
}

// ParseCswapPollHistory reads a cswap poll history: one JSON object per line with
// "ts" and "windowsPct", a map from slot to {"7d": percent, "Fable": percent}. It
// keeps the last reading of each account in each 10-minute interval. The file has
// no reset times, so each account's resets are inferred from drops of more than
// 30 points. Each reading's reset is the reset of the window it was read in: the
// next drop after it, moved back by 7-day steps while still after it, or after
// the last drop, that reset moved on by 7-day steps.
func ParseCswapPollHistory(r io.Reader, slotEmails map[string]string) ([]coreauth.PoolSample, error) {
	readings := make(map[string][]cswapReading)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		raw := strings.TrimSpace(scanner.Text())
		if raw == "" {
			continue
		}
		var entry struct {
			TS         string `json:"ts"`
			WindowsPct map[string]struct {
				SevenDay *float64 `json:"7d"`
				Fable    *float64 `json:"Fable"`
			} `json:"windowsPct"`
		}
		if errDecode := json.Unmarshal([]byte(raw), &entry); errDecode != nil {
			return nil, fmt.Errorf("line %d: %w", line, errDecode)
		}
		at, errTime := time.Parse(time.RFC3339, entry.TS)
		if errTime != nil {
			return nil, fmt.Errorf("line %d: %w", line, errTime)
		}
		for slot, window := range entry.WindowsPct {
			email, ok := slotEmails[slot]
			if !ok || window.SevenDay == nil {
				continue
			}
			reading := cswapReading{at: at.UTC(), used: *window.SevenDay}
			if window.Fable != nil {
				fable := *window.Fable / 100
				reading.fable = &fable
			}
			readings[email] = append(readings[email], reading)
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return nil, errScan
	}
	var samples []coreauth.PoolSample
	for email, series := range readings {
		sort.SliceStable(series, func(i, j int) bool { return series[i].at.Before(series[j].at) })
		resets := inferCswapResets(series)
		byInterval := make(map[time.Time]coreauth.PoolSample)
		for index, reading := range series {
			reset := cswapWindowReset(resets, index, reading.at)
			sample := coreauth.PoolSample{
				At:       reading.at.Truncate(coreauth.PoolSampleInterval),
				Email:    email,
				State:    coreauth.PoolSampleInPool,
				SevenDay: &coreauth.PoolSampleWindow{Used: reading.used / 100, ResetsAt: reset, ObservedAt: reading.at},
			}
			if reading.fable != nil {
				sample.Fable = &coreauth.PoolSampleWindow{Used: *reading.fable, ResetsAt: reset, ObservedAt: reading.at}
			}
			byInterval[sample.At] = sample
		}
		for _, sample := range byInterval {
			samples = append(samples, sample)
		}
	}
	sort.SliceStable(samples, func(i, j int) bool {
		if !samples[i].At.Equal(samples[j].At) {
			return samples[i].At.Before(samples[j].At)
		}
		return samples[i].Email < samples[j].Email
	})
	return samples, nil
}

// cswapReset is an inferred reset: its time, and the index of the first reading
// after the drop.
type cswapReset struct {
	at    time.Time
	after int
}

// inferCswapResets returns the resets of one account, oldest first. A drop
// between two readings is placed at a 7-day step from the previous reset when one
// falls between them, otherwise at the hour of the later reading when it came
// within cswapResetHourLag of the hour, and else at the later reading.
func inferCswapResets(series []cswapReading) []cswapReset {
	var resets []cswapReset
	for i := 1; i < len(series); i++ {
		prev, next := series[i-1], series[i]
		if prev.used-next.used <= cswapResetDrop {
			continue
		}
		reset := time.Time{}
		if len(resets) > 0 {
			for step := resets[len(resets)-1].at.Add(cswapWeek); !step.After(next.at); step = step.Add(cswapWeek) {
				if step.After(prev.at) {
					reset = step
					break
				}
			}
		}
		if reset.IsZero() {
			reset = next.at.Truncate(time.Hour)
			if next.at.Sub(reset) > cswapResetHourLag {
				reset = next.at.Truncate(time.Minute)
			}
		}
		resets = append(resets, cswapReset{at: reset, after: i})
	}
	return resets
}

// cswapWindowReset is the reset of the window that reading index, taken at at,
// was read in. A reading before a drop belongs to the window that drop ends,
// also when the reset is placed at the hour just before the reading, because a
// poll reads the old value for a few minutes after a reset.
func cswapWindowReset(resets []cswapReset, index int, at time.Time) time.Time {
	if len(resets) == 0 {
		return time.Time{}
	}
	next := sort.Search(len(resets), func(i int) bool { return resets[i].after > index })
	if next < len(resets) {
		reset := resets[next].at
		for reset.Add(-cswapWeek).After(at) {
			reset = reset.Add(-cswapWeek)
		}
		return reset
	}
	reset := resets[len(resets)-1].at
	for !reset.After(at) {
		reset = reset.Add(cswapWeek)
	}
	return reset
}
