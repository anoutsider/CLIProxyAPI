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

type scopedWindowStore struct {
	mu     sync.RWMutex
	byAuth map[string][]ScopedWindow
}

var claudeScopedWindows = &scopedWindowStore{byAuth: make(map[string][]ScopedWindow)}

// SetClaudeScopedWindows replaces the per-model weekly windows recorded for an account.
func SetClaudeScopedWindows(authID string, windows []ScopedWindow) {
	if authID == "" {
		return
	}
	copied := append([]ScopedWindow(nil), windows...)
	claudeScopedWindows.mu.Lock()
	claudeScopedWindows.byAuth[authID] = copied
	claudeScopedWindows.mu.Unlock()
}

// ClearClaudeScopedWindows forgets the per-model windows recorded for an account.
func ClearClaudeScopedWindows(authID string) {
	claudeScopedWindows.mu.Lock()
	delete(claudeScopedWindows.byAuth, authID)
	claudeScopedWindows.mu.Unlock()
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
	for _, window := range claudeScopedWindows.byAuth[authID] {
		name := strings.ToLower(strings.TrimSpace(window.Name))
		if name != "" && strings.Contains(model, name) {
			return window, true
		}
	}
	return ScopedWindow{}, false
}

// SoonestResetSelector sends each new session to the eligible account whose
// seven-day window resets soonest, so quota that is about to reset is spent
// first. An account is eligible while its seven-day window, and the requested
// model's weekly window when one is known, are below Threshold.
type SoonestResetSelector struct {
	// Threshold is the utilization, from 0 to 1, at which an account stops
	// receiving new sessions. Zero means DefaultSoonestResetThreshold.
	Threshold float64

	// now is replaced in tests.
	now func() time.Time
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
	return pickSoonestReset(available, model, s.threshold(), now), nil
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

// remainingPerHour is the unused fraction of the window spread over the hours left.
func (c resetCandidate) remainingPerHour(now time.Time) float64 {
	hours := c.reset.Sub(now).Hours()
	if hours < 1 {
		hours = 1
	}
	return (1 - c.used) / hours
}

func pickSoonestReset(available []*Auth, model string, threshold float64, now time.Time) *Auth {
	if len(available) == 0 {
		return nil
	}
	candidates := make([]resetCandidate, 0, len(available))
	for i, auth := range available {
		candidates = append(candidates, resetCandidateFor(auth, i, model, now))
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
			left, right := candidates[i], candidates[j]
			if left.used != right.used {
				return left.used < right.used
			}
			return left.order < right.order
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
	used, okUsed := parseUtilization(auth.Quota.Signals[claudeSevenDayUtilizationSignal])
	reset, okReset := parseUnixSeconds(auth.Quota.Signals[claudeSevenDayResetSignal])
	if okUsed && okReset && reset.After(now) {
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
