package dockeradapter

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const testHostID = "10000000-0000-4000-8000-000000000001"

type fixtureConnector struct {
	identity ConnectionIdentity
	session  *fixtureEngineSession
	fault    *probeFault
}

func (f fixtureConnector) Open(_ context.Context, _ string, _ HostDescriptor) (EngineSession, ConnectionIdentity, *probeFault) {
	return f.session, f.identity, f.fault
}

type fixtureEngineSession struct {
	status map[string]int
	body   map[string]string
	calls  []string
}

func (f *fixtureEngineSession) Do(request *http.Request) (*http.Response, error) {
	f.calls = append(f.calls, request.Method+" "+request.URL.Path)
	status := f.status[request.URL.Path]
	if status == 0 {
		status = http.StatusOK
	}
	body, ok := f.body[request.URL.Path]
	if !ok {
		return nil, errors.New("unexpected Docker API path")
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func (f *fixtureEngineSession) Close() error { return nil }

func localDescriptor() HostDescriptor {
	return HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: testHostID, HostVersion: 2,
		DisplayName: "Desktop", Transport: "local", TargetRef: "local-desktop",
		DockerContextRef: "desktop-linux", HostPlatform: "darwin", HostArchitecture: "arm64",
	}
}

func readyFixture() *fixtureEngineSession {
	return &fixtureEngineSession{status: map[string]int{}, body: map[string]string{
		"/_ping":   "OK",
		"/version": `{"ApiVersion":"1.52","MinAPIVersion":"1.24","Version":"29.8.0","Os":"linux","Arch":"arm64"}`,
		"/info":    `{"ID":"daemon-one","OSType":"linux","Architecture":"aarch64","OperatingSystem":"Docker Desktop","ServerVersion":"29.8.0"}`,
	}}
}

func TestProbeUsesOnlyReadOnlyDockerEndpointsAndPinsIdentity(t *testing.T) {
	session := readyFixture()
	descriptor := localDescriptor()
	prober := Prober{
		Connector: fixtureConnector{session: session, identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint-one"}},
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	observation := prober.Probe(context.Background(), "owner-1", descriptor)
	if observation.Availability != "ready" || observation.IdentitySHA256 == "" || observation.DaemonID != "daemon-one" {
		t.Fatalf("unexpected observation: %+v", observation)
	}
	wantCalls := []string{"GET /_ping", "GET /version", "GET /info"}
	if !reflect.DeepEqual(session.calls, wantCalls) {
		t.Fatalf("probe crossed read-only allowlist: got %v want %v", session.calls, wantCalls)
	}
	if !PlanIdentityValid(observation.IdentitySHA256, observation) {
		t.Fatal("current identity did not validate")
	}
	newVersion := descriptor
	newVersion.HostVersion++
	reprobed := (Prober{Connector: fixtureConnector{session: readyFixture(), identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint-one"}}}).Probe(context.Background(), "owner-1", newVersion)
	if reprobed.IdentitySHA256 != observation.IdentitySHA256 {
		t.Fatalf("host version changed physical identity: old=%s new=%s", observation.IdentitySHA256, reprobed.IdentitySHA256)
	}

	second := readyFixture()
	descriptor.ExpectedIdentitySHA256 = strings.Repeat("a", 64)
	changed := (Prober{Connector: fixtureConnector{session: second, identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint-two"}}}).Probe(context.Background(), "owner-1", descriptor)
	if changed.FailureCode != "host_identity_changed" || changed.Availability != "unavailable" || PlanIdentityValid(observation.IdentitySHA256, changed) {
		t.Fatalf("changed endpoint retained old plan: %+v", changed)
	}
}

func TestProbeReportsDistinctDockerAndPlatformFailures(t *testing.T) {
	if fault := engineRequestFault(context.Background(), &os.PathError{Op: "connect", Path: "/private/docker.sock", Err: os.ErrPermission}); fault.code != "docker_permission_denied" {
		t.Fatalf("local socket permission collapsed: %+v", fault)
	}
	if !errors.Is(classifyDockerStdioFailure([]byte("dial unix /var/run/docker.sock: permission denied"), io.EOF), errDockerPermission) ||
		engineRequestFault(context.Background(), errDockerPermission).code != "docker_permission_denied" {
		t.Fatal("remote Docker permission failure collapsed into transport failure")
	}
	permission := readyFixture()
	permission.status["/_ping"] = http.StatusForbidden
	result := (Prober{Connector: fixtureConnector{session: permission}}).Probe(context.Background(), "owner-1", localDescriptor())
	if result.FailureCode != "docker_permission_denied" || result.FailureStage != "daemon_ping" {
		t.Fatalf("permission failure collapsed: %+v", result)
	}

	windowsContainers := readyFixture()
	windowsContainers.body["/info"] = `{"ID":"daemon-two","OSType":"windows","Architecture":"amd64","OperatingSystem":"Docker Desktop","ServerVersion":"29.8.0"}`
	descriptor := localDescriptor()
	descriptor.HostPlatform = "windows"
	descriptor.HostArchitecture = "amd64"
	result = (Prober{Connector: fixtureConnector{session: windowsContainers}}).Probe(context.Background(), "owner-1", descriptor)
	if result.FailureCode != "platform_incompatible" || result.FailureStage != "target_platform" {
		t.Fatalf("windows containers accepted: %+v", result)
	}
	unknownArchitecture := readyFixture()
	unknownArchitecture.body["/info"] = `{"ID":"daemon-three","OSType":"linux","Architecture":"riscv64","OperatingSystem":"Linux","ServerVersion":"29.8.0"}`
	result = (Prober{Connector: fixtureConnector{session: unknownArchitecture}}).Probe(context.Background(), "owner-1", localDescriptor())
	if result.FailureCode != "platform_unverified" || result.FailureStage != "target_platform" || result.Architecture != "riscv64" {
		t.Fatalf("unknown architecture was not explicitly marked unverified: %+v", result)
	}

	oldEngine := readyFixture()
	oldEngine.body["/version"] = `{"ApiVersion":"1.47","Version":"27.5.1","Os":"linux","Arch":"arm64"}`
	result = (Prober{Connector: fixtureConnector{session: oldEngine}}).Probe(context.Background(), "owner-1", localDescriptor())
	if result.FailureCode != "engine_version_unsupported" {
		t.Fatalf("old engine accepted: %+v", result)
	}

	oversized := readyFixture()
	oversized.body["/info"] = `{"ID":"` + strings.Repeat("d", 129) + `","OSType":"linux","Architecture":"arm64","OperatingSystem":"Docker Desktop","ServerVersion":"29.8.0"}`
	result = (Prober{Connector: fixtureConnector{session: oversized, identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint"}}}).Probe(context.Background(), "owner-1", localDescriptor())
	if result.FailureCode != "docker_response_invalid" || result.DaemonID != "" || result.APIVersion != "1.52" {
		t.Fatalf("oversized daemon field escaped or lost concrete reason: %+v", result)
	}
}

type registryProbeFixture struct{ fault *probeFault }

func (f registryProbeFixture) Check(context.Context, string, string) *probeFault { return f.fault }

func TestProbeKeepsRegistryCredentialsSeparateFromDockerPermissions(t *testing.T) {
	descriptor := localDescriptor()
	descriptor.RegistryCredentialRef = "cred_registry"
	result := (Prober{
		Connector: fixtureConnector{session: readyFixture(), identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint"}},
		Registry: registryProbeFixture{fault: &probeFault{
			stage: "registry_auth", code: "registry_credential_unavailable", next: "Store registry credentials again.",
		}},
	}).Probe(context.Background(), "owner-1", descriptor)
	if result.Availability != "unavailable" || result.FailureStage != "registry_auth" ||
		result.FailureCode != "registry_credential_unavailable" || result.RegistryAvailability != "unavailable" ||
		result.IdentitySHA256 != "" {
		t.Fatalf("registry failure collapsed into daemon access: %+v", result)
	}

	result = (Prober{
		Connector: fixtureConnector{session: readyFixture(), identity: ConnectionIdentity{ContextEndpoint: "sha256:endpoint"}},
		Registry:  registryProbeFixture{},
	}).Probe(context.Background(), "owner-1", descriptor)
	if result.Availability != "ready" || result.RegistryAvailability != "not_checked" {
		t.Fatalf("stored credentials overstated registry authentication: %+v", result)
	}
}

func TestSSHDescriptorRequiresPinBeforeCredentialResolution(t *testing.T) {
	if validateRemoteExecutable("docker", "posix") == nil || validateRemoteExecutable("docker.exe", "powershell") == nil {
		t.Fatal("remote Docker executable accepted from ambient PATH")
	}
	if !remoteExecutableMissing([]byte("The term is not recognized as the name of a cmdlet"), errors.New("exit status 1")) {
		t.Fatal("missing PowerShell Docker executable was not classified")
	}
	descriptor := localDescriptor()
	descriptor.Transport = "ssh"
	descriptor.TargetRef = "ssh-host"
	descriptor.CredentialRef = "cred_key"
	if err := ValidateHostDescriptor(descriptor); err != nil {
		t.Fatal(err)
	}
	targets := staticTargetResolver{target: Target{
		Ref: "ssh-host", Revision: 1, Transport: "ssh", SSHAddress: "host.example:22", SSHUser: "operator",
		DockerExecutable: "/usr/bin/docker", DockerContext: "desktop-linux", RemoteShell: "posix",
		HostKeySHA256: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}}
	credentials := &countingCredentialResolver{}
	_, _, fault := (Connector{Targets: targets, Credentials: credentials}).Open(context.Background(), "owner-1", descriptor)
	if fault == nil || fault.code != "ssh_host_key_unknown" || credentials.calls != 0 {
		t.Fatalf("unknown key reached credential resolution: fault=%+v calls=%d", fault, credentials.calls)
	}
	descriptor.ExpectedHostKey = "SHA256:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
	_, _, fault = (Connector{Targets: targets, Credentials: credentials}).Open(context.Background(), "owner-1", descriptor)
	if fault == nil || fault.code != "ssh_host_key_changed" || credentials.calls != 0 {
		t.Fatalf("changed key reached credential resolution: fault=%+v calls=%d", fault, credentials.calls)
	}
}

func TestSSHPrivateKeyFailuresRemainDistinct(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(privateKey, "r07", []byte("correct-passphrase"))
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(block)
	if _, fault := parseSSHSigner(SSHCredential{PrivateKey: append([]byte(nil), encoded...)}); fault == nil || fault.code != "ssh_passphrase_required" {
		t.Fatalf("missing passphrase collapsed: %+v", fault)
	}
	if _, fault := parseSSHSigner(SSHCredential{PrivateKey: append([]byte(nil), encoded...), Passphrase: []byte("wrong")}); fault == nil || fault.code != "ssh_passphrase_invalid" {
		t.Fatalf("wrong passphrase collapsed: %+v", fault)
	}
	if _, fault := parseSSHSigner(SSHCredential{PrivateKey: append([]byte(nil), encoded...), Passphrase: []byte("correct-passphrase")}); fault != nil {
		t.Fatalf("valid passphrase rejected: %+v", fault)
	}
}

type fixedCredentialResolver struct{ credential SSHCredential }

func (f fixedCredentialResolver) ResolveSSH(context.Context, string, string) (SSHCredential, error) {
	return SSHCredential{
		PrivateKey: append([]byte(nil), f.credential.PrivateKey...),
		Passphrase: append([]byte(nil), f.credential.Passphrase...),
	}, nil
}

func TestObservedSSHHostKeyStopsBeforeUserAuthentication(t *testing.T) {
	_, serverPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serverSigner, err := ssh.NewSignerFromKey(serverPrivate)
	if err != nil {
		t.Fatal(err)
	}
	_, expectedPrivate, _ := ed25519.GenerateKey(rand.Reader)
	expectedSigner, _ := ssh.NewSignerFromKey(expectedPrivate)
	expectedFingerprint := ssh.FingerprintSHA256(expectedSigner.PublicKey())
	_, clientPrivate, _ := ed25519.GenerateKey(rand.Reader)
	clientBlock, err := ssh.MarshalPrivateKey(clientPrivate, "r07-client")
	if err != nil {
		t.Fatal(err)
	}
	clientPEM := pem.EncodeToMemory(clientBlock)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var authenticationCalls atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			authenticationCalls.Add(1)
			return nil, nil
		}}
		serverConfig.AddHostKey(serverSigner)
		_, _, _, _ = ssh.NewServerConn(connection, serverConfig)
	}()
	descriptor := localDescriptor()
	descriptor.Transport = "ssh"
	descriptor.TargetRef = "ssh-host"
	descriptor.CredentialRef = "cred_key"
	descriptor.ExpectedHostKey = expectedFingerprint
	descriptor.DockerContextRef = "default"
	targets := staticTargetResolver{target: Target{
		Ref: "ssh-host", Revision: 1, Transport: "ssh", SSHAddress: listener.Addr().String(), SSHUser: "operator",
		DockerExecutable: "/usr/bin/docker", DockerContext: "default", RemoteShell: "posix", HostKeySHA256: expectedFingerprint,
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, fault := (Connector{Targets: targets, Credentials: fixedCredentialResolver{credential: SSHCredential{PrivateKey: clientPEM}}}).Open(ctx, "owner-1", descriptor)
	<-done
	if fault == nil || fault.code != "ssh_host_key_changed" || authenticationCalls.Load() != 0 {
		t.Fatalf("fault=%+v authenticationCalls=%d", fault, authenticationCalls.Load())
	}
}

type staticTargetResolver struct{ target Target }

func (s staticTargetResolver) ResolveTarget(_ context.Context, _, _ string) (Target, error) {
	return s.target, nil
}

type countingCredentialResolver struct{ calls int }

func (c *countingCredentialResolver) ResolveSSH(context.Context, string, string) (SSHCredential, error) {
	c.calls++
	return SSHCredential{}, ErrSecretUnavailable
}

func TestRemoteTargetRejectsCommandAndContextInjection(t *testing.T) {
	base := Target{
		Ref: "ssh-host", Revision: 1, Transport: "ssh", SSHAddress: "host.example:22", SSHUser: "operator",
		DockerExecutable: "/usr/bin/docker", DockerContext: "desktop-linux", RemoteShell: "posix",
		HostKeySHA256: "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
	}
	for name, mutate := range map[string]func(*Target){
		"address option":  func(value *Target) { value.SSHAddress = "-oProxyCommand=evil:22" },
		"user newline":    func(value *Target) { value.SSHUser = "operator\nevil" },
		"command newline": func(value *Target) { value.DockerExecutable = "/usr/bin/docker\nwhoami" },
		"unknown shell":   func(value *Target) { value.RemoteShell = "cmd" },
		"context shell":   func(value *Target) { value.DockerContext = "ctx;whoami" },
	} {
		t.Run(name, func(t *testing.T) {
			value := base
			mutate(&value)
			if validateTarget(value) == nil {
				t.Fatalf("unsafe target accepted: %+v", value)
			}
		})
	}
}

func TestRemoteContextCannotRedirectToThirdMachine(t *testing.T) {
	for _, endpoint := range []string{`"ssh://third.example"`, `"tcp://third.example:2376"`, `"http://third.example"`} {
		if _, fault := validateRemoteContextEndpoint(endpoint); fault == nil || fault.code != "docker_context_redirected" {
			t.Fatalf("redirected context accepted: endpoint=%s fault=%+v", endpoint, fault)
		}
	}
	for _, endpoint := range []string{`"unix:///var/run/docker.sock"`, `"npipe:////./pipe/docker_engine"`} {
		if got, fault := validateRemoteContextEndpoint(endpoint); fault != nil || got == "" {
			t.Fatalf("local daemon context rejected: endpoint=%s got=%q fault=%+v", endpoint, got, fault)
		}
	}
}

func TestSecretStoreIsEncryptedWriteOnlyAndOwnerScoped(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{0x42}, 32)
	store, err := NewSecretStore(directory, key)
	if err != nil {
		t.Fatal(err)
	}
	operationID := "30000000-0000-4000-8000-000000000001"
	privateKey := []byte("PRIVATE-KEY-SENTINEL")
	passphrase := []byte("PASSPHRASE-SENTINEL")
	provision, err := store.ProvisionSSH(context.Background(), "owner-1", operationID, privateKey, passphrase)
	if err != nil || !provision.Created || !strings.HasPrefix(provision.CredentialRef, "cred_") {
		t.Fatal(provision, err)
	}
	ref := provision.CredentialRef
	onDisk, err := os.ReadFile(filepath.Join(directory, ref+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, privateKey) || bytes.Contains(onDisk, passphrase) || bytes.Contains(onDisk, []byte("owner-1")) {
		t.Fatal("secret or owner leaked to encrypted file")
	}
	replay, err := store.ProvisionSSH(context.Background(), "owner-1", operationID, privateKey, passphrase)
	if err != nil || replay.Created || replay.CredentialRef != ref {
		t.Fatalf("exact replay was not idempotent: provision=%+v err=%v", replay, err)
	}
	if _, err := store.ProvisionSSH(context.Background(), "owner-1", operationID, []byte("different"), passphrase); !errors.Is(err, ErrSecretConflict) {
		t.Fatal("different replay did not conflict", err)
	}
	status, err := store.ProvisionStatus(context.Background(), "owner-1", operationID)
	if err != nil || status.CredentialRef != ref || status.Kind != "ssh" || status.Status != "provisioned" {
		t.Fatalf("invalid provisioning status: %+v err=%v", status, err)
	}
	if _, err := store.ProvisionStatus(context.Background(), "owner-2", operationID); !errors.Is(err, ErrSecretNotFound) {
		t.Fatal("foreign owner observed provisioning operation", err)
	}
	if second, err := NewSecretStore(directory, key); err == nil {
		second.Close()
		t.Fatal("second active credential store acquired directory")
	}
	store.Close()
	store, err = NewSecretStore(directory, key)
	if err != nil {
		t.Fatal("reopen credential store", err)
	}
	defer store.Close()
	status, err = store.ProvisionStatus(context.Background(), "owner-1", operationID)
	if err != nil || status.CredentialRef != ref {
		t.Fatalf("lost-ACK status was not durable: %+v err=%v", status, err)
	}
	resolved, err := store.ResolveSSH(context.Background(), "owner-1", ref)
	if err != nil || string(resolved.PrivateKey) != string(privateKey) || string(resolved.Passphrase) != string(passphrase) {
		t.Fatal("secret did not round trip", err)
	}
	zeroBytes(resolved.PrivateKey)
	zeroBytes(resolved.Passphrase)
	if _, err := store.ResolveSSH(context.Background(), "owner-2", ref); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatal("foreign owner resolved credential", err)
	}
	public, _ := json.Marshal(map[string]string{"credentialRef": ref})
	if bytes.Contains(public, privateKey) || bytes.Contains(public, passphrase) {
		t.Fatal("write-only response leaked secret")
	}
}

func TestSecretStoreConcurrentDifferentReplayNeverOverwrites(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSecretStore(directory, bytes.Repeat([]byte{0x24}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "30000000-0000-4000-8000-000000000002"
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, payload := range [][]byte{[]byte("registry-a"), []byte("registry-b")} {
		payload := append([]byte(nil), payload...)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := store.ProvisionRegistry(context.Background(), "owner-1", operationID, payload)
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, ErrSecretConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent provision error: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	status, err := store.ProvisionStatus(context.Background(), "owner-1", operationID)
	if err != nil || status.CredentialRef == "" {
		t.Fatalf("winner not durable: %+v err=%v", status, err)
	}
}

func TestSecretStoreRecoversOwnedPrePublishTemporaryFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSecretStore(directory, bytes.Repeat([]byte{0x25}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	operationID := "30000000-0000-4000-8000-000000000003"
	ref := store.secretRef("owner-1", operationID)
	if err := os.WriteFile(filepath.Join(directory, "."+ref+".tmp"), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	provision, err := store.ProvisionRegistry(context.Background(), "owner-1", operationID, []byte("registry"))
	if err != nil || provision.CredentialRef != ref {
		t.Fatalf("pre-publish recovery failed: %+v err=%v", provision, err)
	}
	if _, err := os.Lstat(filepath.Join(directory, "."+ref+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary file retained: %v", err)
	}
}

func TestSecretStoreDoesNotAcknowledgeBeforeRecoveredDirectoryBarrier(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewSecretStore(directory, bytes.Repeat([]byte{0x26}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	originalSync := store.syncDirectory
	failSync := true
	store.syncDirectory = func() error {
		if failSync {
			return errors.New("injected directory sync failure")
		}
		return originalSync()
	}
	operationID := "30000000-0000-4000-8000-000000000004"
	if _, err := store.ProvisionRegistry(context.Background(), "owner-1", operationID, []byte("registry")); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("publish without barrier acknowledged: %v", err)
	}
	if _, err := store.ProvisionStatus(context.Background(), "owner-1", operationID); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("status without recovered barrier acknowledged: %v", err)
	}
	if _, err := store.ProvisionRegistry(context.Background(), "owner-1", operationID, []byte("registry")); !errors.Is(err, ErrSecretUnavailable) {
		t.Fatalf("replay without recovered barrier acknowledged: %v", err)
	}
	failSync = false
	status, err := store.ProvisionStatus(context.Background(), "owner-1", operationID)
	if err != nil || status.Status != "provisioned" || status.CredentialRef == "" {
		t.Fatalf("recovered barrier did not expose durable receipt: %+v err=%v", status, err)
	}
}

func TestLiveLocalDockerReadOnlyProbe(t *testing.T) {
	socket := os.Getenv("R07_TEST_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("R07_TEST_DOCKER_SOCKET is not set")
	}
	descriptor := HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: testHostID, HostVersion: 1,
		DisplayName: "Local Docker Desktop", Transport: "local", TargetRef: "live-local-desktop",
		DockerContextRef: "desktop-linux", HostPlatform: "darwin", HostArchitecture: "arm64",
	}
	result := (Prober{Connector: Connector{Targets: staticTargetResolver{target: Target{
		Ref: "live-local-desktop", Revision: 1, Transport: "local", LocalSocket: socket, DockerContext: "desktop-linux",
	}}}}).Probe(context.Background(), "owner-1", descriptor)
	if result.Availability != "ready" || result.DaemonID == "" || result.IdentitySHA256 == "" ||
		result.EngineOS != "linux" || result.Architecture != "arm64" {
		t.Fatalf("local Docker Desktop probe failed: %+v", result)
	}
	t.Logf("daemon=%s engine=%s api=%s os=%s arch=%s identity=%s", result.DaemonID, result.EngineVersion, result.APIVersion, result.EngineOS, result.Architecture, result.IdentitySHA256)
}
