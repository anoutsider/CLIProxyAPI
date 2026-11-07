package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"golang.org/x/crypto/bcrypt"
)

func TestUsageReadKeyRestrictsManagementRequests(t *testing.T) {
	const fullKey, usageKey = "full-key", "usage-key"
	hash := func(secret string) string {
		hashed, errHash := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
		if errHash != nil {
			t.Fatal(errHash)
		}
		return string(hashed)
	}

	manager := coreauth.NewManager(nil, nil, nil)
	auths := map[string]*coreauth.Auth{
		"claude": {ID: "claude-1", Provider: "claude", Attributes: map[string]string{"runtime_only": "true"}},
		"codex":  {ID: "codex-1", Provider: "codex", Attributes: map[string]string{"runtime_only": "true"}},
		"apikey": {ID: "gemini-1", Provider: "gemini", Attributes: map[string]string{"runtime_only": "true", "api_key": "gemini-secret"}},
	}
	for _, auth := range auths {
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		auth.EnsureIndex()
	}

	cfg := &config.Config{}
	cfg.RemoteManagement.SecretKey = hash(fullKey)
	cfg.RemoteManagement.UsageReadKey = hash(usageKey)
	h := &Handler{cfg: cfg, authManager: manager, failedAttempts: make(map[string]*attemptInfo)}

	engine := gin.New()
	mgmt := engine.Group("/v0/management", h.Middleware())
	mgmt.GET("/auth-files", h.ListAuthFiles)
	mgmt.GET("/auth-files/download", func(c *gin.Context) { c.Status(http.StatusOK) })
	mgmt.GET("/config", func(c *gin.Context) { c.Status(http.StatusOK) })
	// The stub decodes the body the way APICall does, to prove the middleware restores it.
	mgmt.POST("/api-call", func(c *gin.Context) {
		var body apiCallRequest
		if errBind := c.ShouldBindJSON(&body); errBind != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.String(http.StatusOK, body.URL)
	})

	apiCall := func(auth, method, url, extra string) string {
		return `{"auth_index":"` + auths[auth].Index + `","method":"` + method + `","url":"` + url + `","header":{"Authorization":"Bearer $TOKEN$"}` + extra + `}`
	}
	const anthropicUsage = "https://api.anthropic.com/api/oauth/usage"
	const codexUsage = "https://chatgpt.com/backend-api/wham/usage"
	const codexCredits = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"

	cases := []struct {
		name, key, method, path, body string
		want                          int
	}{
		{"claude usage", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("claude", "GET", anthropicUsage+"?scope=all", ""), http.StatusOK},
		{"codex usage", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("codex", "GET", codexUsage, ""), http.StatusOK},
		{"codex credits", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("codex", "GET", codexCredits, ""), http.StatusOK},
		{"disallowed url", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("claude", "GET", "https://example.com/api/oauth/usage", ""), http.StatusForbidden},
		{"codex url with query", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("codex", "GET", codexUsage+"?x=1", ""), http.StatusForbidden},
		{"credit redeem post", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("codex", "POST", codexCredits+"/consume", `,"data":"{}"`), http.StatusForbidden},
		{"post to usage url", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("claude", "POST", anthropicUsage, ""), http.StatusForbidden},
		{"request proxy", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("claude", "GET", anthropicUsage, `,"proxy_url":"http://127.0.0.1:9"`), http.StatusForbidden},
		{"host override", usageKey, http.MethodPost, "/v0/management/api-call", `{"auth_index":"` + auths["claude"].Index + `","method":"GET","url":"` + anthropicUsage + `","header":{"Host":"example.com"}}`, http.StatusForbidden},
		{"credential of another provider", usageKey, http.MethodPost, "/v0/management/api-call", apiCall("codex", "GET", anthropicUsage, ""), http.StatusForbidden},
		{"auth file download", usageKey, http.MethodGet, "/v0/management/auth-files/download?name=claude-1", "", http.StatusForbidden},
		{"other route", usageKey, http.MethodGet, "/v0/management/config", "", http.StatusForbidden},
		{"full key any url", fullKey, http.MethodPost, "/v0/management/api-call", apiCall("claude", "POST", "https://example.com/", ""), http.StatusOK},
		{"full key other route", fullKey, http.MethodGet, "/v0/management/config", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.RemoteAddr = "127.0.0.1:12345"
			req.Header.Set("Authorization", "Bearer "+tc.key)
			engine.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}

	// The full key sees the API key in "account"; the usage read key gets only display fields.
	listAuthFiles := func(key string) (map[string]any, string) {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Authorization", "Bearer "+key)
		engine.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("auth-files status = %d, body %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Files []map[string]any `json:"files"`
		}
		if errDecode := json.Unmarshal(rec.Body.Bytes(), &resp); errDecode != nil {
			t.Fatal(errDecode)
		}
		for _, file := range resp.Files {
			if file["id"] == "gemini-1" {
				return file, rec.Body.String()
			}
		}
		t.Fatalf("auth-files did not list gemini-1: %s", rec.Body.String())
		return nil, ""
	}
	if full, _ := listAuthFiles(fullKey); full["account"] != "gemini-secret" {
		t.Fatalf("full key account = %v, want the API key", full["account"])
	}
	usage, usageBody := listAuthFiles(usageKey)
	if usage["auth_index"] != auths["apikey"].Index || usage["provider"] != "gemini" {
		t.Fatalf("usage read key entry lacks display fields: %v", usage)
	}
	if strings.Contains(usageBody, "gemini-secret") {
		t.Fatal("usage read key received a credential value from auth-files")
	}

	if allowed, statusCode, _ := h.AuthenticateManagementKey("127.0.0.1", true, usageKey); allowed || statusCode != http.StatusForbidden {
		t.Fatalf("AuthenticateManagementKey(usage key) = %v, %d; want refused with 403", allowed, statusCode)
	}
}
