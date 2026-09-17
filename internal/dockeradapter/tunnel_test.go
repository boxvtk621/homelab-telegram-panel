package dockeradapter

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel"
	"golang.org/x/crypto/ssh"
)

const tunnelNodeID = "20000000-0000-4000-8000-000000000001"

type tunnelSSHFixture struct {
	listener       net.Listener
	dockerListener net.Listener
	server         *http.Server
	hostKey        string
	clientKey      []byte
	authorized     ssh.PublicKey
	denied         atomic.Bool
	asleep         atomic.Bool
	stallSessions  atomic.Bool
	stdioCalls     atomic.Int32
	forwardCalls   atomic.Int32
	mu             sync.Mutex
	connections    map[net.Conn]struct{}
	closed         chan struct{}
	sessionSeen    chan struct{}
	sessionOnce    sync.Once
}

func newTunnelSSHFixture(t *testing.T) *tunnelSSHFixture {
	t.Helper()
	_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientPrivate, "r08-test")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dockerListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		listener.Close()
		t.Fatal(err)
	}
	fixture := &tunnelSSHFixture{
		listener: listener, dockerListener: dockerListener, hostKey: ssh.FingerprintSHA256(hostSigner.PublicKey()),
		clientKey: pem.EncodeToMemory(block), authorized: clientSigner.PublicKey(), connections: make(map[net.Conn]struct{}),
		closed: make(chan struct{}), sessionSeen: make(chan struct{}),
	}
	fixture.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/_ping":
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "OK")
		case "/version":
			_, _ = io.WriteString(w, `{"ApiVersion":"1.51","MinAPIVersion":"1.24","Version":"29.8.0","Os":"linux","Arch":"amd64"}`)
		case "/info":
			_, _ = io.WriteString(w, `{"ID":"r08-daemon","OSType":"linux","Architecture":"amd64","OperatingSystem":"Debian","ServerVersion":"29.8.0"}`)
		default:
			http.NotFound(w, r)
		}
	})}
	go func() { _ = fixture.server.Serve(dockerListener) }()
	serverConfig := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(fixture.authorized.Marshal()) {
			return nil, errors.New("unauthorized")
		}
		return nil, nil
	}}
	serverConfig.AddHostKey(hostSigner)
	go fixture.accept(serverConfig)
	t.Cleanup(fixture.Close)
	_ = clientPublic
	return fixture
}

func (f *tunnelSSHFixture) accept(config *ssh.ServerConfig) {
	defer close(f.closed)
	for {
		connection, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.connections[connection] = struct{}{}
		f.mu.Unlock()
		go f.serveConnection(connection, config)
	}
}

func (f *tunnelSSHFixture) serveConnection(connection net.Conn, config *ssh.ServerConfig) {
	defer func() {
		_ = connection.Close()
		f.mu.Lock()
		delete(f.connections, connection)
		f.mu.Unlock()
	}()
	if f.asleep.Load() {
		return
	}
	server, channels, requests, err := ssh.NewServerConn(connection, config)
	if err != nil {
		return
	}
	defer server.Close()
	go func() {
		for request := range requests {
			if request.WantReply {
				_ = request.Reply(!f.asleep.Load(), nil)
			}
		}
	}()
	for channel := range channels {
		switch channel.ChannelType() {
		case "session":
			if f.stallSessions.Load() {
				f.sessionOnce.Do(func() { close(f.sessionSeen) })
				continue
			}
			accepted, requests, err := channel.Accept()
			if err == nil {
				go f.session(accepted, requests)
			}
		case "direct-tcpip":
			go f.forward(channel)
		default:
			_ = channel.Reject(ssh.UnknownChannelType, "unsupported")
		}
	}
}

func (f *tunnelSSHFixture) session(channel ssh.Channel, requests <-chan *ssh.Request) {
	for request := range requests {
		if request.Type != "exec" || len(request.Payload) < 4 {
			_ = request.Reply(false, nil)
			continue
		}
		length := int(binary.BigEndian.Uint32(request.Payload[:4]))
		if length != len(request.Payload)-4 {
			_ = request.Reply(false, nil)
			continue
		}
		command := string(request.Payload[4:])
		_ = request.Reply(true, nil)
		switch {
		case strings.Contains(command, "context") && strings.Contains(command, "inspect"):
			_, _ = io.WriteString(channel, `"unix:///var/run/docker.sock"`+"\n")
			sendExitStatus(channel, 0)
			_ = channel.Close()
		case strings.Contains(command, "system") && strings.Contains(command, "dial-stdio"):
			f.stdioCalls.Add(1)
			upstream, err := net.Dial("tcp", f.dockerListener.Addr().String())
			if err != nil {
				sendExitStatus(channel, 1)
				_ = channel.Close()
				return
			}
			proxyChannel(channel, upstream)
		default:
			sendExitStatus(channel, 127)
			_ = channel.Close()
		}
		return
	}
}

type directPayload struct {
	DestinationAddress string
	DestinationPort    uint32
	OriginAddress      string
	OriginPort         uint32
}

func (f *tunnelSSHFixture) forward(channel ssh.NewChannel) {
	if f.denied.Load() {
		_ = channel.Reject(ssh.Prohibited, "forwarding disabled")
		return
	}
	var payload directPayload
	if ssh.Unmarshal(channel.ExtraData(), &payload) != nil || payload.DestinationAddress != "127.0.0.1" || payload.DestinationPort == 0 || payload.DestinationPort > 65535 {
		_ = channel.Reject(ssh.Prohibited, "loopback only")
		return
	}
	upstream, err := net.Dial("tcp", net.JoinHostPort(payload.DestinationAddress, fmt.Sprint(payload.DestinationPort)))
	if err != nil {
		_ = channel.Reject(ssh.ConnectionFailed, "unavailable")
		return
	}
	accepted, requests, err := channel.Accept()
	if err != nil {
		upstream.Close()
		return
	}
	f.forwardCalls.Add(1)
	go ssh.DiscardRequests(requests)
	proxyChannel(accepted, upstream)
}

func sendExitStatus(channel ssh.Channel, status uint32) {
	_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
}

func proxyChannel(channel io.ReadWriteCloser, upstream net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(channel, upstream); done <- struct{}{} }()
	go func() { _, _ = io.Copy(upstream, channel); done <- struct{}{} }()
	<-done
	_ = channel.Close()
	_ = upstream.Close()
	<-done
}

func (f *tunnelSSHFixture) DropAll() {
	f.mu.Lock()
	for connection := range f.connections {
		_ = connection.Close()
	}
	f.mu.Unlock()
}

func (f *tunnelSSHFixture) Sleep() { f.asleep.Store(true); f.DropAll() }
func (f *tunnelSSHFixture) Wake()  { f.asleep.Store(false) }

func (f *tunnelSSHFixture) Close() {
	f.DropAll()
	_ = f.listener.Close()
	_ = f.server.Close()
	<-f.closed
}

func startEcho(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(connection, connection); _ = connection.Close() }()
		}
	}()
	return listener
}

func tunnelBinding(t *testing.T, fixture *tunnelSSHFixture, endpoint string) harnesstunnel.EndpointBinding {
	t.Helper()
	target := tunnelTarget(fixture)
	version := engineVersion{APIVersion: "1.51", MinAPIVersion: "1.24", Version: "29.8.0", Os: "linux", Arch: "amd64"}
	info := engineInfo{ID: "r08-daemon", OSType: "linux", Architecture: "amd64", OperatingSystem: "Debian", ServerVersion: "29.8.0"}
	descriptor := HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: testHostID, HostVersion: 7, DisplayName: "R08 fixture",
		Transport: "ssh", TargetRef: target.Ref, CredentialRef: "ssh-r08", ExpectedHostKey: fixture.hostKey,
		DockerContextRef: target.DockerContext, HostPlatform: "linux", HostArchitecture: "amd64",
	}
	identity, err := identitySHA256(descriptor, ConnectionIdentity{
		HostKeySHA256:   fixture.hostKey,
		ContextEndpoint: safeEndpointIdentity("ssh", target.Ref, target.Revision, "unix:///var/run/docker.sock"),
	}, version, info)
	if err != nil {
		t.Fatal(err)
	}
	return harnesstunnel.EndpointBinding{
		NodeID: tunnelNodeID, RegistrationRevision: 3, RegistrationEpoch: 5, EndpointRevision: 11,
		HostID: testHostID, HostVersion: 7, Transport: "ssh", TargetRef: target.Ref, CredentialRef: "ssh-r08",
		DockerContextRef: target.DockerContext, ExpectedHostKey: fixture.hostKey, ExpectedHostIdentitySHA256: identity,
		HostPlatform: "linux", HostArchitecture: "amd64", ContainerID: strings.Repeat("a", 64), RuntimeGeneration: 2,
		Address: endpoint,
	}
}

func tunnelTarget(fixture *tunnelSSHFixture) Target {
	return Target{
		Ref: "ssh-r08", Revision: 2, Transport: "ssh", SSHAddress: fixture.listener.Addr().String(), SSHUser: "operator",
		DockerExecutable: "/usr/bin/docker", DockerContext: "default", RemoteShell: "posix", HostKeySHA256: fixture.hostKey,
	}
}

func writeTunnelManifest(t *testing.T, binding harnesstunnel.EndpointBinding) (string, string) {
	t.Helper()
	registryHash := strings.Repeat("c", 64)
	raw, err := json.Marshal(harnesstunnel.BindingManifest{
		SchemaID: harnesstunnel.BindingSchemaID, OwnerID: "owner-1", RegistrySHA256: registryHash,
		Nodes: []harnesstunnel.EndpointBinding{binding},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, registryHash
}

func TestTunnelServiceAcceptsStagedCandidateRegistryAfterRestart(t *testing.T) {
	current, candidate := strings.Repeat("c", 64), strings.Repeat("d", 64)
	binding := harnesstunnel.EndpointBinding{
		Kind: "external", NodeID: tunnelNodeID, RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: testHostID, HostVersion: 1, Transport: "local", TargetRef: "local-r10", Address: "10.20.30.40:9443",
	}
	raw, err := json.Marshal(harnesstunnel.BindingManifest{
		SchemaID: harnesstunnel.BindingSchemaID, OwnerID: "owner-1", RegistrySHA256: current,
		AcceptedRegistrySHA256s: []string{current, candidate}, Nodes: []harnesstunnel.EndpointBinding{binding},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bindings.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	service, err := NewTunnelService(TunnelConfig{
		BindingPath: path, OwnerID: "owner-1", RegistrySHA256: candidate,
		Connector: Connector{Targets: staticTargetResolver{target: Target{Ref: "local-r10", Revision: 1, Transport: "local"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	service.Close()
}

func startTunnelService(t *testing.T, fixture *tunnelSSHFixture, binding harnesstunnel.EndpointBinding, target Target) (*harnesstunnel.Client, context.CancelFunc) {
	t.Helper()
	path, registryHash := writeTunnelManifest(t, binding)
	service, err := NewTunnelService(TunnelConfig{
		BindingPath: path, OwnerID: "owner-1", RegistrySHA256: registryHash,
		Connector:         Connector{Targets: staticTargetResolver{target: target}, Credentials: fixedCredentialResolver{credential: SSHCredential{PrivateKey: fixture.clientKey}}},
		Pool:              harnesstunnel.PoolConfig{Total: 4, Streams: 3, PerNode: 4, PerNodeStreams: 3, Pending: 8},
		KeepAliveInterval: time.Minute, RetryDelay: func(int) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "r08-tunnel-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(directory, "tunnel.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- service.Serve(ctx, listener) }()
	client, err := harnesstunnel.NewClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		cancel()
		service.Close()
		if err := <-finished; err != nil {
			t.Errorf("tunnel service: %v", err)
		}
	}
	t.Cleanup(cleanup)
	return client, cancel
}

func roundTripTunnel(t *testing.T, client *harnesstunnel.Client, binding harnesstunnel.EndpointBinding, purpose harnesstunnel.Purpose) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, err := client.DialContext(ctx, "owner-1", binding, purpose)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	payload := []byte("r08-private-channel")
	if _, err := connection.Write(payload); err != nil {
		t.Fatal(err)
	}
	received := make([]byte, len(payload))
	if _, err := io.ReadFull(connection, received); err != nil || string(received) != string(payload) {
		t.Fatalf("tunnel payload: %q err=%v", received, err)
	}
}

func requireDialCode(t *testing.T, err error, code string) {
	t.Helper()
	var dialError *harnesstunnel.DialError
	if !errors.As(err, &dialError) || dialError.Code != code {
		t.Fatalf("dial error=%v, want %s", err, code)
	}
}

func TestPrivateSSHTunnelDirectTCPIPFaultsAndReconnect(t *testing.T) {
	fixture := newTunnelSSHFixture(t)
	echo := startEcho(t)
	binding := tunnelBinding(t, fixture, echo.Addr().String())
	client, _ := startTunnelService(t, fixture, binding, tunnelTarget(fixture))

	purposes := []harnesstunnel.Purpose{
		harnesstunnel.PurposeCommand,
		harnesstunnel.PurposeResponse,
		harnesstunnel.PurposeEvents,
		harnesstunnel.PurposeHealth,
		harnesstunnel.PurposeAdmin,
		harnesstunnel.PurposeReplicaExport,
		harnesstunnel.PurposeReplicaImport,
	}
	for _, purpose := range purposes {
		roundTripTunnel(t, client, binding, purpose)
	}
	if fixture.stdioCalls.Load() != 1 || fixture.forwardCalls.Load() != int32(len(purposes)) {
		t.Fatalf("channels not separated: dial-stdio=%d direct-tcpip=%d", fixture.stdioCalls.Load(), fixture.forwardCalls.Load())
	}

	stale := binding
	stale.EndpointRevision--
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err := client.DialContext(ctx, "owner-1", stale, harnesstunnel.PurposeCommand)
	cancel()
	requireDialCode(t, err, "stale_endpoint")

	fixture.denied.Store(true)
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.DialContext(ctx, "owner-1", binding, harnesstunnel.PurposeCommand)
	cancel()
	requireDialCode(t, err, "ssh_forwarding_denied")
	fixture.denied.Store(false)

	fixture.DropAll()
	roundTripTunnel(t, client, binding, harnesstunnel.PurposeCommand)

	fixture.Sleep()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	_, err = client.DialContext(ctx, "owner-1", binding, harnesstunnel.PurposeHealth)
	cancel()
	requireDialCode(t, err, "ssh_unavailable")
	fixture.Wake()
	roundTripTunnel(t, client, binding, harnesstunnel.PurposeHealth)
}

func TestExternalSSHTunnelUsesDirectTCPIPWithoutDockerExec(t *testing.T) {
	fixture := newTunnelSSHFixture(t)
	echo := startEcho(t)
	binding := tunnelBinding(t, fixture, echo.Addr().String())
	binding.Kind = "external"
	binding.DockerContextRef = ""
	binding.ExpectedHostIdentitySHA256 = ""
	binding.HostPlatform = ""
	binding.HostArchitecture = ""
	binding.ContainerID = ""
	binding.RuntimeGeneration = 0
	client, _ := startTunnelService(t, fixture, binding, tunnelTarget(fixture))
	roundTripTunnel(t, client, binding, harnesstunnel.PurposeHealth)
	if fixture.stdioCalls.Load() != 0 || fixture.forwardCalls.Load() != 1 {
		t.Fatalf("external enrollment used exec/docker channel: dial-stdio=%d direct-tcpip=%d", fixture.stdioCalls.Load(), fixture.forwardCalls.Load())
	}
}

func TestPrivateSSHTunnelRejectsObservedWrongHostIdentity(t *testing.T) {
	fixture := newTunnelSSHFixture(t)
	echo := startEcho(t)
	binding := tunnelBinding(t, fixture, echo.Addr().String())
	binding.ExpectedHostKey = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	target := tunnelTarget(fixture)
	target.HostKeySHA256 = binding.ExpectedHostKey
	client, _ := startTunnelService(t, fixture, binding, target)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := client.DialContext(ctx, "owner-1", binding, harnesstunnel.PurposeCommand)
	requireDialCode(t, err, "host_identity_mismatch")
	if fixture.stdioCalls.Load() != 0 || fixture.forwardCalls.Load() != 0 {
		t.Fatal("wrong host identity reached Docker or Harness endpoint")
	}
}

func TestSSHSetupDoesNotHoldManagerLockAndShutdownCancelsIt(t *testing.T) {
	fixture := newTunnelSSHFixture(t)
	fixture.stallSessions.Store(true)
	echo := startEcho(t)
	binding := tunnelBinding(t, fixture, echo.Addr().String())
	target := tunnelTarget(fixture)
	manager := &sshTunnelManager{
		config: TunnelConfig{
			Connector: Connector{Credentials: fixedCredentialResolver{credential: SSHCredential{PrivateKey: fixture.clientKey}}},
			Now:       time.Now, KeepAliveInterval: time.Minute, KeepAliveMisses: 3,
			RetryDelay: func(int) time.Duration { return 0 },
		},
		entries: make(map[string]*managedSSH), hosts: make(map[string]string), retry: make(map[string]sshRetryState),
		opening: make(map[string]*sshOpening), done: make(chan struct{}),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan string, 1)
	go func() {
		connection, code := manager.Dial(ctx, "owner-1", binding, target)
		if connection != nil {
			_ = connection.Close()
		}
		result <- code
	}()
	select {
	case <-fixture.sessionSeen:
	case <-time.After(time.Second):
		t.Fatal("SSH setup did not reach the deterministic stalled channel")
	}

	lockAvailable := make(chan struct{})
	go func() {
		manager.mu.Lock()
		manager.mu.Unlock()
		close(lockAvailable)
	}()
	select {
	case <-lockAvailable:
	case <-time.After(time.Second):
		t.Fatal("stalled SSH setup held the global manager lock")
	}

	closed := make(chan struct{})
	go func() { manager.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("manager shutdown blocked behind stalled SSH setup")
	}
	select {
	case code := <-result:
		if code != "dialer_unavailable" {
			t.Fatalf("stalled setup result=%s, want dialer_unavailable", code)
		}
	case <-time.After(time.Second):
		t.Fatal("stalled SSH setup did not stop with manager shutdown")
	}
}

func TestLocalPrivateTunnelUsesBoundEndpointWithoutSSH(t *testing.T) {
	echo := startEcho(t)
	dockerDirectory, err := os.MkdirTemp("", "r08-docker-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dockerDirectory)
	dockerSocket := filepath.Join(dockerDirectory, "docker.sock")
	dockerListener, err := net.Listen("unix", dockerSocket)
	if err != nil {
		t.Fatal(err)
	}
	dockerServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_ping":
			_, _ = io.WriteString(w, "OK")
		case "/version":
			_, _ = io.WriteString(w, `{"ApiVersion":"1.51","Version":"29.8.0","Os":"linux","Arch":"arm64"}`)
		case "/info":
			_, _ = io.WriteString(w, `{"ID":"local-r08-daemon","OSType":"linux","Architecture":"arm64","OperatingSystem":"Docker Desktop","ServerVersion":"29.8.0"}`)
		}
	})}
	go func() { _ = dockerServer.Serve(dockerListener) }()
	defer dockerServer.Close()
	target := Target{Ref: "local-r08", Revision: 1, Transport: "local", LocalSocket: dockerSocket, DockerContext: "desktop-linux"}
	descriptor := HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: testHostID, HostVersion: 7, DisplayName: "Harness tunnel host",
		Transport: "local", TargetRef: target.Ref, DockerContextRef: target.DockerContext, HostPlatform: "darwin", HostArchitecture: "arm64",
	}
	identity, err := identitySHA256(descriptor, ConnectionIdentity{
		ContextEndpoint: safeEndpointIdentity("local", target.Ref, target.Revision, "unix:"+target.LocalSocket),
	}, engineVersion{APIVersion: "1.51", Version: "29.8.0", Os: "linux", Arch: "arm64"}, engineInfo{
		ID: "local-r08-daemon", OSType: "linux", Architecture: "arm64", OperatingSystem: "Docker Desktop", ServerVersion: "29.8.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := harnesstunnel.EndpointBinding{
		NodeID: tunnelNodeID, RegistrationRevision: 3, RegistrationEpoch: 5, EndpointRevision: 11,
		HostID: testHostID, HostVersion: 7, Transport: "local", TargetRef: "local-r08", DockerContextRef: "desktop-linux",
		ExpectedHostIdentitySHA256: identity, HostPlatform: "darwin", HostArchitecture: "arm64",
		ContainerID: strings.Repeat("e", 64), RuntimeGeneration: 2, Address: echo.Addr().String(),
	}
	path, registryHash := writeTunnelManifest(t, binding)
	service, err := NewTunnelService(TunnelConfig{
		BindingPath: path, OwnerID: "owner-1", RegistrySHA256: registryHash,
		Connector: Connector{Targets: staticTargetResolver{target: target}},
	})
	if err != nil {
		t.Fatal(err)
	}
	directory, err := os.MkdirTemp("", "r08-local-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	_ = os.Chmod(directory, 0o700)
	socket := filepath.Join(directory, "local.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Serve(ctx, listener) }()
	client, _ := harnesstunnel.NewClient(socket)
	roundTripTunnel(t, client, binding, harnesstunnel.PurposeCommand)
	held, err := client.DialContext(context.Background(), "owner-1", binding, harnesstunnel.PurposeEvents)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	cancel()
	closed := make(chan struct{})
	go func() { service.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("tunnel shutdown waited for long stream")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
