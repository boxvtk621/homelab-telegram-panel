package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/httpapi"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestReadPrivateBoundedRequiresOwnedRegular0600File(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "signing-key.pem")
	if err := os.WriteFile(path, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if value, err := readPrivateBounded(path, 16); err != nil || string(value) != "private" {
		t.Fatalf("value=%q err=%v", value, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateBounded(path, 16); err == nil {
		t.Fatal("world-readable private key accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "signing-key-link.pem")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateBounded(link, 16); err == nil {
		t.Fatal("symlinked private key accepted")
	}
}

func TestCommandMigrationImportAndUDSService(t *testing.T) {
	databaseURL := os.Getenv("AGENT_SERVICE_TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("AGENT_SERVICE_TEST_DATABASE_URL is not set")
	}
	directory, err := os.MkdirTemp("/private/tmp", "hl282-agent-service-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "agent-service.sock")
	registryPath, signerPath, snapshotPath, owner := commandFixtures(t, directory)
	environment := map[string]string{
		"AGENT_SERVICE_DATABASE_URL":      databaseURL,
		"AGENT_SERVICE_SOCKET":            socket,
		"AGENT_SERVICE_REGISTRY":          registryPath,
		"AGENT_SERVICE_SIGNER_PUBLIC_KEY": signerPath,
		"AGENT_SERVICE_IMPORT_SNAPSHOT":   snapshotPath,
		"AGENT_SERVICE_WORKER_TOKEN":      "test-worker-token-0000000000000001",
	}
	lookup := func(key string) (string, bool) { value, ok := environment[key]; return value, ok }

	var output bytes.Buffer
	if code := execute(context.Background(), []string{"migrate"}, lookup, &output); code != 0 || output.String() != "MIGRATION_OK\n" {
		t.Fatalf("migrate code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute(context.Background(), []string{"import"}, lookup, &output); code != 0 ||
		output.String() != "IMPORT_OK nodes=1 dialogs=1 created=1\n" {
		t.Fatalf("import code=%d output=%q", code, output.String())
	}
	output.Reset()
	if code := execute(context.Background(), []string{"import"}, lookup, &output); code != 0 ||
		output.String() != "IMPORT_OK nodes=1 dialogs=1 created=0\n" {
		t.Fatalf("reimport code=%d output=%q", code, output.String())
	}

	first := runService(t, lookup, socket, owner)
	second := runService(t, lookup, socket, owner)
	if len(first.inventory.Items) != 1 || len(second.inventory.Items) != 1 ||
		first.inventory.Items[0].DialogCount != 1 || second.inventory.Items[0].DialogCount != 1 ||
		len(first.bindings.Items) != 1 || len(second.bindings.Items) != 1 ||
		first.bindings.Items[0].LogicalDialogID != second.bindings.Items[0].LogicalDialogID ||
		first.bindings.Items[0].BindingVersion != 1 || second.bindings.Items[0].BindingVersion != 1 {
		t.Fatalf("identity changed across service restart: first=%+v second=%+v", first, second)
	}
}

func TestServiceListenerRecoversCrashLeftSocketAndKeepsSingleton(t *testing.T) {
	directory, err := os.MkdirTemp("/private/tmp", "hl283-agent-service-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "agent-service.sock")
	crashed, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	crashed.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatal("test did not leave a stale socket", err)
	}

	listener, lock, err := openServiceListener(socket)
	if err != nil {
		t.Fatal("stale socket was not recovered", err)
	}
	defer func() {
		_ = listener.Close()
		lock.removeSocket()
		lock.close()
	}()
	if second, secondLock, err := openServiceListener(socket); err == nil {
		_ = second.Close()
		secondLock.close()
		t.Fatal("second service acquired the same socket")
	}
}

func TestServiceListenerCleanupDoesNotRemoveReplacementPath(t *testing.T) {
	directory, err := os.MkdirTemp("/private/tmp", "hl283-agent-service-socket-cleanup-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "agent-service.sock")
	listener, lock, err := openServiceListener(socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(socket); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	lock.removeSocket()
	lock.close()
	contents, err := os.ReadFile(socket)
	if err != nil || string(contents) != "replacement" {
		t.Fatalf("replacement path was removed or changed: contents=%q err=%v", contents, err)
	}
}

type serviceReadback struct {
	inventory model.InventoryPage
	bindings  model.DialogBindingPage
}

func runService(t *testing.T, lookup func(string) (string, bool), socket, owner string) serviceReadback {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- execute(ctx, []string{"serve"}, lookup, io.Discard) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if info, err := os.Stat(socket); err == nil {
			if info.Mode().Perm() != 0o600 {
				cancel()
				t.Fatalf("socket mode=%o", info.Mode().Perm())
			}
			break
		}
		select {
		case code := <-done:
			cancel()
			t.Fatalf("service exited before socket became ready: %d", code)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("service socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	read := func(path string, target any) {
		request, _ := http.NewRequest(http.MethodGet, "http://agent-service"+path, nil)
		request.Header.Set(httpapi.OwnerHeader, owner)
		response, err := client.Do(request)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(target) != nil {
			cancel()
			t.Fatalf("readback path=%s status=%d", path, response.StatusCode)
		}
	}
	var result serviceReadback
	read("/internal/v1/inventory?limit=100", &result.inventory)
	read("/internal/v1/dialog-bindings?nodeId="+result.inventory.Items[0].NodeID+"&limit=100", &result.bindings)
	transport.CloseIdleConnections()
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("service shutdown code=%d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("service did not stop")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatal("service socket remained after shutdown")
	}
	return result
}

func commandFixtures(t *testing.T, directory string) (string, string, string, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	owner := "hl282-cli-" + strings.ReplaceAll(time.Now().UTC().Format("20060102150405.000000000"), ".", "-")
	pin := sha256.Sum256([]byte(owner))
	manifest := model.RegistryManifest{
		RegistryVersion: 1, OwnerID: owner, Mode: "fixture",
		Nodes: []model.RegistryNode{{
			NodeID: "21000000-0000-4000-8000-000000000001", Name: "CLI Agent", Adapter: "codex",
			URL: "https://127.0.0.1:9443", CertificateSHA256: hex.EncodeToString(pin[:]),
		}},
	}
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := json.Marshal(struct {
		Manifest  model.RegistryManifest `json:"manifest"`
		Signature string                 `json:"signature"`
	}{Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(private, canonical))})
	if err != nil {
		t.Fatal(err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	signer := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER})
	snapshot, err := json.Marshal(model.ImportSnapshot{
		SchemaID: model.ImportSchemaID, Complete: true,
		Hosts: []model.HostSeed{{HostID: "11000000-0000-4000-8000-000000000001", Name: "Local Mac"}},
		Nodes: []model.NodeSeed{{
			NodeID: "21000000-0000-4000-8000-000000000001", HostID: "11000000-0000-4000-8000-000000000001",
			RegistrationMode: "compatible", Dialogs: []model.DialogSeed{{NodeDialogID: "31000000-0000-4000-8000-000000000001"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	registryPath := filepath.Join(directory, "registry.json")
	signerPath := filepath.Join(directory, "signer.pem")
	snapshotPath := filepath.Join(directory, "snapshot.json")
	for path, value := range map[string][]byte{registryPath: envelope, signerPath: signer, snapshotPath: snapshot} {
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return registryPath, signerPath, snapshotPath, owner
}
