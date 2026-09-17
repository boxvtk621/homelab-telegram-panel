package configdraft

import (
	"strings"
	"testing"
)

const (
	hostID         = "11111111-1111-4111-8111-111111111111"
	dockerRevision = "22222222-2222-4222-8222-222222222222"
)

func managedDraft() string {
	return `{"schemaVersion":2,"name":"Cursor alpha","engine":"cursor","deployment":{"kind":"managed","hostId":"` + hostID + `","dockerfileRevision":"` + dockerRevision + `","buildContextRevision":"33333333-3333-4333-8333-333333333333","registryCredentialRef":"registry/main"},"profile":{"mode":"universal","basePrompt":"You are an operator.","instructions":["Keep changes bounded."],"mcp":[{"serverId":"homelab","transport":"streamable-http","uri":"mcp://homelab","credentialRef":"mcp/homelab","tools":[{"name":"inventory.read","schemaHash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]}],"access":{"default":"deny","nativeTools":["read"]}}}`
}

func externalDraft() string {
	return `{"schemaVersion":2,"name":"Codex external","engine":"codex","deployment":{"kind":"external","endpointRef":"harness/codex-primary"},"profile":{"mode":"specialized","basePrompt":"","instructions":[],"mcp":[],"access":{"default":"deny","nativeTools":[]}}}`
}

func TestValidCanonicalDraftsArePure(t *testing.T) {
	for _, raw := range []string{managedDraft(), externalDraft()} {
		result := Validate(raw, "FROM scratch\n")
		if !result.Validation.Valid || result.Validation.EffectStatus != "none" || len(result.Validation.Diagnostics) != 0 {
			t.Fatalf("unexpected validation: %#v", result)
		}
	}
}

func TestDiagnosticsRejectDuplicateNullUnknownUnionAndRefs(t *testing.T) {
	tests := []struct{ raw, code, pointer string }{
		{strings.Replace(managedDraft(), `"name":"Cursor alpha"`, `"name":"x","name":"y"`, 1), "duplicate_field", "/name"},
		{strings.Replace(managedDraft(), `"engine":"cursor"`, `"engine":null`, 1), "null_field", "/engine"},
		{strings.Replace(managedDraft(), `"engine":"cursor"`, `"engine":"cursor","surprise":1`, 1), "unknown_field", "/surprise"},
		{strings.Replace(managedDraft(), `"kind":"managed"`, `"kind":"external"`, 1), "unknown_field", "/deployment/hostId"},
		{strings.Replace(managedDraft(), hostID, "bad", 1), "invalid_ref", "/deployment/hostId"},
		{strings.Replace(managedDraft(), `"kind":"managed"`, `"kind":"other"`, 1), "invalid_union", "/deployment/kind"},
	}
	for _, tt := range tests {
		result := Validate(tt.raw, "")
		found := false
		for _, d := range result.Validation.Diagnostics {
			if d.Code == tt.code && d.Pointer == tt.pointer && d.Line > 0 && d.Column > 0 {
				found = true
			}
		}
		if !found {
			t.Errorf("%s at %s not found in %#v", tt.code, tt.pointer, result.Validation.Diagnostics)
		}
	}
}

func TestDiagnosticCoordinatesAndNearestPointers(t *testing.T) {
	raw := "{\n  \"schemaVersion\": 2,\n  \"name\": \"one\",\n  \"na\\u006de\": \"two\"\n}"
	result := Validate(raw, "")
	if d := findDiagnostic(result, "duplicate_field"); d == nil || d.Pointer != "/name" || d.Line != 4 || d.Column != 3 {
		t.Fatalf("escaped duplicate coordinate = %#v", d)
	}

	nested := "{\n  \"schemaVersion\": 2,\n  \"name\": \"x\",\n  \"engine\": \"cursor\",\n  \"deployment\": {\"kind\": \"managed\", \"hostId\": }\n}"
	result = Validate(nested, "")
	if d := findDiagnostic(result, "invalid_json"); d == nil || d.Pointer != "/deployment/hostId" || d.Line != 5 || d.Column != 46 {
		t.Fatalf("nested syntax coordinate = %#v", d)
	}

	eof := "{\n  \"schemaVersion\": 2"
	result = Validate(eof, "")
	if d := findDiagnostic(result, "invalid_json"); d == nil || d.Pointer != "/schemaVersion" || d.Line != 2 || d.Column != 21 {
		t.Fatalf("EOF coordinate = %#v", d)
	}

	trailing := externalDraft() + "\ntrue"
	result = Validate(trailing, "")
	if d := findDiagnostic(result, "invalid_json"); d == nil || d.Pointer != "/profile/access/nativeTools" || d.Line != 2 || d.Column != 1 {
		t.Fatalf("trailing coordinate = %#v", d)
	}

	array := strings.Replace(managedDraft(), `"instructions":["Keep changes bounded."]`, "\"instructions\":[\n      \"first\",\n      null\n    ]", 1)
	result = Validate(array, "")
	if d := findDiagnostic(result, "invalid_text"); d == nil || d.Pointer != "/profile/instructions/1" || d.Line != 3 || d.Column != 7 {
		t.Fatalf("array item coordinate = %#v", d)
	}
}

func TestSecretsAreRejectedWithoutEchoIncludingMalformedInput(t *testing.T) {
	secret := "ghp_abcdefghijklmnopqrstuvwxyz123456"
	for _, input := range []struct{ raw, dockerfile string }{
		{`{"schemaVersion":2,"token":"` + secret, ""},
		{`{"\u0074oken":"fixture-secret-value"`, ""},
		{`{"profile":{"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`, ""},
		{strings.Replace(externalDraft(), `"schemaVersion":2`, `"schemaVersion":2,"\u0074oken":"fixture-secret-value"`, 1), ""},
		{strings.Replace(externalDraft(), `"basePrompt":""`, `"basePrompt":"-----BEGIN \u0052SA PRIVATE KEY-----"`, 1), ""},
		{`{schemaVersion:2, password: "` + secret, ""},
		{externalDraft(), "FROM scratch\nRUN printf '-----BEGIN PRIVATE KEY-----'\n"},
		{externalDraft(), "FROM scratch\nENV API_KEY=" + secret + "\n"},
	} {
		result := Validate(input.raw, input.dockerfile)
		if !HasSecretDiagnostics(result) {
			t.Fatalf("missing secret diagnostic: %#v", result.Validation.Diagnostics)
		}
		for _, d := range result.Validation.Diagnostics {
			if strings.Contains(d.Message+d.Pointer+d.Code, secret) {
				t.Fatal("diagnostic echoed secret value")
			}
		}
	}
}

func TestSizeLimits(t *testing.T) {
	if Validate(strings.Repeat("x", JSONLimit+1), "").Validation.Diagnostics[0].Code != "json_size" {
		t.Fatal("JSON limit not enforced")
	}
	if Validate(externalDraft(), strings.Repeat("x", DockerfileLimit+1)).Validation.Diagnostics[0].Code != "dockerfile_size" {
		t.Fatal("Dockerfile limit not enforced")
	}
}

func findDiagnostic(result Result, code string) *Diagnostic {
	for index := range result.Validation.Diagnostics {
		if result.Validation.Diagnostics[index].Code == code {
			return &result.Validation.Diagnostics[index]
		}
	}
	return nil
}
