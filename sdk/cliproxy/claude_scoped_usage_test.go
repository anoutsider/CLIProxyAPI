package cliproxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// The limits shape the usage endpoint returned on 2026-11-07.
func TestParseClaudeUsageWindowsReadsAccountAndModelLimits(t *testing.T) {
	body := []byte(`{"five_hour":null,"limits":[
		{"kind":"weekly_all","group":"weekly","percent":71,"severity":"normal","resets_at":"2026-11-09T19:59:59.453524+00:00","scope":null,"is_active":true},
		{"kind":"weekly_scoped","group":"weekly","percent":42,"severity":"normal","resets_at":"2026-11-09T20:00:00.453711+00:00","scope":{"model":{"id":null,"display_name":"Fable"},"surface":null},"is_active":false},
		{"kind":"weekly_scoped","percent":null,"resets_at":"2026-11-09T20:00:00Z","scope":{"model":{"display_name":"Other"}}}
	]}`)
	account, windows, err := parseClaudeUsageWindows(body)
	if err != nil {
		t.Fatalf("parseClaudeUsageWindows() error = %v", err)
	}
	if account == nil || account.Utilization != 0.71 || account.ResetsAt.Format(time.RFC3339) != "2026-11-09T19:59:59Z" {
		t.Fatalf("account window = %+v, want 0.71 resetting 2026-11-09T19:59:59Z", account)
	}
	if len(windows) != 1 || windows[0].Name != "Fable" || windows[0].Utilization != 0.42 {
		t.Fatalf("model windows = %+v, want only Fable at 0.42", windows)
	}
}

func TestFetchClaudeUsageSendsOAuthRequest(t *testing.T) {
	var gotAuth, gotBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotBeta = r.Header.Get("Authorization"), r.Header.Get("Anthropic-Beta")
		_, _ = w.Write([]byte(`{"limits":[{"kind":"weekly_all","percent":5,"resets_at":"2026-11-09T20:00:00Z","scope":null}]}`))
	}))
	defer server.Close()
	previous := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = previous })

	auth := &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "fixture-token"}}
	result, err := fetchClaudeUsage(context.Background(), &config.Config{}, auth)
	if err != nil || result.status != http.StatusOK {
		t.Fatalf("fetchClaudeUsage() = %+v, %v", result, err)
	}
	if gotAuth != "Bearer fixture-token" || gotBeta != "oauth-2025-04-20" {
		t.Fatalf("request headers Authorization=%q Anthropic-Beta=%q", gotAuth, gotBeta)
	}
	if result.account == nil || result.account.Utilization != 0.05 {
		t.Fatalf("account window = %+v, want 0.05", result.account)
	}
}

func TestFetchClaudeUsageReportsRetryAfterOnRateLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	previous := claudeUsageURL
	claudeUsageURL = server.URL
	t.Cleanup(func() { claudeUsageURL = previous })

	auth := &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "fixture-token"}}
	result, err := fetchClaudeUsage(context.Background(), &config.Config{}, auth)
	if err == nil || result.status != http.StatusTooManyRequests || result.retryAfter != 10*time.Minute {
		t.Fatalf("fetchClaudeUsage() = %+v, %v; want 429 with a 10 minute Retry-After", result, err)
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
	manager := coreauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(context.Background(), &coreauth.Auth{ID: "polled", Provider: "claude", Status: coreauth.StatusActive, Metadata: map[string]any{"access_token": "fixture"}}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { coreauth.ClearClaudeScopedWindows("polled") })
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	p := newClaudeScopedUsagePoller(service)
	calls := 0
	p.fetch = func(context.Context, *config.Config, *coreauth.Auth) (claudeUsageResult, error) {
		calls++
		return claudeUsageResult{status: http.StatusOK}, nil
	}
	now := time.Now()
	p.tick(context.Background(), now)
	if calls != 0 {
		t.Fatalf("poller fetched %d times under round-robin, want none", calls)
	}
	soonest := &config.Config{}
	soonest.Routing.Strategy = "soonest-reset"
	service.cfg = soonest
	p.tick(context.Background(), now)
	p.tick(context.Background(), now.Add(time.Minute))
	if calls != 1 {
		t.Fatalf("poller fetched %d times under soonest-reset within 15 minutes, want 1", calls)
	}
}

func TestClaudeUsageEligibleNeedsAnActiveClaudeOAuthAccount(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	oauth := &coreauth.Auth{ID: "a", Provider: "claude", Metadata: map[string]any{"access_token": "fixture", "expired": "2026-11-07T15:00:00Z"}}
	if !claudeUsageEligible(oauth, now) {
		t.Fatalf("an active Claude OAuth account should be polled")
	}
	for name, auth := range map[string]*coreauth.Auth{
		"disabled": {ID: "b", Provider: "claude", Disabled: true, Metadata: map[string]any{"access_token": "fixture"}},
		"codex":    {ID: "c", Provider: "codex", Metadata: map[string]any{"access_token": "fixture"}},
		"api key":  {ID: "d", Provider: "claude", Attributes: map[string]string{"api_key": "fixture"}, Metadata: map[string]any{"access_token": "fixture"}},
		"no token": {ID: "e", Provider: "claude"},
		"expired":  {ID: "f", Provider: "claude", Metadata: map[string]any{"access_token": "fixture", "expired": "2026-11-07T11:00:00Z"}},
	} {
		if claudeUsageEligible(auth, now) {
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

func TestClaudeScopedUsagePollerRetainForgetsRemovedAccounts(t *testing.T) {
	p := newClaudeScopedUsagePoller(nil)
	now := time.Now()
	p.record("gone", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	p.record("kept", claudeUsageResult{status: http.StatusTooManyRequests}, errors.New("429"), now)
	p.retain([]*coreauth.Auth{{ID: "kept"}})
	if _, ok := p.next["gone"]; ok {
		t.Fatalf("poll state of a removed account was kept")
	}
	if _, ok := p.backoff["kept"]; !ok {
		t.Fatalf("poll state of a kept account was dropped")
	}
}
