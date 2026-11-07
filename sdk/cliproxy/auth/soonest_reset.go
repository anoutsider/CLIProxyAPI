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
// model's weekly window when one is known, are below Threshold. When no
// candidate has window data, as for non-Claude providers, it rotates round-robin.
type SoonestResetSelector struct {
	// Threshold is the utilization, from 0 to 1, at which an account stops
	// receiving new sessions. Zero means DefaultSoonestResetThreshold.
	Threshold float64

	// now is replaced in tests.
	now func() time.Time

	roundRobin RoundRobinSelector
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
	if picked := pickSoonestReset(available, model, s.threshold(), now); picked != nil {
		return picked, nil
	}
	return s.roundRobin.Pick(ctx, provider, model, opts, available)
}

type resetCandidate struct {
	auth  *Auth
	order int
	known bool
	used  float64
	reset time.Time
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

func pickSoonestReset(available []*Auth, model string, threshold float64, now time.Time) *Auth {
	candidates := make([]resetCandidate, 0, len(available))
	anyKnown := false
	for i, auth := range available {
		candidate := resetCandidateFor(auth, i, model, now)
		anyKnown = anyKnown || candidate.known || candidate.modelUsed >= 0
		candidates = append(candidates, candidate)
	}
	if !anyKnown {
		return nil
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
		return candidates[0].auth
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		left, right := eligible[i], eligible[j]
		if left.known != right.known {
			return left.known
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
	return eligible[0].auth
}

func resetCandidateFor(auth *Auth, order int, model string, now time.Time) resetCandidate {
	candidate := resetCandidate{auth: auth, order: order, modelUsed: -1}
	if auth == nil {
		return candidate
	}
	// A window whose reset time has passed has started over, so its old
	// utilization no longer applies and the account ranks with unknown data.
	// Response headers are the fresher source. The usage endpoint's account-wide
	// window covers accounts that have served no request since the process started,
	// because the header snapshot is held in memory only.
	used, okUsed := parseUtilization(auth.Quota.Signals[claudeSevenDayUtilizationSignal])
	reset, okReset := parseUnixSeconds(auth.Quota.Signals[claudeSevenDayResetSignal])
	if okUsed && okReset && reset.After(now) {
		candidate.known = true
		candidate.used = used
		candidate.reset = reset
	} else if window, ok := claudeAccountWindowFor(auth.ID); ok && window.ResetsAt.After(now) {
		candidate.known = true
		candidate.used = window.Utilization
		candidate.reset = window.ResetsAt
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
