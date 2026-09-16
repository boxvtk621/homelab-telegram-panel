package dockeradapter

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel"
	"golang.org/x/crypto/ssh"
)

type TunnelConfig struct {
	BindingPath, OwnerID, RegistrySHA256 string
	Connector                            Connector
	Pool                                 harnesstunnel.PoolConfig
	Now                                  func() time.Time
	KeepAliveInterval                    time.Duration
	KeepAliveMisses                      int
	RetryDelay                           func(int) time.Duration
}

// TunnelService accepts only exact node/revision/purpose requests on a private
// UDS. The address and SSH credential reference are resolved adapter-side.
type TunnelService struct {
	config TunnelConfig
	pool   *harnesstunnel.Pool
	ssh    *sshTunnelManager
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
	active map[net.Conn]struct{}
}

func NewTunnelService(config TunnelConfig) (*TunnelService, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Pool == (harnesstunnel.PoolConfig{}) {
		config.Pool = harnesstunnel.DefaultPoolConfig()
	}
	if config.KeepAliveInterval == 0 {
		config.KeepAliveInterval = 15 * time.Second
	}
	if config.KeepAliveMisses == 0 {
		config.KeepAliveMisses = 3
	}
	if config.RetryDelay == nil {
		config.RetryDelay = tunnelRetryDelay
	}
	manifest, err := harnesstunnel.LoadManifest(config.BindingPath)
	if err != nil || manifest.OwnerID != config.OwnerID || manifest.RegistrySHA256 != config.RegistrySHA256 ||
		config.Connector.Targets == nil || config.KeepAliveInterval <= 0 || config.KeepAliveInterval > time.Minute ||
		config.KeepAliveMisses < 1 || config.KeepAliveMisses > 10 {
		return nil, errors.New("invalid Harness tunnel configuration")
	}
	pool, err := harnesstunnel.NewPool(config.Pool)
	if err != nil {
		return nil, err
	}
	manager := &sshTunnelManager{
		config: config, entries: make(map[string]*managedSSH), hosts: make(map[string]string),
		retry: make(map[string]sshRetryState), opening: make(map[string]*sshOpening), done: make(chan struct{}),
	}
	return &TunnelService{config: config, pool: pool, ssh: manager, active: make(map[net.Conn]struct{})}, nil
}

func tunnelRetryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	maximum := 1 << min(attempt-1, 4)
	// The delay is bounded and jittered without shared PRNG state. Attempts are
	// observable only as a retryable fixed error, never as secret-bearing logs.
	digest := sha256.Sum256([]byte{byte(attempt)})
	seconds := 1 + int(digest[0])%maximum
	if seconds > 30 {
		seconds = 30
	}
	return time.Duration(seconds) * time.Second
}

func (s *TunnelService) Serve(ctx context.Context, listener net.Listener) error {
	if s == nil || listener == nil {
		return errors.New("invalid Harness tunnel listener")
	}
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = connection.Close()
			return nil
		}
		s.active[connection] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go func() {
			defer s.wg.Done()
			s.handle(connection)
		}()
	}
}

func (s *TunnelService) handle(connection net.Conn) {
	defer func() {
		_ = connection.Close()
		s.mu.Lock()
		delete(s.active, connection)
		s.mu.Unlock()
	}()
	now := s.config.Now()
	_ = connection.SetDeadline(now.Add(10 * time.Second))
	request, err := harnesstunnel.ReadDialRequest(connection)
	if err != nil || request.Validate(now) != nil {
		s.reject(connection, "invalid_request")
		return
	}
	manifest, err := harnesstunnel.LoadManifest(s.config.BindingPath)
	if err != nil || manifest.OwnerID != s.config.OwnerID || manifest.RegistrySHA256 != s.config.RegistrySHA256 {
		s.reject(connection, "binding_unavailable")
		return
	}
	binding, ok := manifest.Binding(request.NodeID)
	if !ok {
		s.reject(connection, "node_not_registered")
		return
	}
	if !request.Matches(manifest.OwnerID, binding) {
		s.reject(connection, "stale_endpoint")
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(request.DeadlineUnixMilli))
	defer cancel()
	lease, err := s.pool.Acquire(ctx, request.NodeID, request.Purpose)
	if err != nil {
		code := "pool_exhausted"
		if errors.Is(err, context.DeadlineExceeded) {
			code = "setup_timeout"
		}
		s.reject(connection, code)
		return
	}
	defer lease.Release()
	upstream, code := s.dial(ctx, manifest.OwnerID, binding)
	if code != "" {
		s.reject(connection, code)
		return
	}
	defer upstream.Close()
	if harnesstunnel.WriteDialReply(connection, harnesstunnel.DialReply{SchemaID: harnesstunnel.ReplySchemaID, Status: "ready"}) != nil {
		return
	}
	_ = connection.SetDeadline(time.Time{})
	copyTunnel(connection, upstream)
}

func (s *TunnelService) reject(connection net.Conn, code string) {
	_ = harnesstunnel.WriteDialReply(connection, harnesstunnel.DialReply{SchemaID: harnesstunnel.ReplySchemaID, Status: "rejected", Code: code})
}

func (s *TunnelService) dial(ctx context.Context, owner string, binding harnesstunnel.EndpointBinding) (net.Conn, string) {
	target, err := s.config.Connector.Targets.ResolveTarget(ctx, owner, binding.TargetRef)
	if err != nil || validateTarget(target) != nil || target.Transport != binding.Transport ||
		target.DockerContext != binding.DockerContextRef || binding.Transport == "ssh" && target.HostKeySHA256 != binding.ExpectedHostKey {
		return nil, "host_identity_mismatch"
	}
	if binding.Transport == "local" {
		if code := verifyLocalBinding(ctx, s.config.Connector, owner, binding); code != "" {
			return nil, code
		}
		connection, err := (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}).DialContext(ctx, "tcp", binding.Address)
		if err != nil {
			return nil, "endpoint_unavailable"
		}
		return connection, ""
	}
	return s.ssh.Dial(ctx, owner, binding, target)
}

func copyTunnel(left, right net.Conn) {
	done := make(chan struct{}, 2)
	copyOne := func(destination, source net.Conn) {
		_, _ = io.Copy(destination, source)
		if closer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- struct{}{}
	}
	go copyOne(left, right)
	go copyOne(right, left)
	<-done
	_ = left.Close()
	_ = right.Close()
	<-done
}

func (s *TunnelService) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.closed = true
	for connection := range s.active {
		_ = connection.Close()
	}
	s.mu.Unlock()
	s.pool.Close()
	s.ssh.Close()
	s.wg.Wait()
}

func bindingDescriptor(binding harnesstunnel.EndpointBinding) HostDescriptor {
	return HostDescriptor{
		SchemaID: HostDescriptorSchemaID, HostID: binding.HostID, HostVersion: binding.HostVersion,
		DisplayName: "Harness tunnel host", Transport: binding.Transport, TargetRef: binding.TargetRef,
		CredentialRef: binding.CredentialRef, ExpectedHostKey: binding.ExpectedHostKey,
		DockerContextRef: binding.DockerContextRef, ExpectedIdentitySHA256: binding.ExpectedHostIdentitySHA256,
		HostPlatform: binding.HostPlatform, HostArchitecture: binding.HostArchitecture,
	}
}

func verifyLocalBinding(ctx context.Context, connector Connector, owner string, binding harnesstunnel.EndpointBinding) string {
	descriptor := bindingDescriptor(binding)
	session, connection, fault := connector.Open(ctx, owner, descriptor)
	if fault != nil {
		return "host_identity_mismatch"
	}
	defer session.Close()
	return verifyEngineBinding(ctx, session, descriptor, connection)
}

type managedSSH struct {
	client     *ssh.Client
	connection net.Conn
	closed     chan struct{}
}

type sshRetryState struct {
	attempt int
	next    time.Time
}

type sshOpening struct {
	key        string
	done       chan struct{}
	cancel     context.CancelFunc
	client     *ssh.Client
	connection net.Conn
	code       string
}

type sshTunnelManager struct {
	mu      sync.Mutex
	config  TunnelConfig
	entries map[string]*managedSSH
	hosts   map[string]string
	retry   map[string]sshRetryState
	opening map[string]*sshOpening
	done    chan struct{}
	closed  bool
}

func sshBindingKey(owner string, binding harnesstunnel.EndpointBinding, target Target) string {
	return owner + "\x00" + binding.HostID + "\x00" + strconv.FormatInt(binding.HostVersion, 10) + "\x00" +
		binding.TargetRef + "\x00" + strconv.FormatInt(target.Revision, 10) + "\x00" + target.SSHAddress + "\x00" + target.SSHUser + "\x00" +
		target.DockerExecutable + "\x00" + target.RemoteShell + "\x00" + binding.CredentialRef + "\x00" + binding.ExpectedHostKey + "\x00" + binding.ExpectedHostIdentitySHA256
}

func (m *sshTunnelManager) Dial(ctx context.Context, owner string, binding harnesstunnel.EndpointBinding, target Target) (net.Conn, string) {
	key := sshBindingKey(owner, binding, target)
	for attempt := 0; attempt < 2; attempt++ {
		entry, code := m.connection(ctx, key, owner, binding, target)
		if code != "" {
			return nil, code
		}
		connection, err := dialSSHContext(ctx, entry.client, binding.Address)
		if err == nil {
			return connection, ""
		}
		var channelError *ssh.OpenChannelError
		if errors.As(err, &channelError) && channelError.Reason == ssh.Prohibited {
			return nil, "ssh_forwarding_denied"
		}
		m.invalidate(key, entry)
	}
	return nil, "ssh_unavailable"
}

func (m *sshTunnelManager) connection(ctx context.Context, key, owner string, binding harnesstunnel.EndpointBinding, target Target) (*managedSSH, string) {
	hostScope := owner + "\x00" + binding.HostID
	for {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return nil, "dialer_unavailable"
		}
		if m.opening == nil {
			m.opening = make(map[string]*sshOpening)
		}
		if m.done == nil {
			m.done = make(chan struct{})
		}
		if current := m.entries[key]; current != nil {
			m.mu.Unlock()
			return current, ""
		}
		if pending := m.opening[hostScope]; pending != nil {
			done, managerDone := pending.done, m.done
			m.mu.Unlock()
			select {
			case <-done:
				if pending.code != "" {
					return nil, pending.code
				}
				continue
			case <-ctx.Done():
				return nil, "ssh_unavailable"
			case <-managerDone:
				return nil, "dialer_unavailable"
			}
		}
		if m.retry == nil {
			m.retry = make(map[string]sshRetryState)
		}
		state := m.retry[key]
		if delay := state.next.Sub(m.config.Now()); delay > 0 {
			managerDone := m.done
			m.mu.Unlock()
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, "ssh_reconnect_backoff"
			case <-managerDone:
				timer.Stop()
				return nil, "dialer_unavailable"
			case <-timer.C:
				continue
			}
		}

		var prior *managedSSH
		if priorKey := m.hosts[hostScope]; priorKey != "" && priorKey != key {
			if prior = m.entries[priorKey]; prior != nil {
				delete(m.entries, priorKey)
				close(prior.closed)
			}
			delete(m.hosts, hostScope)
		}
		setupCtx, cancelSetup := context.WithCancel(ctx)
		pending := &sshOpening{key: key, done: make(chan struct{}), cancel: cancelSetup}
		m.opening[hostScope] = pending
		m.mu.Unlock()
		if prior != nil {
			_ = prior.client.Close()
			_ = prior.connection.Close()
		}

		// Network setup deliberately runs outside the manager mutex. Per-host
		// in-flight state suppresses duplicate setup while other hosts and Close
		// remain available.
		client, underlying, _, fault := m.config.Connector.openSSHClient(setupCtx, owner, binding.ExpectedHostKey, binding.CredentialRef, target)
		if fault != nil {
			code := "ssh_unavailable"
			if fault.code == "ssh_host_key_changed" || fault.code == "ssh_host_key_unknown" {
				code = "host_identity_mismatch"
			}
			return m.completeOpening(hostScope, pending, nil, nil, code)
		}

		m.mu.Lock()
		if m.opening[hostScope] != pending || m.closed {
			m.mu.Unlock()
			return m.completeOpening(hostScope, pending, client, underlying, "dialer_unavailable")
		}
		pending.client, pending.connection = client, underlying
		m.mu.Unlock()

		// x/crypto/ssh channel creation has no context API. Closing this exact
		// setup client on cancellation bounds NewSession/Start and wakes shutdown.
		stopCancelClose := context.AfterFunc(setupCtx, func() {
			_ = client.Close()
			_ = underlying.Close()
		})
		code := verifySSHBinding(setupCtx, client, binding, target)
		stopped := stopCancelClose()
		if !stopped || setupCtx.Err() != nil {
			code = "ssh_unavailable"
		}
		return m.completeOpening(hostScope, pending, client, underlying, code)
	}
}

func (m *sshTunnelManager) completeOpening(hostScope string, pending *sshOpening, client *ssh.Client, underlying net.Conn, code string) (*managedSSH, string) {
	var entry *managedSSH
	m.mu.Lock()
	if current := m.opening[hostScope]; current == pending {
		delete(m.opening, hostScope)
	} else {
		code = "dialer_unavailable"
	}
	if m.closed {
		code = "dialer_unavailable"
	}
	if code == "" {
		entry = &managedSSH{client: client, connection: underlying, closed: make(chan struct{})}
		m.entries[pending.key] = entry
		m.hosts[hostScope] = pending.key
		delete(m.retry, pending.key)
	} else if !m.closed {
		state := m.retry[pending.key]
		state.attempt++
		state.next = m.config.Now().Add(m.config.RetryDelay(state.attempt))
		m.retry[pending.key] = state
	}
	pending.code = code
	close(pending.done)
	m.mu.Unlock()
	pending.cancel()

	if entry == nil {
		if client != nil {
			_ = client.Close()
		}
		if underlying != nil {
			_ = underlying.Close()
		}
		return nil, code
	}
	go m.keepAlive(pending.key, entry)
	go func() {
		_ = client.Wait()
		m.invalidate(pending.key, entry)
	}()
	return entry, ""
}

func verifySSHBinding(ctx context.Context, client *ssh.Client, binding harnesstunnel.EndpointBinding, target Target) string {
	endpoint, fault := inspectRemoteContext(ctx, client, target)
	if fault != nil {
		return "host_identity_mismatch"
	}
	stdio, err := openSSHStdio(ctx, client, target)
	if err != nil {
		return "docker_channel_unavailable"
	}
	transport := engineTransport(func(context.Context, string, string) (net.Conn, error) { return stdio, nil })
	session := &httpEngineSession{client: engineHTTPClient(transport), transport: transport, close: stdio.Close}
	defer session.Close()
	descriptor := bindingDescriptor(binding)
	return verifyEngineBinding(ctx, session, descriptor, ConnectionIdentity{
		HostKeySHA256:   binding.ExpectedHostKey,
		ContextEndpoint: safeEndpointIdentity("ssh", target.Ref, target.Revision, endpoint),
	})
}

func verifyEngineBinding(ctx context.Context, session EngineSession, descriptor HostDescriptor, connection ConnectionIdentity) string {
	if fault := pingEngine(ctx, session); fault != nil {
		return "docker_channel_unavailable"
	}
	var version engineVersion
	if fault := readEngineJSON(ctx, session, "/version", maximumVersionBody, &version); fault != nil {
		return "host_identity_mismatch"
	}
	var info engineInfo
	if fault := readEngineJSON(ctx, session, "/info", maximumInfoBody, &info); fault != nil {
		return "host_identity_mismatch"
	}
	if fault := validateEngine(descriptor, version, info); fault != nil {
		return "host_identity_mismatch"
	}
	identity, err := identitySHA256(descriptor, connection, version, info)
	if err != nil || identity != descriptor.ExpectedIdentitySHA256 {
		return "host_identity_mismatch"
	}
	return ""
}

func dialSSHContext(ctx context.Context, client *ssh.Client, address string) (net.Conn, error) {
	type result struct {
		connection net.Conn
		err        error
	}
	ready := make(chan result, 1)
	go func() {
		connection, err := client.Dial("tcp", address)
		ready <- result{connection: connection, err: err}
	}()
	select {
	case value := <-ready:
		return value.connection, value.err
	case <-ctx.Done():
		go func() {
			value := <-ready
			if value.connection != nil {
				_ = value.connection.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func (m *sshTunnelManager) keepAlive(key string, entry *managedSSH) {
	ticker := time.NewTicker(m.config.KeepAliveInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-entry.closed:
			return
		case <-ticker.C:
			err := boundedSSHKeepAlive(entry.client, m.config.KeepAliveInterval)
			if err == nil {
				misses = 0
				continue
			}
			misses++
			if misses >= m.config.KeepAliveMisses {
				m.invalidate(key, entry)
				return
			}
		}
	}
}

func boundedSSHKeepAlive(client *ssh.Client, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return context.DeadlineExceeded
	}
}

func (m *sshTunnelManager) invalidate(key string, entry *managedSSH) {
	removed := false
	m.mu.Lock()
	if m.entries[key] == entry {
		delete(m.entries, key)
		for hostScope, hostKey := range m.hosts {
			if hostKey == key {
				delete(m.hosts, hostScope)
			}
		}
		state := m.retry[key]
		state.attempt++
		state.next = m.config.Now().Add(m.config.RetryDelay(state.attempt))
		m.retry[key] = state
		close(entry.closed)
		removed = true
	}
	m.mu.Unlock()
	if removed {
		_ = entry.client.Close()
		_ = entry.connection.Close()
	}
}

func (m *sshTunnelManager) Close() {
	var entries []*managedSSH
	var openings []*sshOpening
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		if m.done != nil {
			close(m.done)
		}
		for key, entry := range m.entries {
			delete(m.entries, key)
			close(entry.closed)
			entries = append(entries, entry)
		}
		for _, pending := range m.opening {
			openings = append(openings, pending)
		}
		clear(m.hosts)
	}
	m.mu.Unlock()
	for _, pending := range openings {
		pending.cancel()
		if pending.client != nil {
			_ = pending.client.Close()
		}
		if pending.connection != nil {
			_ = pending.connection.Close()
		}
	}
	for _, entry := range entries {
		_ = entry.client.Close()
		_ = entry.connection.Close()
	}
}
