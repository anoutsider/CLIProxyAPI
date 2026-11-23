package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	cliproxysession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

// claudeThreadSoonestResetHub is a Manager with the hub's routing: session
// affinity that rebinds on quota, around soonest-reset at the 0.9 threshold.
// The owner account resets soonest, so a new session goes to it first.
type claudeThreadSoonestResetHub struct {
	manager      *Manager
	owner, soon  string
	late, caller string
	called       []string
}

func newClaudeThreadSoonestResetHub(t *testing.T, model string, ownerUsed float64) *claudeThreadSoonestResetHub {
	t.Helper()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{Threshold: 0.9, DrainWindow: 24 * time.Hour},
		TTL:      time.Hour,
		RebindOn: SessionAffinityRebindOnQuota,
	})
	t.Cleanup(selector.Stop)
	hub := &claudeThreadSoonestResetHub{caller: t.Name() + "-" + uuid.NewString()}
	hub.manager = NewManager(nil, selector, nil)
	hub.manager.SetRetryConfig(0, 0, 0)
	hub.owner, hub.soon, hub.late = hub.caller+"-a-owner", hub.caller+"-b-soon", hub.caller+"-c-late"
	windows := map[string]ScopedWindow{
		hub.owner: {Utilization: ownerUsed, ResetsAt: time.Now().Add(3 * 24 * time.Hour)},
		hub.soon:  {Utilization: 0.40, ResetsAt: time.Now().Add(4 * 24 * time.Hour)},
		hub.late:  {Utilization: 0.10, ResetsAt: time.Now().Add(6 * 24 * time.Hour)},
	}
	for id, window := range windows {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: model}})
		window.ObservedAt = time.Now()
		SetClaudeUsageWindows(id, &window, nil)
		authID := id
		t.Cleanup(func() {
			registry.GetGlobalRegistry().UnregisterClient(authID)
			ClearClaudeScopedWindows(authID)
		})
		auth := &Auth{ID: id, Provider: "claude", Status: StatusActive, Attributes: map[string]string{AttributeAuthKind: AuthKindOAuth}}
		if _, err := hub.manager.Register(WithSkipPersist(t.Context()), auth); err != nil {
			t.Fatal(err)
		}
	}
	hub.manager.RegisterExecutor(&claudeThreadTestExecutor{
		executeFunc: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			hub.called = append(hub.called, auth.ID)
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})
	return hub
}

func (h *claudeThreadSoonestResetHub) execute(t *testing.T, model, session, thread string) (string, error) {
	t.Helper()
	opts := cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FormatClaude,
		Headers:      http.Header{"X-Session-Id": []string{session}},
		Metadata:     map[string]any{cliproxyexecutor.CallerScopeMetadataKey: h.caller},
	}
	req := cliproxyexecutor.Request{
		Model:   model,
		Payload: []byte(`{"model":"` + model + `","thread":` + thread + `,"messages":[{"role":"user","content":"next"}]}`),
	}
	resp, err := h.manager.Execute(WithSkipPersist(t.Context()), []string{"claude"}, req, opts)
	return string(resp.Payload), err
}

func (h *claudeThreadSoonestResetHub) rememberThread(msgID string) {
	scope := cliproxysession.ClaudeThreadCallerScope("", map[string]any{cliproxyexecutor.CallerScopeMetadataKey: h.caller})
	cliproxysession.RememberClaudeThreadAuth(scope, msgID, h.owner)
}

// Claude Code answers a 404 thread_not_found by sending thread create with the
// whole conversation. A continuation pinned to a cooling owner must get that 404
// without reaching another account, and the create must then go through the
// soonest-reset choice instead of the cooling owner.
func TestClaudeThreadCoolingOwnerReplaysThroughSoonestReset(t *testing.T) {
	const model = "claude-opus-5-5"
	hub := newClaudeThreadSoonestResetHub(t, model, 0.50)
	session := "session-" + uuid.NewString()

	if got, err := hub.execute(t, model, session, `{"type":"create"}`); err != nil || got != hub.owner {
		t.Fatalf("first create went to %q (err=%v), want the soonest-reset account %q", got, err, hub.owner)
	}
	msgID := "msg_thread_" + uuid.NewString()
	hub.rememberThread(msgID)

	hub.manager.MarkResult(WithSkipPersist(t.Context()), Result{
		AuthID: hub.owner, Provider: "claude", Model: model, RouteModel: model, Success: false,
		Error: &Error{Code: "rate_limit", Message: "weekly limit reached", HTTPStatus: http.StatusTooManyRequests, Retryable: true},
	})
	hub.called = nil

	_, err := hub.execute(t, model, session, `{"type":"continue","previous_message_id":"`+msgID+`"}`)
	if status := statusCodeFromError(err); status != http.StatusNotFound || !clienterror.IsClaudeThreadNotFound(status, err) {
		t.Fatalf("continuation to a cooling owner: status %d err %v, want 404 thread_not_found", status, err)
	}
	if len(hub.called) != 0 {
		t.Fatalf("continuation reached accounts %v, want none", hub.called)
	}

	got, err := hub.execute(t, model, session, `{"type":"create"}`)
	if err != nil {
		t.Fatalf("replayed create error = %v", err)
	}
	if got != hub.soon {
		t.Fatalf("replayed create went to %q, want the next soonest-reset account %q", got, hub.soon)
	}
}

// The pin holds while the owner can serve: an owner past the 0.9 threshold but
// not cooling still gets the continuation. The threshold moves new sessions only.
func TestClaudeThreadOwnerPastThresholdKeepsContinuation(t *testing.T) {
	const model = "claude-opus-5-5"
	hub := newClaudeThreadSoonestResetHub(t, model, 0.95)
	msgID := "msg_thread_" + uuid.NewString()
	hub.rememberThread(msgID)

	got, err := hub.execute(t, model, "session-"+uuid.NewString(), `{"type":"continue","previous_message_id":"`+msgID+`"}`)
	if err != nil || got != hub.owner {
		t.Fatalf("continuation went to %q (err=%v), want the owner %q", got, err, hub.owner)
	}
	if got, err := hub.execute(t, model, "session-"+uuid.NewString(), `{"type":"create"}`); err != nil || got != hub.soon {
		t.Fatalf("new session went to %q (err=%v), want %q below the threshold", got, err, hub.soon)
	}
}
