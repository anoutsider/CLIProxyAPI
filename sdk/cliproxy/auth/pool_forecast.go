package auth

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// Pool forecast account states.
const (
	PoolStateNext        = "next"
	PoolStateEligible    = "eligible"
	PoolStateDraining    = "draining"
	PoolStateAtLimit     = "at-limit"
	PoolStateNoData      = "no-data"
	PoolStateNeedsSignIn = "needs-sign-in"
	PoolStateDisabled    = "disabled"
)

// Pool forecast alert kinds.
const (
	PoolAlertRunsOut = "runs-out"
	PoolAlertSignIn  = "sign-in"
)

const (
	poolSevenDays        = 7 * 24 * time.Hour
	poolProjectionHours  = 7 * 24
	poolHistoryHours     = 7 * 24
	poolBurnDays         = 14
	poolMinBurnDays      = 3
	poolResetDrop        = 0.3
	poolRunsOutAlertTime = 72 * time.Hour
	// poolSampleMaxAge is how long a sample stands for an account's state when no
	// newer one exists. An account without a sample for longer is left out.
	poolSampleMaxAge = 6 * time.Hour
	poolFull         = 1 - 1e-9
)

// PoolAccount is one Claude account's current state, the input of BuildPoolForecast.
type PoolAccount struct {
	ID       string
	Email    string
	Disabled bool
	// SignInRefusedAt is when the account's login was first seen refused; zero
	// when it is not refused.
	SignInRefusedAt time.Time
	// SevenDay is the current seven-day window, or nil when unknown.
	SevenDay *ScopedWindow
	// Models are the current per-model weekly windows.
	Models []ScopedWindow
}

// PoolForecastInput is what BuildPoolForecast reads.
type PoolForecastInput struct {
	Now time.Time
	// Threshold and DrainWindow are the soonest-reset routing limits, with the
	// meaning of the SoonestResetSelector fields.
	Threshold   float64
	DrainWindow time.Duration
	Accounts    []PoolAccount
	// History holds the stored samples; samples after Now are ignored.
	History []PoolSample
}

// PoolForecast is the GET /v0/management/pool-forecast response.
type PoolForecast struct {
	GeneratedAt      time.Time             `json:"generatedAt"`
	Threshold        float64               `json:"threshold"`
	DrainWindowHours float64               `json:"drainWindowHours"`
	HistoryDays      int                   `json:"historyDays"`
	Accounts         []PoolForecastAccount `json:"accounts"`
	Pool             PoolSummary           `json:"pool"`
	History          []PoolHistoryPoint    `json:"history"`
	Projection       PoolProjection        `json:"projection"`
	Accuracy         *PoolAccuracy         `json:"accuracy"`
	Alerts           []PoolAlert           `json:"alerts"`
}

// PoolForecastAccount is one account of the forecast.
type PoolForecastAccount struct {
	ID          string            `json:"id"`
	Email       string            `json:"email"`
	State       string            `json:"state"`
	SevenDay    *PoolSevenDay     `json:"sevenDay"`
	Models      []PoolModelWindow `json:"models"`
	LeftAtReset *PoolKinds        `json:"leftAtReset"`
}

// PoolSevenDay is an account's seven-day window.
type PoolSevenDay struct {
	Used       float64    `json:"used"`
	ResetsAt   time.Time  `json:"resetsAt"`
	ObservedAt *time.Time `json:"observedAt"`
}

// PoolModelWindow is an account's per-model weekly window.
type PoolModelWindow struct {
	Name     string    `json:"name"`
	Used     float64   `json:"used"`
	ResetsAt time.Time `json:"resetsAt"`
}

// PoolKinds holds one value for each kind of day.
type PoolKinds struct {
	Quiet   float64 `json:"quiet"`
	Typical float64 `json:"typical"`
	Busy    float64 `json:"busy"`
}

// PoolSummary sums the accounts that routing can use.
type PoolSummary struct {
	Accounts       int            `json:"accounts"`
	Remaining      float64        `json:"remaining"`
	FableRemaining float64        `json:"fableRemaining"`
	BurnPerDay     PoolBurn       `json:"burnPerDay"`
	RunsOutAt      PoolTimes      `json:"runsOutAt"`
	NextReset      *PoolNextReset `json:"nextReset"`
}

// PoolBurn is the daily burn of each kind of day and of the last 24 hours.
type PoolBurn struct {
	Quiet   *float64 `json:"quiet"`
	Typical *float64 `json:"typical"`
	Busy    *float64 `json:"busy"`
	Last24h float64  `json:"last24h"`
}

// PoolTimes holds one optional time for each kind of day.
type PoolTimes struct {
	Quiet   *time.Time `json:"quiet"`
	Typical *time.Time `json:"typical"`
	Busy    *time.Time `json:"busy"`
}

// PoolNextReset is the soonest seven-day reset of the pool.
type PoolNextReset struct {
	Email    string    `json:"email"`
	At       time.Time `json:"at"`
	Restores float64   `json:"restores"`
}

// PoolHistoryPoint is the pool's unused quota at one hour.
type PoolHistoryPoint struct {
	At             time.Time `json:"at"`
	Remaining      float64   `json:"remaining"`
	FableRemaining float64   `json:"fableRemaining"`
}

// PoolPoint is one projected hour.
type PoolPoint struct {
	At        time.Time `json:"at"`
	Remaining float64   `json:"remaining"`
}

// PoolProjection holds the hourly projection of each kind of day.
type PoolProjection struct {
	Quiet   []PoolPoint `json:"quiet"`
	Typical []PoolPoint `json:"typical"`
	Busy    []PoolPoint `json:"busy"`
}

// PoolAccuracy compares the typical-day prediction made at ForecastAt with the
// burn measured over the following 24 hours.
type PoolAccuracy struct {
	ForecastAt       time.Time `json:"forecastAt"`
	PredictedTypical float64   `json:"predictedTypical"`
	Actual           float64   `json:"actual"`
}

// PoolAlert is a condition to notify about. Key stays the same while the
// condition lasts, so a notifier can send each alert once.
type PoolAlert struct {
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Message string `json:"message"`
}

// poolSlot is one account in the projection.
type poolSlot struct {
	used  float64
	reset time.Time
}

type poolRun struct {
	points    []PoolPoint
	runsOutAt *time.Time
	// leftAtReset is each slot's unused fraction just before its first reset.
	leftAtReset []float64
}

// BuildPoolForecast forecasts the Claude pool from the accounts' current
// windows and the stored history.
func BuildPoolForecast(in PoolForecastInput) PoolForecast {
	now := in.Now.UTC().Truncate(time.Second)
	limits := (&SoonestResetSelector{Threshold: in.Threshold, DrainWindow: in.DrainWindow}).limits()
	history := make([]PoolSample, 0, len(in.History))
	for _, sample := range in.History {
		if !sample.At.After(now) {
			history = append(history, sample)
		}
	}
	sortPoolSamples(history)

	out := PoolForecast{
		GeneratedAt:      now,
		Threshold:        limits.threshold,
		DrainWindowHours: limits.drain.Hours(),
		Accounts:         make([]PoolForecastAccount, 0, len(in.Accounts)),
		History:          poolHistoryPoints(history, now),
		Alerts:           []PoolAlert{},
	}
	if len(history) > 0 {
		out.HistoryDays = int(math.Round(now.Sub(history[0].At).Hours() / 24))
	}

	accounts := poolAccountStates(in.Accounts, limits, now)
	var slots []poolSlot
	var slotAccount []int
	for i, account := range accounts {
		out.Accounts = append(out.Accounts, account.view)
		if !account.inPool {
			continue
		}
		window := in.Accounts[i].SevenDay
		slots = append(slots, poolSlot{used: window.Utilization, reset: window.ResetsAt})
		slotAccount = append(slotAccount, i)
		out.Pool.Remaining += 1 - window.Utilization
		if fable, ok := poolFableWindow(in.Accounts[i].Models, now); ok {
			out.Pool.FableRemaining += 1 - fable.Utilization
		}
	}
	out.Pool.Accounts = len(slots)
	out.Pool.Remaining = round4(out.Pool.Remaining)
	out.Pool.FableRemaining = round4(out.Pool.FableRemaining)

	days := poolDailyBurns(history, now)
	out.Pool.BurnPerDay.Last24h = round4(poolBurnBetween(history, now.Add(-24*time.Hour), now))
	rates, okRates := days.rates(now.Truncate(24 * time.Hour))
	if okRates {
		out.Pool.BurnPerDay.Quiet = floatPtr(round4(rates.Quiet))
		out.Pool.BurnPerDay.Typical = floatPtr(round4(rates.Typical))
		out.Pool.BurnPerDay.Busy = floatPtr(round4(rates.Busy))
	}
	out.Accuracy = days.accuracy(now)

	var runs [3]poolRun
	if okRates && len(slots) > 0 {
		for kind, rate := range []float64{rates.Quiet, rates.Typical, rates.Busy} {
			runs[kind] = projectPool(slots, rate, limits, now)
		}
		out.Projection = PoolProjection{Quiet: runs[0].points, Typical: runs[1].points, Busy: runs[2].points}
		out.Pool.RunsOutAt = PoolTimes{Quiet: runs[0].runsOutAt, Typical: runs[1].runsOutAt, Busy: runs[2].runsOutAt}
		for slot, i := range slotAccount {
			out.Accounts[i].LeftAtReset = &PoolKinds{
				Quiet:   round4(runs[0].leftAtReset[slot]),
				Typical: round4(runs[1].leftAtReset[slot]),
				Busy:    round4(runs[2].leftAtReset[slot]),
			}
		}
	}

	soonest := -1
	for slot := range slots {
		if soonest < 0 || slots[slot].reset.Before(slots[soonest].reset) {
			soonest = slot
		}
	}
	if soonest >= 0 {
		restores := slots[soonest].used
		if okRates {
			restores = 1 - runs[1].leftAtReset[soonest]
		}
		out.Pool.NextReset = &PoolNextReset{
			Email:    in.Accounts[slotAccount[soonest]].Email,
			At:       slots[soonest].reset.UTC().Truncate(time.Second),
			Restores: round4(restores),
		}
	}

	resets := make([]PoolNextReset, len(slots))
	for slot, i := range slotAccount {
		resets[slot] = PoolNextReset{Email: in.Accounts[i].Email, At: slots[slot].reset.UTC().Truncate(time.Second)}
	}
	out.Alerts = append(out.Alerts, poolSignInAlerts(in.Accounts, out.Accounts)...)
	if alert, ok := poolRunsOutAlert(out.Pool, resets, now); ok {
		out.Alerts = append(out.Alerts, alert)
	}
	return out
}

type poolAccountState struct {
	view   PoolForecastAccount
	inPool bool
}

// poolAccountStates gives each account its routing state. The account routing
// would pick for a new session, among those with data, is "next".
func poolAccountStates(accounts []PoolAccount, limits resetLimits, now time.Time) []poolAccountState {
	states := make([]poolAccountState, len(accounts))
	var eligible []resetCandidate
	for i, account := range accounts {
		view := PoolForecastAccount{ID: account.ID, Email: account.Email, Models: []PoolModelWindow{}}
		for _, model := range account.Models {
			if model.ResetsAt.After(now) {
				view.Models = append(view.Models, PoolModelWindow{Name: model.Name, Used: round4(model.Utilization), ResetsAt: model.ResetsAt.UTC().Truncate(time.Second)})
			}
		}
		if window := account.SevenDay; window != nil {
			view.SevenDay = &PoolSevenDay{Used: round4(window.Utilization), ResetsAt: window.ResetsAt.UTC().Truncate(time.Second)}
			if !window.ObservedAt.IsZero() {
				observed := window.ObservedAt.UTC().Truncate(time.Second)
				view.SevenDay.ObservedAt = &observed
			}
		}
		switch {
		case account.Disabled:
			view.State = PoolStateDisabled
		case !account.SignInRefusedAt.IsZero():
			view.State = PoolStateNeedsSignIn
		case account.SevenDay == nil || !account.SevenDay.ResetsAt.After(now):
			view.State = PoolStateNoData
		default:
			states[i].inPool = true
			candidate := resetCandidate{order: i, known: true, used: account.SevenDay.Utilization, reset: account.SevenDay.ResetsAt, modelUsed: -1}
			switch {
			case !candidate.eligible(limits, now):
				view.State = PoolStateAtLimit
			case candidate.used >= limits.threshold:
				view.State = PoolStateDraining
				eligible = append(eligible, candidate)
			default:
				view.State = PoolStateEligible
				eligible = append(eligible, candidate)
			}
		}
		states[i].view = view
	}
	if len(eligible) > 0 {
		sortEligible(eligible, now)
		states[eligible[0].order].view.State = PoolStateNext
	}
	return states
}

func poolFableWindow(models []ScopedWindow, now time.Time) (ScopedWindow, bool) {
	for _, model := range models {
		if strings.EqualFold(strings.TrimSpace(model.Name), "fable") && model.ResetsAt.After(now) {
			return model, true
		}
	}
	return ScopedWindow{}, false
}

// projectPool runs the pool forward hour by hour for 7 days at perDay burn.
// Each hour's burn is taken in routing order; at its reset an account's
// utilization returns to 0 and its next reset is 7 days later.
func projectPool(start []poolSlot, perDay float64, limits resetLimits, now time.Time) poolRun {
	slots := append([]poolSlot(nil), start...)
	run := poolRun{points: make([]PoolPoint, 0, poolProjectionHours), leftAtReset: make([]float64, len(slots))}
	resetSeen := make([]bool, len(slots))
	base := now.Truncate(time.Hour)
	prev := now
	for step := 1; step <= poolProjectionHours; step++ {
		at := base.Add(time.Duration(step) * time.Hour)
		takePoolBurn(slots, perDay/24*at.Sub(prev).Hours(), limits, prev)
		for i := range slots {
			for !slots[i].reset.After(at) {
				if !resetSeen[i] {
					resetSeen[i] = true
					run.leftAtReset[i] = 1 - slots[i].used
				}
				slots[i].used = 0
				slots[i].reset = slots[i].reset.Add(poolSevenDays)
			}
		}
		remaining, full := 0.0, true
		for _, slot := range slots {
			remaining += 1 - slot.used
			full = full && slot.used >= poolFull
		}
		if full && run.runsOutAt == nil {
			runsOut := at
			run.runsOutAt = &runsOut
		}
		run.points = append(run.points, PoolPoint{At: at, Remaining: round4(remaining)})
		prev = at
	}
	for i := range slots {
		if !resetSeen[i] {
			run.leftAtReset[i] = 1 - slots[i].used
		}
	}
	return run
}

// takePoolBurn spends amount from the slots the way routing sends sessions:
// eligible accounts first, each filled to its limit before the next; with none
// eligible, the least used account up to 100%. Burn beyond that is lost.
func takePoolBurn(slots []poolSlot, amount float64, limits resetLimits, at time.Time) {
	for amount > 1e-12 {
		candidates := make([]resetCandidate, 0, len(slots))
		eligible := make([]resetCandidate, 0, len(slots))
		for i, slot := range slots {
			candidate := resetCandidate{order: i, known: true, used: slot.used, reset: slot.reset, modelUsed: -1}
			candidates = append(candidates, candidate)
			if candidate.eligible(limits, at) {
				eligible = append(eligible, candidate)
			}
		}
		var chosen resetCandidate
		var limit float64
		if len(eligible) > 0 {
			sortEligible(eligible, at)
			chosen, limit = eligible[0], limits.limitFor(eligible[0].reset, at)
		} else {
			sortByFullest(candidates)
			chosen, limit = candidates[0], 1
			if chosen.used >= poolFull {
				return
			}
		}
		room := limit - chosen.used
		if amount < room {
			slots[chosen.order].used += amount
			return
		}
		slots[chosen.order].used = limit
		amount -= room
	}
}

// poolPairs calls fn for each pair of consecutive samples of one account that is
// not a reset: a drop of more than 0.3, or a reset time passed between them.
func poolPairs(history []PoolSample, fn func(prev, next PoolSample)) {
	last := make(map[string]PoolSample)
	for _, sample := range history {
		prev, ok := last[sample.Email]
		last[sample.Email] = sample
		if !ok {
			continue
		}
		if prev.Used-sample.Used > poolResetDrop {
			continue
		}
		if !prev.ResetsAt.IsZero() && !prev.ResetsAt.After(sample.At) {
			continue
		}
		fn(prev, sample)
	}
}

// poolDays holds the burn of each full UTC day of the history, from first on.
// A day is full when history began at or before its start, the day has ended,
// and no gap of more than poolSampleMaxAge between pool samples overlaps it.
type poolDays struct {
	first time.Time
	burns []float64
	gaps  []bool
}

// burn returns the burn of the full day starting at day.
func (d poolDays) burn(day time.Time) (float64, bool) {
	if day.Before(d.first) {
		return 0, false
	}
	index := int(day.Sub(d.first) / (24 * time.Hour))
	if index >= len(d.burns) || d.gaps[index] || !d.first.Add(time.Duration(index)*24*time.Hour).Equal(day) {
		return 0, false
	}
	return d.burns[index], true
}

// markGap marks the days that overlap the sampling gap (from, to).
func (d poolDays) markGap(from, to time.Time) {
	if to.Sub(from) <= poolSampleMaxAge || !to.After(d.first) {
		return
	}
	start := 0
	if from.After(d.first) {
		start = int(from.Sub(d.first) / (24 * time.Hour))
	}
	for index := start; index < len(d.gaps) && d.first.Add(time.Duration(index)*24*time.Hour).Before(to); index++ {
		d.gaps[index] = true
	}
}

func poolDailyBurns(history []PoolSample, now time.Time) poolDays {
	if len(history) == 0 {
		return poolDays{}
	}
	first := history[0].At.UTC().Truncate(24 * time.Hour)
	if first.Before(history[0].At) {
		first = first.Add(24 * time.Hour)
	}
	days := poolDays{first: first}
	if full := int(now.Sub(first) / (24 * time.Hour)); full > 0 {
		days.burns = make([]float64, full)
		days.gaps = make([]bool, full)
	}
	for i := 1; i < len(history); i++ {
		days.markGap(history[i-1].At, history[i].At)
	}
	days.markGap(history[len(history)-1].At, now)
	poolPairs(history, func(prev, next PoolSample) {
		if next.At.Before(first) {
			return
		}
		if index := int(next.At.Sub(first) / (24 * time.Hour)); index < len(days.burns) {
			days.burns[index] += next.Used - prev.Used
		}
	})
	return days
}

// poolBurnBetween is the burn of the pairs whose later sample is in (from, to].
func poolBurnBetween(history []PoolSample, from, to time.Time) float64 {
	total := 0.0
	poolPairs(history, func(prev, next PoolSample) {
		if next.At.After(from) && !next.At.After(to) {
			total += next.Used - prev.Used
		}
	})
	return total
}

// rates returns the 25th, 50th and 90th percentile of the burn of the last
// poolBurnDays full days before day, or false with fewer than poolMinBurnDays.
func (d poolDays) rates(day time.Time) (PoolKinds, bool) {
	values := make([]float64, 0, poolBurnDays)
	for back := 1; back <= poolBurnDays; back++ {
		if burn, ok := d.burn(day.Add(-time.Duration(back) * 24 * time.Hour)); ok {
			values = append(values, burn)
		}
	}
	if len(values) < poolMinBurnDays {
		return PoolKinds{}, false
	}
	sort.Float64s(values)
	return PoolKinds{Quiet: percentile(values, 0.25), Typical: percentile(values, 0.5), Busy: percentile(values, 0.9)}, true
}

// accuracy compares the typical prediction made at the start of yesterday with
// yesterday's burn.
func (d poolDays) accuracy(now time.Time) *PoolAccuracy {
	day := now.Truncate(24 * time.Hour).Add(-24 * time.Hour)
	actual, ok := d.burn(day)
	if !ok {
		return nil
	}
	rates, okRates := d.rates(day)
	if !okRates {
		return nil
	}
	return &PoolAccuracy{ForecastAt: day, PredictedTypical: round4(rates.Typical), Actual: round4(actual)}
}

// percentile interpolates linearly between the closest ranks of sorted values.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := p * float64(len(sorted)-1)
	low := int(math.Floor(rank))
	high := int(math.Ceil(rank))
	return sorted[low] + (sorted[high]-sorted[low])*(rank-float64(low))
}

// poolHistoryPoints returns the pool's unused quota at each hour of the last 7
// days, from each account's latest sample at or before the hour.
func poolHistoryPoints(history []PoolSample, now time.Time) []PoolHistoryPoint {
	byEmail := make(map[string][]PoolSample)
	for _, sample := range history {
		byEmail[sample.Email] = append(byEmail[sample.Email], sample)
	}
	points := make([]PoolHistoryPoint, 0, poolHistoryHours)
	base := now.Truncate(time.Hour)
	for back := poolHistoryHours - 1; back >= 0; back-- {
		at := base.Add(-time.Duration(back) * time.Hour)
		point := PoolHistoryPoint{At: at}
		counted := 0
		for _, samples := range byEmail {
			sample, ok := poolSampleAt(samples, at)
			if !ok {
				continue
			}
			counted++
			used, _ := poolUsedAt(sample, at)
			point.Remaining += 1 - used
			if sample.FableUsed != nil {
				fable := *sample.FableUsed
				if !sample.ResetsAt.After(at) {
					fable = 0
				}
				point.FableRemaining += 1 - fable
			}
		}
		if counted == 0 {
			continue
		}
		point.Remaining = round4(point.Remaining)
		point.FableRemaining = round4(point.FableRemaining)
		points = append(points, point)
	}
	return points
}

// poolSampleAt returns the latest of an account's samples at or before at, if it
// is no older than poolSampleMaxAge.
func poolSampleAt(samples []PoolSample, at time.Time) (PoolSample, bool) {
	index := sort.Search(len(samples), func(i int) bool { return samples[i].At.After(at) }) - 1
	if index < 0 || at.Sub(samples[index].At) > poolSampleMaxAge {
		return PoolSample{}, false
	}
	return samples[index], true
}

// poolUsedAt is a sample's utilization at a later time: 0 once its reset has
// passed, with the reset moved on by 7-day steps.
func poolUsedAt(sample PoolSample, at time.Time) (float64, time.Time) {
	reset := sample.ResetsAt
	if reset.IsZero() || reset.After(at) {
		return sample.Used, reset
	}
	for !reset.After(at) {
		reset = reset.Add(poolSevenDays)
	}
	return 0, reset
}

// PoolAccountsFromHistory rebuilds each account's state at now from stored
// samples, for a replay of the forecast over past data.
func PoolAccountsFromHistory(history []PoolSample, now time.Time) []PoolAccount {
	byEmail := make(map[string][]PoolSample)
	var emails []string
	for _, sample := range history {
		if sample.At.After(now) {
			continue
		}
		if _, ok := byEmail[sample.Email]; !ok {
			emails = append(emails, sample.Email)
		}
		byEmail[sample.Email] = append(byEmail[sample.Email], sample)
	}
	sort.Strings(emails)
	accounts := make([]PoolAccount, 0, len(emails))
	for _, email := range emails {
		samples := byEmail[email]
		sortPoolSamples(samples)
		sample, ok := poolSampleAt(samples, now)
		if !ok || sample.ResetsAt.IsZero() {
			continue
		}
		used, reset := poolUsedAt(sample, now)
		accounts = append(accounts, PoolAccount{
			ID:       email,
			Email:    email,
			SevenDay: &ScopedWindow{Utilization: used, ResetsAt: reset, ObservedAt: sample.At},
		})
	}
	return accounts
}

func poolSignInAlerts(accounts []PoolAccount, views []PoolForecastAccount) []PoolAlert {
	serving := 0
	for _, view := range views {
		if view.State != PoolStateDisabled && view.State != PoolStateNeedsSignIn {
			serving++
		}
	}
	var alerts []PoolAlert
	for i, account := range accounts {
		if views[i].State != PoolStateNeedsSignIn {
			continue
		}
		refused := account.SignInRefusedAt.UTC()
		alerts = append(alerts, PoolAlert{
			Kind:    PoolAlertSignIn,
			Key:     fmt.Sprintf("sign-in:%s:%d", account.ID, refused.Unix()),
			Message: fmt.Sprintf("%s needs signing in on the hub panel. Its login was refused at %s UTC. The other %d accounts are still serving.", account.Email, refused.Format("15:04"), serving),
		})
	}
	return alerts
}

// poolRunsOutAlert fires when the typical-day projection empties every account
// within 72 hours; the projection has already applied the resets before then.
// The message names the first of resets after the busy-day run-out, or
// pool.NextReset when the busy day does not run out.
func poolRunsOutAlert(pool PoolSummary, resets []PoolNextReset, now time.Time) (PoolAlert, bool) {
	typical := pool.RunsOutAt.Typical
	if typical == nil || typical.Sub(now) > poolRunsOutAlertTime {
		return PoolAlert{}, false
	}
	hours := func(at time.Time) int { return int(math.Round(at.Sub(now).Hours())) }
	busy := *typical
	if pool.RunsOutAt.Busy != nil {
		busy = *pool.RunsOutAt.Busy
	}
	span := fmt.Sprintf("%d hours to not within 7 days", hours(busy))
	if pool.RunsOutAt.Quiet != nil {
		span = fmt.Sprintf("%d to %d hours", hours(busy), hours(*pool.RunsOutAt.Quiet))
	}
	message := fmt.Sprintf("Claude quota runs out in %s (busy day to quiet day).", span)
	next := pool.NextReset
	if pool.RunsOutAt.Busy != nil {
		if reset, ok := poolResetAfter(resets, busy); ok {
			next = &reset
		}
	}
	if next != nil {
		gap := int(math.Round(next.At.Sub(busy).Hours()))
		if gap < 0 {
			gap = 0
		}
		message += fmt.Sprintf(" The next reset is %s in %d hours, +%d%%. Expect up to %d hours with no Claude quota.", next.Email, hours(next.At), int(math.Round(next.Restores*100)), gap)
	}
	return PoolAlert{Kind: PoolAlertRunsOut, Key: "runs-out:" + typical.UTC().Format("2006-01-02"), Message: message}, true
}

// poolResetAfter returns the earliest reset after at, each account's reset moved
// on by 7-day steps. Every account is full at a run-out, so it restores 100%.
func poolResetAfter(resets []PoolNextReset, at time.Time) (PoolNextReset, bool) {
	var first PoolNextReset
	found := false
	for _, reset := range resets {
		next := reset.At
		for !next.After(at) {
			next = next.Add(poolSevenDays)
		}
		if !found || next.Before(first.At) {
			first, found = PoolNextReset{Email: reset.Email, At: next, Restores: 1}, true
		}
	}
	return first, found
}

// poolSignInSeen records when a refused login was first seen, for accounts whose
// refusal time is not recorded on the auth.
var poolSignInSeen = struct {
	sync.Mutex
	at map[string]time.Time
}{at: make(map[string]time.Time)}

// ClaudePoolAccounts reads the current state of each Claude OAuth account.
func ClaudePoolAccounts(auths []*Auth, now time.Time) []PoolAccount {
	accounts := make([]PoolAccount, 0, len(auths))
	refused := make(map[string]struct{})
	for _, auth := range auths {
		if !claudeAccountLearnsFromResponses(auth) {
			continue
		}
		_, email := auth.AccountInfo()
		account := PoolAccount{
			ID:       auth.ID,
			Email:    email,
			Disabled: auth.Disabled || auth.Status == StatusDisabled,
			Models:   claudeModelWindows(auth.ID),
		}
		if window, ok := claudeSevenDayWindow(auth, now); ok {
			account.SevenDay = &window
		}
		if auth.RequiresSignIn || (auth.RefreshError != nil && strings.EqualFold(strings.TrimSpace(auth.RefreshError.Code), "invalid_grant")) {
			refused[auth.ID] = struct{}{}
			account.SignInRefusedAt = auth.RenewalEventAt
			if account.SignInRefusedAt.IsZero() {
				account.SignInRefusedAt = poolSignInFirstSeen(auth.ID, now)
			}
		}
		accounts = append(accounts, account)
	}
	poolSignInSeen.Lock()
	for authID := range poolSignInSeen.at {
		if _, ok := refused[authID]; !ok {
			delete(poolSignInSeen.at, authID)
		}
	}
	poolSignInSeen.Unlock()
	sort.SliceStable(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts
}

func poolSignInFirstSeen(authID string, now time.Time) time.Time {
	poolSignInSeen.Lock()
	defer poolSignInSeen.Unlock()
	if at, ok := poolSignInSeen.at[authID]; ok {
		return at
	}
	poolSignInSeen.at[authID] = now
	return now
}

// ClaudePoolSamples reads one pool sample per Claude OAuth account with a known
// seven-day window, keyed by the account's email.
func ClaudePoolSamples(auths []*Auth, now time.Time) []PoolSample {
	at := now.UTC().Truncate(PoolSampleInterval)
	samples := make([]PoolSample, 0, len(auths))
	for _, account := range ClaudePoolAccounts(auths, now) {
		if account.Email == "" || account.SevenDay == nil {
			continue
		}
		sample := PoolSample{At: at, Email: account.Email, Used: account.SevenDay.Utilization, ResetsAt: account.SevenDay.ResetsAt.UTC()}
		if fable, ok := poolFableWindow(account.Models, now); ok {
			sample.FableUsed = floatPtr(fable.Utilization)
		}
		samples = append(samples, sample)
	}
	return samples
}

func floatPtr(value float64) *float64 {
	return &value
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}
