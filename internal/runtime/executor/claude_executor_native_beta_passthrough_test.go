package executor

import (
	"net/http"
	"strings"
	"testing"
)

// Beta lists Claude Code 2.1.319 sent through T3 on 2026-11-07, captured with
// request-log. A confirmed native client's own betas reach upstream in the same
// set and order, including effort on Haiku and on helper requests.
func TestApplyClaudeHeaders_ConfirmedClientBetasForwardedUnchanged(t *testing.T) {
	for _, tt := range []struct {
		name  string
		betas string
		body  string
	}{
		{
			name:  "haiku helper",
			betas: "oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,thinking-token-count-2026-05-13,effort-2025-11-24,structured-outputs-2025-12-15",
			body:  `{"model":"claude-haiku-5-5","output_config":{"format":{"type":"json_schema"}}}`,
		},
		{
			name:  "haiku main",
			betas: "oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,claude-code-20250219,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,thinking-token-count-2026-05-13,advanced-tool-use-2025-11-20,effort-2025-11-24,extended-cache-ttl-2025-04-11",
			body:  `{"model":"claude-haiku-5-5","thinking":{"type":"adaptive"},"output_config":{"effort":"medium"}}`,
		},
		{
			name:  "opus helper",
			betas: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,thinking-token-count-2026-05-13,effort-2025-11-24,structured-outputs-2025-12-15",
			body:  `{"model":"claude-opus-5-5","output_config":{"format":{"type":"json_schema"}}}`,
		},
		{
			name:  "opus main",
			betas: "claude-code-20250219,oauth-2025-04-20,interleaved-thinking-2025-05-14,context-management-2025-06-27,per-turn-control-2026-07-01,mid-conversation-tool-changes-2026-07-01,inline-tools-2026-09-15,advisor-tool-2026-03-01,thinking-token-count-2026-05-13,advanced-tool-use-2025-11-20,effort-2025-11-24,extended-cache-ttl-2025-04-11",
			body:  `{"model":"claude-opus-5-5","thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			incoming := http.Header{}
			incoming.Set("Anthropic-Beta", tt.betas)
			req := newClaudeHeaderTestRequest(t, nil)
			if err := applyClaudeHeaders(req, claudeOAuthAuthForBetaPolicy(), claudeRaceProbeOAuthKey, true, nil,
				[]byte(tt.body), nil, incoming, true); err != nil {
				t.Fatalf("applyClaudeHeaders() error = %v", err)
			}
			got := req.Header.Get("Anthropic-Beta")
			if !strings.HasPrefix(got, tt.betas) {
				t.Fatalf("Anthropic-Beta = %q, want the caller's list %q unchanged at the front", got, tt.betas)
			}
		})
	}
}

// A confirmed client that omits a beta its request needs still gets it added,
// and a gated beta the proxy would add itself stays gated.
func TestApplyClaudeHeaders_ConfirmedClientStillGetsProxyBetasGated(t *testing.T) {
	incoming := http.Header{}
	incoming.Set("Anthropic-Beta", claudeCodeBeta+",interleaved-thinking-2025-05-14")
	req := newClaudeHeaderTestRequest(t, nil)
	if err := applyClaudeHeaders(req, claudeOAuthAuthForBetaPolicy(), claudeRaceProbeOAuthKey, false, nil,
		[]byte(`{"model":"claude-haiku-5-5","tools":[{"type":"advisor_20260301","name":"advisor"}]}`), nil, incoming, true); err != nil {
		t.Fatalf("applyClaudeHeaders() error = %v", err)
	}
	got := req.Header.Get("Anthropic-Beta")
	for _, want := range []string{claudeOAuthBeta, claudeAdvisorToolBeta} {
		if !strings.Contains(got, want) {
			t.Fatalf("Anthropic-Beta = %q, want proxy-added %s", got, want)
		}
	}
	if strings.Contains(got, claudeEffortBeta) {
		t.Fatalf("Anthropic-Beta = %q, want no effort beta the caller did not send", got)
	}
}
