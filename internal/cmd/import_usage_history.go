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

// cswapResetDrop is the fall in seven-day percent between two readings taken as a reset.
const cswapResetDrop = 30

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
// 30 points, with 7-day steps from the nearest one.
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
		byInterval := make(map[time.Time]cswapReading)
		for _, reading := range series {
			byInterval[reading.at.Truncate(coreauth.PoolSampleInterval)] = reading
		}
		for at, reading := range byInterval {
			samples = append(samples, coreauth.PoolSample{
				At:        at,
				Email:     email,
				Used:      reading.used / 100,
				ResetsAt:  nextCswapReset(resets, reading.at),
				FableUsed: reading.fable,
			})
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

// inferCswapResets returns the reset times of one account, oldest first. A drop
// between two readings is placed at a 7-day step from the previous reset when one
// falls between them, otherwise at the hour of the later reading when it came
// within cswapResetHourLag of the hour, and else at the later reading.
func inferCswapResets(series []cswapReading) []time.Time {
	var resets []time.Time
	for i := 1; i < len(series); i++ {
		prev, next := series[i-1], series[i]
		if prev.used-next.used <= cswapResetDrop {
			continue
		}
		reset := time.Time{}
		if len(resets) > 0 {
			for step := resets[len(resets)-1].Add(cswapWeek); !step.After(next.at); step = step.Add(cswapWeek) {
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
		resets = append(resets, reset)
	}
	return resets
}

// nextCswapReset is the first reset after at: the next inferred reset moved back
// by 7-day steps while still after at, or the last one moved forward.
func nextCswapReset(resets []time.Time, at time.Time) time.Time {
	if len(resets) == 0 {
		return time.Time{}
	}
	index := sort.Search(len(resets), func(i int) bool { return resets[i].After(at) })
	if index < len(resets) {
		reset := resets[index]
		for reset.Add(-cswapWeek).After(at) {
			reset = reset.Add(-cswapWeek)
		}
		return reset
	}
	reset := resets[len(resets)-1]
	for !reset.After(at) {
		reset = reset.Add(cswapWeek)
	}
	return reset
}
