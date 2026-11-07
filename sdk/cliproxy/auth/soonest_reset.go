package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// DefaultSoonestResetThreshold is the utilization at which an account stops
// receiving new sessions under the soonest-reset strategy.
const DefaultSoonestResetThreshold = 0.9

// soonestResetProbeWindow is how long an account without data keeps its place at
// the front after it was first tried. A response normally supplies the data well
// within it; one that does not drops behind the accounts with data.
const soonestResetProbeWindow = 10 * time.Minute

// soonestResetReprobeInterval is how long after a probe an account without data
// can be tried again, for example after its first request failed. It also bounds
// how long the entry of a removed account is kept.
const soonestResetReprobeInterval = time.Hour

const (
	claudeSevenDayUtilizationSignal = "Anthropic-Ratelimit-Unified-7d-Utilization"
	claudeSevenDayResetSignal       = "Anthropic-Ratelimit-Unified-7d-Reset"
)

// ScopedWindow is one per-model weekly window, such as Fable's, read from
// Anthropic's usage endpoint for one account.
type ScopedWindow struct {
	// Name is the model's display name as Anthropic reports it, for example "Fable".
	Name string
	// Utilization is the fraction of the window used, from 0 to 1.
	Utilization float64
	// ResetsAt is when the window resets.
	ResetsAt time.Time
	// ObservedAt is when the window was read.
	ObservedAt time.Time
}

type claudeUsageWindows struct {
	account *ScopedWindow
	models  []ScopedWindow
}

type scopedWindowStore struct {
	mu     sync.RWMutex
	byAuth map[string]claudeUsageWindows
}

var claudeScopedWindows = &scopedWindowStore{byAuth: make(map[string]claudeUsageWindows)}

// SetClaudeUsageWindows replaces the windows read from Anthropic's usage endpoint
// for an account: the account-wide weekly window, when reported, and the
// per-model weekly windows.
func SetClaudeUsageWindows(authID string, account *ScopedWindow, models []ScopedWindow) {
	if authID == "" {
		return
	}
	entry := claudeUsageWindows{models: append([]ScopedWindow(nil), models...)}
	if account != nil {
		copied := *account
		entry.account = &copied
	}
	claudeScopedWindows.mu.Lock()
	claudeScopedWindows.byAuth[authID] = entry
	claudeScopedWindows.mu.Unlock()
}

// ClearClaudeScopedWindows forgets the usage windows recorded for an account.
func ClearClaudeScopedWindows(authID string) {
	claudeScopedWindows.mu.Lock()
	delete(claudeScopedWindows.byAuth, authID)
	claudeScopedWindows.mu.Unlock()
}

// RetainClaudeScopedWindows forgets the usage windows of every account not in keep.
func RetainClaudeScopedWindows(keep map[string]struct{}) {
	claudeScopedWindows.mu.Lock()
	defer claudeScopedWindows.mu.Unlock()
	for authID := range claudeScopedWindows.byAuth {
		if _, ok := keep[authID]; !ok {
			delete(claudeScopedWindows.byAuth, authID)
		}
	}
}

// claudeScopedWindowFor returns the per-model window whose name appears in the
// requested model ID, for example "Fable" in "claude-fable-5-1".
func claudeScopedWindowFor(authID, model string) (ScopedWindow, bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if authID == "" || model == "" {
		return ScopedWindow{}, false
	}
	claudeScopedWindows.mu.RLock()
	defer claudeScopedWindows.mu.RUnlock()
	for _, window := range claudeScopedWindows.byAuth[authID].models {
		name := strings.ToLower(strings.TrimSpace(window.Name))
		if name != "" && strings.Contains(model, name) {
			return window, true
		}
	}
	return ScopedWindow{}, false
}

func claudeAccountWindowFor(authID string) (ScopedWindow, bool) {
	claudeScopedWindows.mu.RLock()
	defer claudeScopedWindows.mu.RUnlock()
	account := claudeScopedWindows.byAuth[authID].account
	if account == nil {
		return ScopedWindow{}, false
	}
	return *account, true
}

// SoonestResetSelector sends each new session to the eligible account whose
// seven-day window resets soonest, so quota that is about to reset is spent
// first. An account is eligible while its seven-day window, and the requested
// model's weekly window when one is known, are below Threshold. An eligible
// account without seven-day data is tried first, so its next response supplies
// the data. When no candidate has window data, as for non-Claude providers, it
// rotates round-robin.
type SoonestResetSelector struct {
	// Threshold is the utilization, from 0 to 1, at which an account stops
	// receiving new sessions. Zero means DefaultSoonestResetThreshold.
	Threshold float64

	// now is replaced in tests.
	now func() time.Time

	roundRobin RoundRobinSelector

	mu     sync.Mutex
	probed map[string]time.Time
}

func (s *SoonestResetSelector) threshold() float64 {
	if s == nil || s.Threshold <= 0 || s.Threshold > 1 {
		return DefaultSoonestResetThreshold
	}
	return s.Threshold
}

func (s *SoonestResetSelector) clock() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

// Pick selects among the available auths for provider and model.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := s.clock()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	s.mu.Lock()
	if s.probed == nil {
		s.probed = make(map[string]time.Time)
	}
	picked, learning := pickSoonestReset(available, model, s.threshold(), now, s.probed)
	if picked != nil && learning {
		if _, ok := s.probed[picked.ID]; !ok {
			s.probed[picked.ID] = now
		}
	}
	s.mu.Unlock()
	if picked == nil {
		return s.roundRobin.Pick(ctx, provider, model, opts, available)
	}
	return picked, nil
}

type resetCandidate struct {
	auth  *Auth
	order int
	known bool
	// learns marks an account without data that goes first, because its next
	// response is expected to supply the data.
	learns bool
	used   float64
	reset  time.Time
	// modelUsed is the requested model's weekly utilization, or -1 when unknown.
	modelUsed float64
}

func (c resetCandidate) eligible(threshold float64) bool {
	if c.modelUsed >= threshold {
		return false
	}
	return !c.known || c.used < threshold
}

// fullest is the higher of the seven-day and model window utilizations.
func (c resetCandidate) fullest() float64 {
	return math.Max(c.used, c.modelUsed)
}

// remainingPerHour is the unused fraction of the window spread over the hours left.
func (c resetCandidate) remainingPerHour(now time.Time) float64 {
	hours := c.reset.Sub(now).Hours()
	if hours < 1 {
		hours = 1
	}
	return (1 - c.used) / hours
}

// pickSoonestReset returns the chosen account and whether it was chosen to learn
// its window, or nil when no candidate has any window data. probed holds when
// each account without data was first tried; entries of accounts with data are
// removed, so an account whose window resets can be tried again.
func pickSoonestReset(available []*Auth, model string, threshold float64, now time.Time, probed map[string]time.Time) (*Auth, bool) {
	for authID, first := range probed {
		if now.Sub(first) >= soonestResetReprobeInterval {
			delete(probed, authID)
		}
	}
	candidates := make([]resetCandidate, 0, len(available))
	anyKnown := false
	for i, auth := range available {
		candidate := resetCandidateFor(auth, i, model, now)
		if candidate.known {
			delete(probed, auth.ID)
		} else if claudeAccountLearnsFromResponses(auth) {
			first, tried := probed[auth.ID]
			candidate.learns = !tried || now.Sub(first) < soonestResetProbeWindow
		}
		anyKnown = anyKnown || candidate.known || candidate.modelUsed >= 0
		candidates = append(candidates, candidate)
	}
	if !anyKnown {
		return nil, false
	}
	eligible := make([]resetCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		if candidate.eligible(threshold) {
			eligible = append(eligible, candidate)
		}
	}
	if len(eligible) == 0 {
		// Every account is past the threshold: send the session where the most
		// quota is left rather than refusing a request the upstream may still serve.
		sort.SliceStable(candidates, func(i, j int) bool {
			left, right := candidates[i].fullest(), candidates[j].fullest()
			if left != right {
				return left < right
			}
			return candidates[i].order < candidates[j].order
		})
		return candidates[0].auth, false
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		// An account without data that can learn from a response goes first: one
		// response teaches its window, whereas ranking it last would leave it unused
		// until a poll succeeds. Other accounts without data go last.
		if left.rank() != right.rank() {
			return left.rank() < right.rank()
		}
		if !left.known {
			return left.order < right.order
		}
		if !left.reset.Equal(right.reset) {
			return left.reset.Before(right.reset)
		}
		leftRate, rightRate := left.remainingPerHour(now), right.remainingPerHour(now)
		if leftRate != rightRate {
			return leftRate > rightRate
		}
		return left.order < right.order
	})
	return eligible[0].auth, eligible[0].learns
}

// rank orders eligible candidates: learning accounts, then accounts with data,
// then accounts without data that cannot learn.
func (c resetCandidate) rank() int {
	switch {
	case c.learns:
		return 0
	case c.known:
		return 1
	default:
		return 2
	}
}

// claudeAccountLearnsFromResponses reports whether an account's responses carry
// the seven-day headers: a Claude OAuth account. API-key credentials do not.
func claudeAccountLearnsFromResponses(auth *Auth) bool {
	if auth == nil || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return false
	}
	return auth.Attributes == nil || strings.TrimSpace(auth.Attributes["api_key"]) == ""
}

func resetCandidateFor(auth *Auth, order int, model string, now time.Time) resetCandidate {
	candidate := resetCandidate{auth: auth, order: order, modelUsed: -1}
	if auth == nil {
		return candidate
	}
	// A window whose reset time has passed has started over, so its old
	// utilization no longer applies and the account ranks with unknown data.
	// Two sources give the seven-day window: response headers and the usage
	// endpoint's account-wide window. The newer one wins. The header snapshot is
	// held in memory only, so after a restart the polled window is all there is.
	used, okUsed := parseUtilization(auth.Quota.Signals[claudeSevenDayUtilizationSignal])
	reset, okReset := parseUnixSeconds(auth.Quota.Signals[claudeSevenDayResetSignal])
	headerValid := okUsed && okReset && reset.After(now)
	window, okWindow := claudeAccountWindowFor(auth.ID)
	windowValid := okWindow && window.ResetsAt.After(now)
	switch {
	case windowValid && (!headerValid || window.ObservedAt.After(auth.Quota.ObservedAt)):
		candidate.known = true
		candidate.used = window.Utilization
		candidate.reset = window.ResetsAt
	case headerValid:
		candidate.known = true
		candidate.used = used
		candidate.reset = reset
	}
	if window, ok := claudeScopedWindowFor(auth.ID, model); ok && window.ResetsAt.After(now) {
		candidate.modelUsed = window.Utilization
	}
	return candidate
}

// parseUtilization reads a utilization as a fraction from 0 to 1. A value above
// 1.5 is taken as a percentage.
func parseUtilization(value string) (float64, bool) {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
		return 0, false
	}
	if parsed > 1.5 {
		parsed /= 100
	}
	return parsed, true
}

func parseUnixSeconds(value string) (time.Time, bool) {
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || parsed <= 0 {
		return time.Time{}, false
	}
	return time.Unix(parsed, 0), true
}
