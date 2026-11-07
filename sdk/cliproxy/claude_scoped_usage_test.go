package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestParseClaudeScopedWindowsReadsWeeklyScopedLimits(t *testing.T) {
	body := []byte(`{"five_hour":null,"seven_day":{"utilization":71,"resets_at":"2026-11-09T19:59:59Z"},
		"limits":[
			{"kind":"weekly_scoped","percent":42,"resets_at":"2026-11-09T20:00:00+00:00","scope":{"model":{"display_name":"Fable"}}},
			{"kind":"weekly","percent":71,"resets_at":"2026-11-09T20:00:00Z"},
			{"kind":"weekly_scoped","percent":null,"resets_at":"2026-11-09T20:00:00Z","scope":{"model":{"display_name":"Other"}}}
		]}`)
	windows, err := parseClaudeScopedWindows(body)
	if err != nil {
		t.Fatalf("parseClaudeScopedWindows() error = %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("windows = %+v, want only the Fable weekly_scoped limit", windows)
	}
	got := windows[0]
	want := time.Date(2026, 11, 9, 20, 0, 0, 0, time.UTC)
	if got.Name != "Fable" || got.Utilization != 0.42 || !got.ResetsAt.Equal(want) {
		t.Fatalf("window = %+v, want Fable at 0.42 resetting %s", got, want)
	}
}

func TestClaudeScopedUsagePollerBacksOffOnRateLimit(t *testing.T) {
	p := newClaudeScopedUsagePoller(nil)
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

	p.record("a", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	if p.due("a", now.Add(29*time.Minute)) || !p.due("a", now.Add(30*time.Minute)) {
		t.Fatalf("first 429 should wait 30 minutes")
	}
	p.record("a", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	if p.due("a", now.Add(59*time.Minute)) {
		t.Fatalf("second 429 should double the wait to 60 minutes")
	}
	for i := 0; i < 5; i++ {
		p.record("a", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	}
	if !p.due("a", now.Add(2*time.Hour)) {
		t.Fatalf("backoff should cap at 2 hours")
	}
	p.record("a", claudeUsageResult{status: http.StatusTooManyRequests, retryAfter: 3 * time.Hour}, errors.New("429"), now)
	if p.due("a", now.Add(2*time.Hour+time.Minute)) {
		t.Fatalf("a longer Retry-After should be honoured")
	}

	p.record("a", claudeUsageResult{status: http.StatusOK}, nil, now)
	if p.due("a", now.Add(14*time.Minute)) || !p.due("a", now.Add(15*time.Minute)) {
		t.Fatalf("a success should poll again after 15 minutes")
	}
	p.record("a", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	if !p.due("a", now.Add(30*time.Minute)) {
		t.Fatalf("a success should reset the backoff")
	}
}

func TestClaudeScopedUsagePollerRunsOnlyUnderSoonestReset(t *testing.T) {
	calls := 0
	service := &Service{cfg: &config.Config{}}
	p := newClaudeScopedUsagePoller(service)
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) (claudeUsageResult, error) {
		calls++
		return claudeUsageResult{status: http.StatusOK}, nil
	}
	p.tick(context.Background(), time.Now())
	if calls != 0 {
		t.Fatalf("poller fetched %d times under round-robin, want none", calls)
	}
}

func TestClaudeUsageEligibleNeedsAnActiveClaudeOAuthAccount(t *testing.T) {
	oauth := &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "fixture"}}
	if !claudeUsageEligible(oauth) {
		t.Fatalf("an active Claude OAuth account should be polled")
	}
	for name, auth := range map[string]*coreauth.Auth{
		"disabled": {ID: "b", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "fixture"}},
		"codex":    {ID: "c", Provider: "codex", Metadata: map[string]any{"access_token": "fixture"}},
		"api key":  {ID: "d", Provider: "claude", Attributes: map[string]string{"api_key": "fixture"}, Metadata: map[string]any{"access_token": "fixture"}},
		"no token": {ID: "e", Provider: "claude"},
	} {
		if claudeUsageEligible(auth) {
			t.Fatalf("%s account should not be polled", name)
		}
	}
}

func TestNewRoutingSelectorBuildsSoonestReset(t *testing.T) {
	cfg := &config.Config{}
	cfg.Routing.Strategy = "soonest-reset"
	cfg.Routing.SoonestResetThreshold = 0.8
	cfg.Routing.SessionAffinity = true
	state := normalizedRoutingRuntimeState(cfg)
	if state.strategy != "soonest-reset" || state.soonestResetThreshold != 0.8 {
		t.Fatalf("state = %+v, want soonest-reset at 0.8", state)
	}
	if _, ok := newRoutingSelector(state).(*coreauth.SessionAffinitySelector); !ok {
		t.Fatalf("session affinity should wrap the soonest-reset selector")
	}
	state.sessionAffinity = false
	selector, ok := newRoutingSelector(state).(*coreauth.SoonestResetSelector)
	if !ok || selector.Threshold != 0.8 {
		t.Fatalf("selector = %#v, want SoonestResetSelector at 0.8", newRoutingSelector(state))
	}
}
