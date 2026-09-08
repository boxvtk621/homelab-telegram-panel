package mobilegateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

type commandControllerFake struct {
	calls  int
	err    error
	result mobilecontrollerclient.CreateDialogResult
	input  mobilecontrollerclient.CreateDialogRequest
}

func (fake *commandControllerFake) CreateDialog(_ context.Context, _ string, input mobilecontrollerclient.CreateDialogRequest) (mobilecontrollerclient.CreateDialogResult, error) {
	fake.calls++
	fake.input = input
	return fake.result, fake.err
}

func TestDialogCreateRejectsAmbiguousUntrustedPayloads(t *testing.T) {
	t.Parallel()
	valid := `{"command_id":"018f0c9e-8f4b-4a6b-8c9d-000000000099","expected_collection_version":0}`
	if _, err := decodePublicCreateDialog([]byte(valid)); err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{
		`{}`, `[]`, valid + `{}`, strings.Replace(valid, ":0", ":null", 1),
		strings.Replace(valid, ":0", ":-1", 1), strings.Replace(valid, ":0", ":9007199254740991", 1),
		strings.Replace(valid, `:0}`, `:0,"expected_collection_version":0}`, 1),
		strings.Replace(valid, `:0}`, `:0,"actor_id":"owner"}`, 1),
		strings.Replace(valid, `:0}`, `:0,"client_instance_id":"foreign"}`, 1),
	} {
		if _, err := decodePublicCreateDialog([]byte(payload)); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}

func TestDialogCreateUnknownOutcomeNeverAcknowledged(t *testing.T) {
	t.Parallel()
	fixture := newReadTestFixture(t, validFakeReadController())
	resume := serve(fixture.mux, http.MethodPost, "/api/v1/session/resume", "", map[string]string{
		"Origin": testPublicOrigin, "Cookie": cookieHeader(fixture.session), clientInstanceHeader: "read-client",
	})
	if resume.Code != http.StatusOK {
		t.Fatalf("resume: %s", resume.Body.String())
	}
	controller := &commandControllerFake{err: errors.New("private-provider-error")}
	handler, err := NewDialogCommandsHandler(fixture.handler, controller)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if err := handler.Register(mux); err != nil {
		t.Fatal(err)
	}
	response := serve(mux, http.MethodPost, "/api/v1/dialogs", `{"command_id":"018f0c9e-8f4b-4a6b-8c9d-000000000099","expected_collection_version":0}`, map[string]string{
		"Origin": testPublicOrigin, "Content-Type": "application/json", "Cookie": cookieHeader(fixture.session), CSRFHeaderName: responseCSRF(t, resume),
	})
	if response.Code != http.StatusServiceUnavailable || controller.calls != 1 || controller.input.ClientInstanceID != "read-client" || strings.Contains(response.Body.String(), "private-provider-error") {
		t.Fatalf("uncertain result=%s calls=%d", response.Body.String(), controller.calls)
	}
}

func TestSessionResumeIsOriginAndClientBound(t *testing.T) {
	t.Parallel()
	fixture := newReadTestFixture(t, validFakeReadController())
	for _, headers := range []map[string]string{
		{"Origin": "https://evil.test", "Cookie": cookieHeader(fixture.session), clientInstanceHeader: "read-client"},
		{"Origin": testPublicOrigin, "Cookie": cookieHeader(fixture.session), clientInstanceHeader: "another-client"},
		{"Origin": testPublicOrigin, clientInstanceHeader: "read-client"},
	} {
		response := serve(fixture.mux, http.MethodPost, "/api/v1/session/resume", "", headers)
		if response.Code < 400 {
			t.Fatalf("unsafe resume succeeded: %s", response.Body.String())
		}
	}
	headers := map[string]string{"Origin": testPublicOrigin, "Cookie": cookieHeader(fixture.session), clientInstanceHeader: "read-client"}
	first := serve(fixture.mux, http.MethodPost, "/api/v1/session/resume", "", headers)
	second := serve(fixture.mux, http.MethodPost, "/api/v1/session/resume", "", headers)
	old := responseCSRF(t, first)
	next := responseCSRF(t, second)
	if old == next {
		t.Fatal("CSRF nonce did not rotate")
	}
	if _, err := fixture.auth.sessions.AuthenticateMutation(fixture.session.Value, old); err == nil {
		t.Fatal("old CSRF remains valid")
	}
	if _, err := fixture.auth.sessions.AuthenticateMutation(fixture.session.Value, next); err != nil {
		t.Fatal(err)
	}
	if len(second.Result().Cookies()) != 0 {
		t.Fatal("resume replaced authentication cookie")
	}
}
