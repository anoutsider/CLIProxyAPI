package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	claudeauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// claudeUsageURL is replaced in tests.
var claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

const (
	claudeScopedUsageInterval     = 15 * time.Minute
	claudeScopedUsageMinBackoff   = 30 * time.Minute
	claudeScopedUsageMaxBackoff   = 2 * time.Hour
	claudeScopedUsageTick         = time.Minute
	claudeScopedUsageTimeout      = 20 * time.Second
	claudeScopedUsageMaxBodyBytes = 1 << 20
)

// claudeScopedUsagePoller reads each Claude account's per-model weekly windows,
// such as Fable's, from Anthropic's usage endpoint while the soonest-reset
// strategy is active. Response headers carry only the account-wide seven-day
// window, so this endpoint is the only source of the per-model ones.
type claudeScopedUsagePoller struct {
	service *Service
	fetch   func(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (claudeUsageResult, error)

	mu      sync.Mutex
	next    map[string]time.Time
	backoff map[string]time.Duration
}

type claudeUsageResult struct {
	status     int
	retryAfter time.Duration
	account    *coreauth.ScopedWindow
	windows    []coreauth.ScopedWindow
}

func newClaudeScopedUsagePoller(service *Service) *claudeScopedUsagePoller {
	return &claudeScopedUsagePoller{
		service: service,
		fetch:   fetchClaudeUsage,
		next:    make(map[string]time.Time),
		backoff: make(map[string]time.Duration),
	}
}

func (p *claudeScopedUsagePoller) run(ctx context.Context) {
	ticker := time.NewTicker(claudeScopedUsageTick)
	defer ticker.Stop()
	p.tick(ctx, time.Now())
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			p.tick(ctx, now)
		}
	}
}

func (p *claudeScopedUsagePoller) currentConfig() *config.Config {
	if p == nil || p.service == nil {
		return nil
	}
	p.service.cfgMu.RLock()
	defer p.service.cfgMu.RUnlock()
	return p.service.cfg
}

func (p *claudeScopedUsagePoller) tick(ctx context.Context, now time.Time) {
	cfg := p.currentConfig()
	if normalizedRoutingRuntimeState(cfg).strategy != "soonest-reset" {
		return
	}
	if p.service == nil || p.service.coreManager == nil {
		return
	}
	auths := p.service.coreManager.List()
	p.retain(auths)
	for _, auth := range auths {
		if ctx.Err() != nil {
			return
		}
		if !claudeUsageEligible(auth, now) || !p.due(auth.ID, now) {
			continue
		}
		result, err := p.fetch(ctx, cfg, auth)
		p.record(auth.ID, result, err, now)
	}
}

// retain forgets poll state and usage windows for accounts the manager no longer holds.
func (p *claudeScopedUsagePoller) retain(auths []*coreauth.Auth) {
	keep := make(map[string]struct{}, len(auths))
	for _, auth := range auths {
		if auth != nil {
			keep[auth.ID] = struct{}{}
		}
	}
	coreauth.RetainClaudeScopedWindows(keep)
	p.mu.Lock()
	defer p.mu.Unlock()
	for authID := range p.next {
		if _, ok := keep[authID]; !ok {
			delete(p.next, authID)
			delete(p.backoff, authID)
		}
	}
}

// claudeUsageEligible reports whether an account can be polled now: an enabled
// Claude OAuth account whose access token has not expired. The refresh loop
// renews an expired token, and the next tick polls it.
func claudeUsageEligible(auth *coreauth.Auth, now time.Time) bool {
	if auth == nil || auth.Disabled || !strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") {
		return false
	}
	if auth.Attributes != nil && strings.TrimSpace(auth.Attributes["api_key"]) != "" {
		return false
	}
	if claudeauth.ReadMetadataString(&auth.Metadata, "access_token") == "" {
		return false
	}
	if expired, err := time.Parse(time.RFC3339, claudeauth.ReadMetadataString(&auth.Metadata, "expired")); err == nil && !now.Before(expired) {
		return false
	}
	return true
}

func (p *claudeScopedUsagePoller) due(authID string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	next, ok := p.next[authID]
	return !ok || !now.Before(next)
}

func (p *claudeScopedUsagePoller) record(authID string, result claudeUsageResult, err error, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case err == nil && result.status == http.StatusOK:
		coreauth.SetClaudeUsageWindows(authID, result.account, result.windows)
		log.Debugf("claude scoped usage: account %s has %d per-model windows", authID, len(result.windows))
		delete(p.backoff, authID)
		p.next[authID] = now.Add(claudeScopedUsageInterval)
	case result.status == http.StatusTooManyRequests:
		wait := p.backoff[authID] * 2
		if wait < claudeScopedUsageMinBackoff {
			wait = claudeScopedUsageMinBackoff
		}
		if wait > claudeScopedUsageMaxBackoff {
			wait = claudeScopedUsageMaxBackoff
		}
		if result.retryAfter > wait {
			wait = result.retryAfter
		}
		p.backoff[authID] = wait
		p.next[authID] = now.Add(wait)
		log.Infof("claude scoped usage: account %s rate limited, next poll in %s", authID, wait)
	default:
		p.next[authID] = now.Add(claudeScopedUsageInterval)
		log.Warnf("claude scoped usage: account %s poll failed (status %d): %v", authID, result.status, err)
	}
}

func fetchClaudeUsage(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (claudeUsageResult, error) {
	token := claudeauth.ReadMetadataString(&auth.Metadata, "access_token")
	reqCtx, cancel := context.WithTimeout(ctx, claudeScopedUsageTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		return claudeUsageResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "claude-cli/"+helps.DefaultClaudeVersion(cfg)+" (external, cli)")
	client := helps.NewUtlsHTTPClient(reqCtx, cfg, auth, claudeScopedUsageTimeout)
	resp, err := client.Do(req)
	if err != nil {
		return claudeUsageResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	result := claudeUsageResult{status: resp.StatusCode, retryAfter: parseRetryAfterSeconds(resp.Header.Get("Retry-After"))}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, claudeScopedUsageMaxBodyBytes))
		return result, fmt.Errorf("usage endpoint returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudeScopedUsageMaxBodyBytes))
	if err != nil {
		return result, err
	}
	result.account, result.windows, err = parseClaudeUsageWindows(body, time.Now())
	return result, err
}

func parseRetryAfterSeconds(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// parseClaudeUsageWindows reads the limits of a usage response: the account-wide
// "weekly_all" window and one "weekly_scoped" window per model.
func parseClaudeUsageWindows(body []byte, observedAt time.Time) (*coreauth.ScopedWindow, []coreauth.ScopedWindow, error) {
	var payload struct {
		Limits []struct {
			Kind     string   `json:"kind"`
			Percent  *float64 `json:"percent"`
			ResetsAt *string  `json:"resets_at"`
			Scope    *struct {
				Model struct {
					DisplayName string `json:"display_name"`
				} `json:"model"`
			} `json:"scope"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, nil, err
	}
	var account *coreauth.ScopedWindow
	windows := make([]coreauth.ScopedWindow, 0, len(payload.Limits))
	for _, limit := range payload.Limits {
		if limit.Percent == nil || limit.ResetsAt == nil {
			continue
		}
		resetsAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*limit.ResetsAt))
		if err != nil {
			continue
		}
		window := coreauth.ScopedWindow{Utilization: *limit.Percent / 100, ResetsAt: resetsAt, ObservedAt: observedAt}
		switch limit.Kind {
		case "weekly_all":
			account = &window
		case "weekly_scoped":
			if limit.Scope == nil || strings.TrimSpace(limit.Scope.Model.DisplayName) == "" {
				continue
			}
			window.Name = strings.TrimSpace(limit.Scope.Model.DisplayName)
			windows = append(windows, window)
		}
	}
	return account, windows, nil
}
