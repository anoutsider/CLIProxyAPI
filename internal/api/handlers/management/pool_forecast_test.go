package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

func TestPoolForecastServesUsageReadKeyWithoutTokens(t *testing.T) {
	const fullKey, usageKey = "full-key", "usage-key"
	const accessToken, refreshToken = "sk-ant-oat01-forecast-access", "sk-ant-ort01-forecast-refresh"
	hash := func(secret string) string {
		hashed, errHash := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
		if errHash != nil {
			t.Fatal(errHash)
		}
		return string(hashed)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID:         "claude-forecast@example.com.json",
		Provider:   "claude",
		Attributes: map[string]string{"runtime_only": "true"},
		Metadata:   map[string]any{"email": "forecast@example.com", "access_token": accessToken, "refresh_token": refreshToken},
		Quota: coreauth.QuotaState{Signals: map[string]string{
			"Anthropic-Ratelimit-Unified-7d-Utilization": "0.4",
			"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(time.Now().Add(48*time.Hour).Unix(), 10),
		}},
	}
	if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
		t.Fatal(errRegister)
	}
	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = hash(fullKey)
	cfg.RemoteManagement.UsageReadKey = hash(usageKey)
	h := &Handler{cfg: cfg, authManager: manager, failedAttempts: make(map[string]*attemptInfo)}
	engine := gin.New()
	engine.Group("/v0/management", h.Middleware()).GET("/pool-forecast", h.GetPoolForecast)

	for _, key := range []string{usageKey, fullKey} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/pool-forecast", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer "+key)
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("key %s: status = %d, body %s", key, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		if strings.Contains(body, accessToken) || strings.Contains(body, refreshToken) {
			t.Fatalf("key %s: pool forecast holds a token: %s", key, body)
		}
		var forecast coreauth.PoolForecast
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &forecast); errDecode != nil {
			t.Fatal(errDecode)
		}
		if len(forecast.Accounts) != 1 || forecast.Accounts[0].Email != "forecast@example.com" || forecast.Accounts[0].State != coreauth.PoolStateNext {
			t.Fatalf("key %s: accounts = %+v, want the one account as next", key, forecast.Accounts)
		}
		if forecast.Pool.Remaining != 0.6 {
			t.Fatalf("key %s: remaining = %v, want 0.6", key, forecast.Pool.Remaining)
		}
	}
}
