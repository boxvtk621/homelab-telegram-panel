package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
)

func TestJournalInitializationIsExplicitAndOneTime(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DOCKER_ADAPTER_STATE_DIR":   directory,
		"DOCKER_ADAPTER_DAEMON_ID":   "fixture-daemon",
		"DOCKER_ADAPTER_INSTANCE_ID": "fixture-instance",
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	var output bytes.Buffer
	if code := execute([]string{"journal-check"}, lookup, &output); code != 1 || output.String() != "JOURNAL_UNAVAILABLE\n" {
		t.Fatalf("uninitialized check code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-init"}, lookup, &output); code != 0 || output.String() != "JOURNAL_INITIALIZED\n" {
		t.Fatalf("init code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-init"}, lookup, &output); code != 1 || output.String() != "JOURNAL_INITIALIZATION_REJECTED\n" {
		t.Fatalf("reinitialize code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute([]string{"journal-check"}, lookup, &output); code != 0 || output.String() != "JOURNAL_OK\n" {
		t.Fatalf("reopen code=%d output=%q", code, output.String())
	}
}

func TestHostProbeCommandUsesAdapterOnlyLocalSocket(t *testing.T) {
	directory, err := os.MkdirTemp("/private/tmp", "hl290-adapter-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		calls = append(calls, request.Method+" "+request.URL.Path)
		mu.Unlock()
		switch request.URL.Path {
		case "/_ping":
			_, _ = w.Write([]byte("OK"))
		case "/version":
			_, _ = w.Write([]byte(`{"ApiVersion":"1.52","Version":"29.8.0","Os":"linux","Arch":"arm64"}`))
		case "/info":
			_, _ = w.Write([]byte(`{"ID":"local-daemon","OSType":"linux","Architecture":"arm64","OperatingSystem":"Docker Desktop","ServerVersion":"29.8.0"}`))
		default:
			http.Error(w, "forbidden", http.StatusForbidden)
		}
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	targetsPath := filepath.Join(directory, "targets.json")
	targets := map[string]any{
		"schemaId": "docker-adapter-targets-v1",
		"owners": map[string]any{"owner-1": map[string]any{"local-desktop": dockeradapter.Target{
			Ref: "local-desktop", Revision: 1, Transport: "local", LocalSocket: socket, DockerContext: "desktop-linux",
		}}},
	}
	rawTargets, _ := json.Marshal(targets)
	if err := os.WriteFile(targetsPath, rawTargets, 0o600); err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(directory, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x42}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	secretDirectory := filepath.Join(directory, "secrets")
	if err := os.Mkdir(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DOCKER_ADAPTER_OWNER": "owner-1", "DOCKER_ADAPTER_TARGETS": targetsPath,
		"DOCKER_ADAPTER_SECRET_DIR": secretDirectory, "DOCKER_ADAPTER_MASTER_KEY_FILE": keyPath,
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	descriptor := dockeradapter.HostDescriptor{
		SchemaID: dockeradapter.HostDescriptorSchemaID, HostID: "10000000-0000-4000-8000-000000000001",
		HostVersion: 1, DisplayName: "Desktop", Transport: "local", TargetRef: "local-desktop",
		DockerContextRef: "desktop-linux", HostPlatform: "darwin", HostArchitecture: "arm64",
	}
	rawDescriptor, _ := json.Marshal(descriptor)
	var output bytes.Buffer
	if code := executeIO([]string{"host-probe"}, lookup, bytes.NewReader(rawDescriptor), &output); code != 0 {
		t.Fatalf("probe code=%d output=%s", code, output.String())
	}
	var observation dockeradapter.HostObservation
	if json.Unmarshal(output.Bytes(), &observation) != nil || observation.Availability != "ready" || observation.DaemonID != "local-daemon" {
		t.Fatalf("unexpected observation: %s", output.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"GET /_ping", "GET /version", "GET /info"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("probe made non-read calls: got=%v want=%v", calls, want)
	}
}

func TestSecretPutCommandReturnsOnlyOpaqueReference(t *testing.T) {
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "master.key")
	if err := os.WriteFile(keyPath, bytes.Repeat([]byte{0x37}, 32), 0o600); err != nil {
		t.Fatal(err)
	}
	secretDirectory := filepath.Join(directory, "secrets")
	if err := os.Mkdir(secretDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{
		"DOCKER_ADAPTER_OWNER": "owner-1", "DOCKER_ADAPTER_SECRET_KIND": "ssh",
		"DOCKER_ADAPTER_OPERATION_ID": "30000000-0000-4000-8000-000000000001",
		"DOCKER_ADAPTER_SECRET_DIR":   secretDirectory, "DOCKER_ADAPTER_MASTER_KEY_FILE": keyPath,
	}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }
	privateKey := []byte("PRIVATE-KEY-SENTINEL")
	passphrase := []byte("PASSPHRASE-SENTINEL")
	raw, _ := json.Marshal(map[string][]byte{"privateKey": privateKey, "passphrase": passphrase})
	var output bytes.Buffer
	if code := executeIO([]string{"secret-put"}, lookup, bytes.NewReader(raw), &output); code != 0 {
		t.Fatalf("secret put code=%d output=%s", code, output.String())
	}
	if bytes.Contains(output.Bytes(), privateKey) || bytes.Contains(output.Bytes(), passphrase) {
		t.Fatal("secret input echoed")
	}
	var response map[string]string
	if json.Unmarshal(output.Bytes(), &response) != nil || response["credentialRef"] == "" ||
		response["operationId"] != values["DOCKER_ADAPTER_OPERATION_ID"] || response["status"] != "provisioned" {
		t.Fatalf("invalid reference response: %s", output.String())
	}
	store, err := dockeradapter.NewSecretStore(secretDirectory, bytes.Repeat([]byte{0x37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolved, err := store.ResolveSSH(context.Background(), "owner-1", response["credentialRef"])
	if err != nil || string(resolved.PrivateKey) != string(privateKey) || string(resolved.Passphrase) != string(passphrase) {
		t.Fatal("stored credential unavailable", err)
	}
}

func TestUnknownDBStateUsesOnlyExactAcknowledgedReplayAsReconciled(t *testing.T) {
	for name, fixture := range map[string]struct {
		claimed   string
		execution dockeradapter.Execution
		want      string
		known     bool
	}{
		"fresh acknowledged":          {claimed: "not_sent", execution: dockeradapter.Execution{JournalState: "acknowledged"}, want: "acknowledged", known: true},
		"unknown acknowledged replay": {claimed: "unknown", execution: dockeradapter.Execution{JournalState: "acknowledged", Replayed: true}, want: "reconciled", known: true},
		"unknown sent replay":         {claimed: "unknown", execution: dockeradapter.Execution{JournalState: "sent", Replayed: true}, want: "sent", known: false},
	} {
		t.Run(name, func(t *testing.T) {
			got, known := completionEffectState(fixture.claimed, fixture.execution)
			if got != fixture.want || known != fixture.known {
				t.Fatalf("state=%s known=%v", got, known)
			}
		})
	}
}

func TestClaimedUnknownWithoutExactJournalNeverCallsApply(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	executor, err := dockeradapter.Initialize(directory, "fixture-daemon", "fixture-instance")
	if err != nil {
		t.Fatal(err)
	}
	defer executor.Close()
	request := dockeradapter.Request{
		SchemaID: dockeradapter.RequestSchemaID, OperationID: "op-unknown-missing", StepID: "apply-1",
		Generation: 1, Action: "fixture.apply", ResourceIDs: []string{"container:agent-1"},
	}
	proof := dockeradapter.AuthorityProof{
		OperationID: request.OperationID, Generation: request.Generation, WorkerID: "worker-1",
		WorkerToken: "worker-token-1", OperationVersion: 1,
	}
	authority := dockeradapter.NewFixtureAuthority()
	if err := authority.Grant("fixture-daemon", "fixture-instance", request, proof); err != nil {
		t.Fatal(err)
	}
	backend := dockeradapter.NewFixtureBackend()
	if _, err := executeClaimedWork(context.Background(), executor, authority, proof, backend, request, "unknown"); !errors.Is(err, dockeradapter.ErrReconciliationRequired) {
		t.Fatal("unknown work without exact journal was not blocked", err)
	}
	if backend.ApplyCalls() != 0 || len(executor.Snapshot()) != 0 {
		t.Fatalf("unknown work crossed effect boundary: applyCalls=%d journal=%+v", backend.ApplyCalls(), executor.Snapshot())
	}
}
