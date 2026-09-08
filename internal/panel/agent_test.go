package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/cursoragent"
	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
)

type syntheticAgent func(context.Context, string, cursoragent.Tool, func(string)) (string, error)

func (f syntheticAgent) Run(c context.Context, p string, t cursoragent.Tool, s func(string)) (string, error) {
	return f(c, p, t, s)
}

const runID = "00000000-0000-4000-8000-000000000001"
const runBody = `{"command_id":"` + runID + `","issue_id":"HL-210","prompt":"Synthetic question","parent_id":"","expected_updated":123}`

func agentUpstream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/issues/HL-210":
		fmt.Fprint(w, `{"id":"2-1","idReadable":"HL-210","updated":123,"project":{"id":"0-1","shortName":"HL"},"summary":"Synthetic issue","description":"Synthetic context","customFields":[]}`)
	case "/api/articles/HL-A-23":
		fmt.Fprint(w, `{"id":"4-23","idReadable":"HL-A-23","updated":123,"project":{"id":"0-1","shortName":"HL"},"summary":"Synthetic instructions","content":"Synthetic instructions only"}`)
	default:
		upstream(w, r)
	}
}
func waitRun(t *testing.T, s *Server, cookie string, terminal string) agentView {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		w := request(s, "GET", "", "/api/v2/agent/runs", cookie, "")
		var out struct{ Runs []agentView }
		if json.Unmarshal(w.Body.Bytes(), &out) != nil {
			t.Fatal(w.Body.String())
		}
		if len(out.Runs) > 0 && out.Runs[0].Status == terminal {
			return out.Runs[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("run never reached", terminal)
	return agentView{}
}

func TestAgentRuntimeUsesYouTrackContextAndRecoversExactCommand(t *testing.T) {
	s := setup(t, agentUpstream)
	var starts atomic.Int32
	s.agent = syntheticAgent(func(ctx context.Context, prompt string, tool cursoragent.Tool, progress func(string)) (string, error) {
		starts.Add(1)
		if !strings.Contains(prompt, "Synthetic context") || !strings.Contains(prompt, "Synthetic instructions only") || strings.Contains(prompt, testToken) {
			t.Error("unsafe or missing context")
		}
		progress("reading_youtrack")
		out, _ := json.Marshal(tool(ctx, "youtrack_issue", json.RawMessage(`{}`)))
		if !strings.Contains(string(out), "HL-210") {
			t.Error("actual YouTrack tool unavailable")
		}
		denied, _ := json.Marshal(tool(ctx, "youtrack_article", json.RawMessage(`{"id":"OTHER-A-23"}`)))
		if !strings.Contains(string(denied), "error") {
			t.Error("foreign project accepted")
		}
		return "Synthetic answer", nil
	})
	id, csrf := login(t, s)
	for i := 0; i < 2; i++ {
		if w := request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	got := waitRun(t, s, id, "finished")
	if got.Result != "Synthetic answer" || starts.Load() != 1 {
		t.Fatal(got, starts.Load())
	}
	if w := request(s, "POST", strings.Replace(runBody, "Synthetic question", "changed", 1), "/api/v2/agent/runs", id, csrf); w.Code != 409 {
		t.Fatal("conflicting replay accepted")
	}
}

func TestAgentCancellationCapacityAndSessionIsolation(t *testing.T) {
	s := setup(t, agentUpstream)
	s.agent = syntheticAgent(func(ctx context.Context, _ string, _ cursoragent.Tool, _ func(string)) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	id, csrf := login(t, s)
	if w := request(s, "POST", runBody, "/api/v2/agent/runs", id, "bad"); w.Code != 403 {
		t.Fatal("CSRF bypass")
	}
	if w := request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	other, foreign, err := s.sessions.create(youtrack.User{ID: "1-2", Login: "different"}, testToken)
	if err != nil {
		t.Fatal(err)
	}
	otherCSRF := foreign.csrf
	if w := request(s, "GET", "", "/api/v2/agent/runs", other, ""); !strings.Contains(w.Body.String(), `"runs":[]`) {
		t.Fatal("session leak")
	}
	if w := request(s, "POST", `{}`, "/api/v2/agent/runs/"+runID+"/cancel", other, otherCSRF); w.Code != 404 {
		t.Fatal("foreign cancel")
	}
	if w := request(s, "POST", strings.Replace(runBody, runID, "00000000-0000-4000-8000-000000000002", 1), "/api/v2/agent/runs", id, csrf); w.Code != 429 {
		t.Fatal("capacity bypass")
	}
	if w := request(s, "POST", `{}`, "/api/v2/agent/runs/"+runID+"/cancel", id, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	waitRun(t, s, id, "cancelled")
}

func TestAgentFailsClosedOnMissingConfigAndStaleContext(t *testing.T) {
	s := setup(t, agentUpstream)
	id, csrf := login(t, s)
	if w := request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf); w.Code != 503 {
		t.Fatal("missing SDK pretended ready")
	}
	s.agent = syntheticAgent(func(context.Context, string, cursoragent.Tool, func(string)) (string, error) {
		t.Error("stale issue dispatched")
		return "", nil
	})
	request(s, "POST", strings.Replace(runBody, ":123", ":124", 1), "/api/v2/agent/runs", id, csrf)
	if run := waitRun(t, s, id, "failed"); run.Error != "issue_changed" {
		t.Fatal(run)
	}
}

// HL-238@3: a web logout revokes access, not the bounded execution grant.
func TestLogoutKeepsAgentAndToolsRunningAndOwnerCanReturn(t *testing.T) {
	s := setup(t, agentUpstream)
	started := make(chan struct{})
	proceed := make(chan struct{})
	s.agent = syntheticAgent(func(ctx context.Context, _ string, tool cursoragent.Tool, _ func(string)) (string, error) {
		close(started)
		select {
		case <-proceed:
		case <-ctx.Done():
			return "", ctx.Err()
		}
		out, _ := json.Marshal(tool(ctx, "youtrack_issue", json.RawMessage(`{}`)))
		if !strings.Contains(string(out), "HL-210") {
			t.Error("run grant revoked by logout")
		}
		return "Completed in background", nil
	})
	id, csrf := login(t, s)
	request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("not started")
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", id, csrf); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "", "/api/v2/agent/runs", id, ""); w.Code != 401 {
		t.Fatal("logged-out browser retained access")
	}
	// Let both the former expiry watcher and history pruning tick at least once.
	time.Sleep(1200 * time.Millisecond)
	again, againCSRF := login(t, s)
	running := waitRun(t, s, again, "running")
	if running.ID != runID {
		t.Fatal("identity lost across login")
	}
	// Same owner's original command is read back, never a second run.
	if w := request(s, "POST", runBody, "/api/v2/agent/runs", again, againCSRF); w.Code != 200 {
		t.Fatal("relogin replay rejected", w.Code)
	}
	close(proceed)
	if result := waitRun(t, s, again, "finished"); result.Result != "Completed in background" {
		t.Fatal(result)
	}
}
func TestLogoutDoesNotFenceHeldBootstrap(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/articles/HL-A-23" {
			close(entered)
			<-release
		}
		agentUpstream(w, r)
	})
	var calls atomic.Int32
	s.agent = syntheticAgent(func(context.Context, string, cursoragent.Tool, func(string)) (string, error) {
		calls.Add(1)
		return "Background answer", nil
	})
	id, csrf := login(t, s)
	request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf)
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("fixture not entered")
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", id, csrf); w.Code != 200 {
		close(release)
		t.Fatal(w.Code)
	}
	close(release)
	again, _ := login(t, s)
	waitRun(t, s, again, "finished")
	if calls.Load() != 1 {
		t.Fatal("logout cancelled accepted work")
	}
}
func TestLogoutDoesNotFenceHeldToolResponse(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	received := make(chan bool, 1)
	s := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/articles/HL-A-24" {
			close(entered)
			<-release
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"4-24","idReadable":"HL-A-24","project":{"id":"0-1","shortName":"HL"},"content":"held permitted context"}`)
			return
		}
		agentUpstream(w, r)
	})
	s.agent = syntheticAgent(func(ctx context.Context, _ string, tool cursoragent.Tool, _ func(string)) (string, error) {
		out, _ := json.Marshal(tool(ctx, "youtrack_article", json.RawMessage(`{"id":"HL-A-24"}`)))
		received <- strings.Contains(string(out), "held permitted context")
		return "Background answer", nil
	})
	id, csrf := login(t, s)
	request(s, "POST", runBody, "/api/v2/agent/runs", id, csrf)
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("fixture not entered")
	}
	if w := request(s, "POST", `{}`, "/api/v2/logout", id, csrf); w.Code != 200 {
		close(release)
		t.Fatal(w.Code)
	}
	close(release)
	select {
	case ok := <-received:
		if !ok {
			t.Fatal("bounded run lost its grant")
		}
	case <-time.After(time.Second):
		t.Fatal("tool stopped")
	}
	again, _ := login(t, s)
	waitRun(t, s, again, "finished")
}
