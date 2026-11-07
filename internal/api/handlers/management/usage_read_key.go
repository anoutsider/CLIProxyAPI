package management

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// managementKeyScope is the access a management key grants.
type managementKeyScope int

const (
	managementKeyNone managementKeyScope = iota
	managementKeyFull
	managementKeyUsageRead
)

// usageReadKeyContextKey marks a request authenticated with the usage read key.
const usageReadKeyContextKey = "management.usage-read-key"

const usageReadKeyForbidden = "usage read key is not permitted for this request"

// usageReadAPICallTarget is an api-call URL the usage read key may request with GET,
// using a credential of provider.
type usageReadAPICallTarget struct {
	host       string
	path       string
	allowQuery bool
	provider   string
}

// usageReadAPICallTargets are the usage URLs T3 Code reads through api-call.
var usageReadAPICallTargets = []usageReadAPICallTarget{
	{host: "api.anthropic.com", path: "/api/oauth/usage", allowQuery: true, provider: "claude"},
	{host: "chatgpt.com", path: "/backend-api/wham/usage", provider: "codex"},
	{host: "chatgpt.com", path: "/backend-api/wham/rate-limit-reset-credits", provider: "codex"},
}

// usageReadAuthFileFields are the auth-files entry fields returned to the usage read key.
var usageReadAuthFileFields = []string{"id", "auth_index", "name", "type", "provider", "email", "disabled", "id_token"}

// usageReadKeyAllows reports whether the usage read key may make request c. It reads the
// api-call body and restores it for the handler.
func (h *Handler) usageReadKeyAllows(c *gin.Context) bool {
	switch c.Request.Method + " " + c.FullPath() {
	case http.MethodGet + " /v0/management/auth-files":
		return true
	case http.MethodPost + " /v0/management/api-call":
		return h.usageReadAPICallAllowed(c)
	default:
		return false
	}
}

func (h *Handler) usageReadAPICallAllowed(c *gin.Context) bool {
	if c.Request.Body == nil {
		return false
	}
	raw, errRead := io.ReadAll(c.Request.Body)
	if errRead != nil {
		return false
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Decode with the same binding as APICall so both read the same fields.
	var body apiCallRequest
	if errBind := binding.JSON.BindBody(raw, &body); errBind != nil {
		return false
	}
	if strings.ToUpper(strings.TrimSpace(body.Method)) != http.MethodGet || strings.TrimSpace(body.ProxyURL) != "" {
		return false
	}
	for key := range body.Header {
		if strings.EqualFold(key, "host") {
			return false
		}
	}
	target, ok := usageReadAPICallTargetFor(strings.TrimSpace(body.URL))
	if !ok {
		return false
	}
	auth := h.authByIndex(firstNonEmptyString(body.AuthIndexSnake, body.AuthIndexCamel, body.AuthIndexPascal))
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), target.provider)
}

func usageReadAPICallTargetFor(rawURL string) (usageReadAPICallTarget, bool) {
	parsed, errParse := url.Parse(rawURL)
	if errParse != nil || parsed.Scheme != "https" || parsed.Opaque != "" || parsed.User != nil || parsed.Fragment != "" {
		return usageReadAPICallTarget{}, false
	}
	hasQuery := parsed.RawQuery != "" || parsed.ForceQuery
	for _, target := range usageReadAPICallTargets {
		if parsed.Host == target.host && parsed.EscapedPath() == target.path && (target.allowQuery || !hasQuery) {
			return target, true
		}
	}
	return usageReadAPICallTarget{}, false
}

// usageReadAuthFileEntry returns entry reduced to usageReadAuthFileFields when c is
// authenticated with the usage read key, so no credential value reaches that key.
func usageReadAuthFileEntry(c *gin.Context, entry gin.H) gin.H {
	if !c.GetBool(usageReadKeyContextKey) {
		return entry
	}
	reduced := make(gin.H, len(usageReadAuthFileFields))
	for _, field := range usageReadAuthFileFields {
		if value, ok := entry[field]; ok {
			reduced[field] = value
		}
	}
	return reduced
}
