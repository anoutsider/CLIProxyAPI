package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	internallogging "github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// Drives the real Manager with session affinity around SoonestResetSelector.
// The accounts start with no data, learn their seven-day windows from response
// headers through MarkResult, and new sessions then follow the reset times and
// the requested model's window.
func TestManagerSoonestResetLearnsWindowsFromResponses(t *testing.T) {
	ctx := internallogging.WithResponseHeadersHolder(context.Background())
	selector := NewSessionAffinitySelector(&SoonestResetSelector{})
	t.Cleanup(selector.Stop)
	manager := NewManager(nil, selector, nil)
	manager.SetRetryConfig(0, 0, 0)

	const opus, fable = "claude-opus-5-5", "claude-fable-5-1"
	lateID, soonID := "a-late-"+t.Name(), "z-soon-"+t.Name()
	windows := map[string]struct {
		used    string
		resetIn time.Duration
	}{
		lateID: {"0.10", 5 * 24 * time.Hour},
		soonID: {"0.60", 1 * 24 * time.Hour},
	}
	for _, id := range []string{lateID, soonID} {
		registry.GetGlobalRegistry().RegisterClient(id, "claude", []*registry.ModelInfo{{ID: opus}, {ID: fable}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
		if _, err := manager.Register(ctx, &Auth{ID: id, Provider: "claude", Status: StatusActive}); err != nil {
			t.Fatal(err)
		}
	}
	manager.RegisterExecutor(&mockCustomErrorExecutor{
		identifier: "claude",
		executeFn: func(ctx context.Context, auth *Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
			window := windows[auth.ID]
			headers := http.Header{}
			headers.Set("Anthropic-Ratelimit-Unified-7d-Utilization", window.used)
			headers.Set("Anthropic-Ratelimit-Unified-7d-Reset", strconv.FormatInt(time.Now().Add(window.resetIn).Unix(), 10))
			internallogging.SetResponseHeaders(ctx, headers)
			return cliproxyexecutor.Response{Payload: []byte(auth.ID)}, nil
		},
	})
	execute := func(session, model string) string {
		t.Helper()
		opts := cliproxyexecutor.Options{Metadata: map[string]any{}, Headers: http.Header{"X-Session-Id": []string{session}}}
		response, err := manager.Execute(ctx, []string{"claude"}, cliproxyexecutor.Request{Model: model}, opts)
		if err != nil {
			t.Fatalf("Execute(%s, %s) error = %v", session, model, err)
		}
		return string(response.Payload)
	}

	// No account has data yet, so the first two sessions rotate and each response
	// teaches the hub one account's window.
	first, second := execute("learn-1", opus), execute("learn-2", opus)
	if first == second {
		t.Fatalf("sessions without data both went to %s, want rotation", first)
	}
	if got := execute("new-opus", opus); got != soonID {
		t.Fatalf("new Opus session went to %s, want %s whose window resets first", got, soonID)
	}

	SetClaudeUsageWindows(soonID, nil, []ScopedWindow{{Name: "Fable", Utilization: 0.95, ResetsAt: time.Now().Add(24 * time.Hour)}})
	t.Cleanup(func() { ClearClaudeScopedWindows(soonID) })
	if got := execute("new-fable", fable); got != lateID {
		t.Fatalf("new Fable session went to %s, want %s: %s's Fable window is full", got, lateID, soonID)
	}
	if got := execute("new-opus-2", opus); got != soonID {
		t.Fatalf("new Opus session went to %s, want %s: a full Fable window must not bench Opus", got, soonID)
	}
	if got := execute("new-opus", opus); got != soonID {
		t.Fatalf("existing session moved to %s, want it to stay bound to %s", got, soonID)
	}
}
