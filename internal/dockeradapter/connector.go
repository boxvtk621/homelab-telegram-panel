package dockeradapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

type Target struct {
	Ref              string `json:"ref"`
	Revision         int64  `json:"revision"`
	Transport        string `json:"transport"`
	LocalSocket      string `json:"localSocket,omitempty"`
	SSHAddress       string `json:"sshAddress,omitempty"`
	SSHUser          string `json:"sshUser,omitempty"`
	DockerExecutable string `json:"dockerExecutable,omitempty"`
	DockerContext    string `json:"dockerContext"`
	RemoteShell      string `json:"remoteShell,omitempty"`
	HostKeySHA256    string `json:"hostKeySHA256,omitempty"`
}

type TargetResolver interface {
	ResolveTarget(context.Context, string, string) (Target, error)
}

type SSHCredential struct {
	PrivateKey []byte
	Passphrase []byte
}

type CredentialResolver interface {
	ResolveSSH(context.Context, string, string) (SSHCredential, error)
}

type Connector struct {
	Targets     TargetResolver
	Credentials CredentialResolver
	Dialer      *net.Dialer
}

type httpEngineSession struct {
	client    *http.Client
	transport *http.Transport
	close     func() error
	once      sync.Once
}

func (s *httpEngineSession) Do(request *http.Request) (*http.Response, error) {
	return s.client.Do(request)
}

func (s *httpEngineSession) Close() error {
	s.transport.CloseIdleConnections()
	var err error
	s.once.Do(func() {
		if s.close != nil {
			err = s.close()
		}
	})
	return err
}

func (c Connector) Open(ctx context.Context, owner string, descriptor HostDescriptor) (EngineSession, ConnectionIdentity, *probeFault) {
	if c.Targets == nil {
		return nil, ConnectionIdentity{}, &probeFault{stage: "descriptor", code: "target_not_registered", next: "Зарегистрируйте server-side target descriptor."}
	}
	target, err := c.Targets.ResolveTarget(ctx, owner, descriptor.TargetRef)
	if err != nil || validateTarget(target) != nil || target.Ref != descriptor.TargetRef ||
		target.Transport != descriptor.Transport || target.DockerContext != descriptor.DockerContextRef {
		return nil, ConnectionIdentity{}, &probeFault{stage: "descriptor", code: "target_not_registered", next: "Проверьте server-side target и Docker context."}
	}
	if descriptor.Transport == "local" {
		return openLocalSession(target)
	}
	return c.openSSHSession(ctx, owner, descriptor, target)
}

func validateTarget(target Target) error {
	if !refPattern.MatchString(target.Ref) || target.Revision < 1 || target.Revision > maximumSafeInt ||
		!refPattern.MatchString(target.DockerContext) {
		return errors.New("invalid target")
	}
	switch target.Transport {
	case "local":
		if target.LocalSocket == "" || !filepath.IsAbs(target.LocalSocket) || filepath.Clean(target.LocalSocket) != target.LocalSocket ||
			len(target.LocalSocket) > 100 || strings.ContainsAny(target.LocalSocket, "\x00\r\n") ||
			target.SSHAddress != "" || target.SSHUser != "" || target.DockerExecutable != "" || target.RemoteShell != "" || target.HostKeySHA256 != "" {
			return errors.New("invalid local target")
		}
	case "ssh":
		if target.LocalSocket != "" || validateSSHAddress(target.SSHAddress) != nil || !refPattern.MatchString(target.SSHUser) ||
			!fingerprintPattern.MatchString(target.HostKeySHA256) || validateRemoteExecutable(target.DockerExecutable, target.RemoteShell) != nil {
			return errors.New("invalid ssh target")
		}
	default:
		return errors.New("invalid target transport")
	}
	return nil
}

func openLocalSession(target Target) (EngineSession, ConnectionIdentity, *probeFault) {
	dialer := &net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}
	transport := engineTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", target.LocalSocket)
	})
	identity := ConnectionIdentity{ContextEndpoint: safeEndpointIdentity("local", target.Ref, target.Revision, "unix:"+target.LocalSocket)}
	return &httpEngineSession{client: engineHTTPClient(transport), transport: transport}, identity, nil
}

var (
	hostnamePattern          = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	windowsExecutablePattern = regexp.MustCompile(`^[A-Za-z]:\\[^\x00\r\n]+\\docker\.exe$`)
)

func validateSSHAddress(value string) error {
	host, portText, err := net.SplitHostPort(value)
	if err != nil || host == "" || strings.ContainsAny(host, "\x00\r\n \t") {
		return errors.New("invalid ssh address")
	}
	if net.ParseIP(host) == nil && !hostnamePattern.MatchString(host) {
		return errors.New("invalid ssh host")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return errors.New("invalid ssh port")
	}
	return nil
}

func validateRemoteExecutable(executable, shell string) error {
	if len(executable) == 0 || len(executable) > 260 || strings.ContainsAny(executable, "\x00\r\n") {
		return errors.New("invalid docker executable")
	}
	switch shell {
	case "posix":
		if !strings.HasPrefix(executable, "/") || strings.Contains(executable, "//") {
			return errors.New("invalid posix docker executable")
		}
	case "powershell":
		if !windowsExecutablePattern.MatchString(executable) {
			return errors.New("invalid windows docker executable")
		}
	default:
		return errors.New("invalid remote shell")
	}
	return nil
}

var (
	errHostKeyChanged   = errors.New("ssh host key changed")
	errHostKeyUnknown   = errors.New("ssh host key unknown")
	errDockerPermission = errors.New("docker daemon permission denied")
)

func (c Connector) openSSHSession(ctx context.Context, owner string, descriptor HostDescriptor, target Target) (EngineSession, ConnectionIdentity, *probeFault) {
	if descriptor.ExpectedHostKey == "" {
		return nil, ConnectionIdentity{}, &probeFault{stage: "host_identity", code: "ssh_host_key_unknown", next: "Сверьте host key по доверенному каналу и сохраните pin."}
	}
	if subtle.ConstantTimeCompare([]byte(descriptor.ExpectedHostKey), []byte(target.HostKeySHA256)) != 1 {
		return nil, ConnectionIdentity{}, &probeFault{stage: "host_identity", code: "ssh_host_key_changed", next: "Остановитесь и перепроверьте identity сервера."}
	}
	if c.Credentials == nil {
		return nil, ConnectionIdentity{}, &probeFault{stage: "transport_auth", code: "ssh_credential_unavailable", next: "Сохраните SSH identity повторно."}
	}
	credential, err := c.Credentials.ResolveSSH(ctx, owner, descriptor.CredentialRef)
	if err != nil {
		return nil, ConnectionIdentity{}, &probeFault{stage: "transport_auth", code: "ssh_credential_unavailable", next: "Проверьте credential ref."}
	}
	signer, fault := parseSSHSigner(credential)
	zeroBytes(credential.PrivateKey)
	zeroBytes(credential.Passphrase)
	if fault != nil {
		return nil, ConnectionIdentity{}, fault
	}
	observedHostKey := ""
	config := &ssh.ClientConfig{
		User: target.SSHUser, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			observedHostKey = ssh.FingerprintSHA256(key)
			if descriptor.ExpectedHostKey == "" {
				return errHostKeyUnknown
			}
			if subtle.ConstantTimeCompare([]byte(observedHostKey), []byte(descriptor.ExpectedHostKey)) != 1 {
				return errHostKeyChanged
			}
			return nil
		},
		Timeout: 5 * time.Second,
	}
	dialer := c.Dialer
	if dialer == nil {
		dialer = &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 15 * time.Second}
	}
	connection, err := dialer.DialContext(ctx, "tcp", target.SSHAddress)
	if err != nil {
		return nil, ConnectionIdentity{}, sshConnectionFault(ctx, err, false)
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	clientConnection, channels, requests, err := ssh.NewClientConn(connection, target.SSHAddress, config)
	if err != nil {
		_ = connection.Close()
		if errors.Is(err, errHostKeyUnknown) {
			return nil, ConnectionIdentity{}, &probeFault{stage: "host_identity", code: "ssh_host_key_unknown", next: "Сверьте host key по доверенному каналу и сохраните pin."}
		}
		if errors.Is(err, errHostKeyChanged) || observedHostKey != "" && observedHostKey != descriptor.ExpectedHostKey {
			return nil, ConnectionIdentity{}, &probeFault{stage: "host_identity", code: "ssh_host_key_changed", next: "Остановитесь и перепроверьте identity сервера."}
		}
		return nil, ConnectionIdentity{}, sshConnectionFault(ctx, err, true)
	}
	_ = connection.SetDeadline(time.Time{})
	client := ssh.NewClient(clientConnection, channels, requests)
	endpoint, fault := inspectRemoteContext(ctx, client, target)
	if fault != nil {
		_ = client.Close()
		_ = connection.Close()
		return nil, ConnectionIdentity{}, fault
	}
	transport := engineTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		return openSSHStdio(ctx, client, connection, target)
	})
	closeAll := func() error {
		_ = client.Close()
		return connection.Close()
	}
	identity := ConnectionIdentity{HostKeySHA256: observedHostKey, ContextEndpoint: safeEndpointIdentity("ssh", target.Ref, target.Revision, endpoint)}
	return &httpEngineSession{client: engineHTTPClient(transport), transport: transport, close: closeAll}, identity, nil
}

func parseSSHSigner(credential SSHCredential) (ssh.Signer, *probeFault) {
	if len(credential.PrivateKey) == 0 || len(credential.PrivateKey) > 64<<10 || len(credential.Passphrase) > 4<<10 {
		return nil, &probeFault{stage: "transport_auth", code: "ssh_key_invalid", next: "Сохраните корректный private key."}
	}
	var signer ssh.Signer
	var err error
	if len(credential.Passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(credential.PrivateKey, credential.Passphrase)
	} else {
		signer, err = ssh.ParsePrivateKey(credential.PrivateKey)
	}
	if err == nil {
		return signer, nil
	}
	var passphraseMissing *ssh.PassphraseMissingError
	if errors.As(err, &passphraseMissing) {
		return nil, &probeFault{stage: "transport_auth", code: "ssh_passphrase_required", next: "Введите passphrase для сохранённого key."}
	}
	if len(credential.Passphrase) > 0 {
		return nil, &probeFault{stage: "transport_auth", code: "ssh_passphrase_invalid", next: "Проверьте passphrase SSH key."}
	}
	return nil, &probeFault{stage: "transport_auth", code: "ssh_key_invalid", next: "Сохраните корректный private key."}
}

func sshConnectionFault(ctx context.Context, err error, authenticatedHost bool) *probeFault {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &probeFault{stage: "transport_auth", code: "ssh_timeout", next: "Проверьте SSH endpoint и сеть.", retryable: true}
	}
	if authenticatedHost {
		return &probeFault{stage: "transport_auth", code: "ssh_authentication_failed", next: "Проверьте SSH user, key и passphrase."}
	}
	return &probeFault{stage: "transport_auth", code: "ssh_unreachable", next: "Проверьте SSH endpoint и сеть.", retryable: true}
}

func inspectRemoteContext(ctx context.Context, client *ssh.Client, target Target) (string, *probeFault) {
	command := remoteDockerCommand(target, "context", "inspect", target.DockerContext, "--format", "{{json .Endpoints.docker.Host}}")
	output, fault := runBoundedSSH(ctx, client, command, 4<<10)
	if fault != nil {
		return "", fault
	}
	return validateRemoteContextEndpoint(output)
}

func validateRemoteContextEndpoint(output string) (string, *probeFault) {
	var endpoint string
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &endpoint) != nil || len(endpoint) == 0 || len(endpoint) > 1024 || strings.ContainsAny(endpoint, "\x00\r\n") {
		return "", &probeFault{stage: "host_identity", code: "docker_context_invalid", next: "Проверьте зарегистрированный Docker context."}
	}
	if !strings.HasPrefix(endpoint, "unix://") && !strings.HasPrefix(endpoint, "npipe://") {
		return "", &probeFault{stage: "host_identity", code: "docker_context_redirected", next: "Remote context должен вести к локальному daemon этого SSH host."}
	}
	return endpoint, nil
}

func runBoundedSSH(ctx context.Context, client *ssh.Client, command string, maximum int64) (string, *probeFault) {
	session, err := client.NewSession()
	if err != nil {
		return "", &probeFault{stage: "transport_auth", code: "ssh_channel_unavailable", next: "Проверьте SSH server.", retryable: true}
	}
	defer session.Close()
	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", &probeFault{stage: "transport_auth", code: "ssh_channel_unavailable", next: "Проверьте SSH server.", retryable: true}
	}
	stderr := &boundedCapture{maximum: 4 << 10}
	session.Stderr = stderr
	if err := session.Start(command); err != nil {
		return "", &probeFault{stage: "host_identity", code: "docker_executable_not_found", next: "Проверьте Docker executable и context."}
	}
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, readErr := io.ReadAll(io.LimitReader(stdout, maximum+1))
		waitErr := session.Wait()
		if readErr == nil {
			readErr = waitErr
		}
		done <- result{body: body, err: readErr}
	}()
	select {
	case <-ctx.Done():
		_ = session.Close()
		return "", &probeFault{stage: "host_identity", code: "probe_timeout", next: "Проверьте SSH channel и Docker context.", retryable: true}
	case value := <-done:
		if value.err != nil || int64(len(value.body)) > maximum {
			if remoteExecutableMissing(stderr.Bytes(), value.err) {
				return "", &probeFault{stage: "host_identity", code: "docker_executable_not_found", next: "Проверьте абсолютный путь Docker executable."}
			}
			return "", &probeFault{stage: "host_identity", code: "docker_context_invalid", next: "Проверьте Docker executable и context."}
		}
		return string(value.body), nil
	}
}

type boundedCapture struct {
	data    []byte
	maximum int
}

func (w *boundedCapture) Write(value []byte) (int, error) {
	written := len(value)
	remaining := w.maximum - len(w.data)
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		w.data = append(w.data, value...)
	}
	return written, nil
}

func (w *boundedCapture) Bytes() []byte { return w.data }

func remoteExecutableMissing(stderr []byte, err error) bool {
	var exitError *ssh.ExitError
	if errors.As(err, &exitError) && exitError.ExitStatus() == 127 {
		return true
	}
	normalized := bytes.ToLower(stderr)
	defer zeroBytes(normalized)
	return bytes.Contains(normalized, []byte("command not found")) ||
		bytes.Contains(normalized, []byte("is not recognized as the name of a cmdlet"))
}

func remoteDockerCommand(target Target, arguments ...string) string {
	parts := append([]string{target.DockerExecutable}, arguments...)
	quoted := make([]string, len(parts))
	for index, part := range parts {
		quoted[index] = quoteRemote(part, target.RemoteShell)
	}
	if target.RemoteShell == "powershell" {
		return "& " + strings.Join(quoted, " ")
	}
	return "exec " + strings.Join(quoted, " ")
}

func quoteRemote(value, shell string) string {
	if shell == "powershell" {
		return "'" + strings.ReplaceAll(value, "'", "''") + "'"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func openSSHStdio(ctx context.Context, client *ssh.Client, underlying net.Conn, target Target) (net.Conn, error) {
	session, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stdout, err := session.StdoutPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	command := remoteDockerCommand(target, "--context", target.DockerContext, "system", "dial-stdio")
	if err := session.Start(command); err != nil {
		_ = session.Close()
		return nil, err
	}
	state := &sshCommandState{done: make(chan struct{})}
	go observeSSHCommand(session, stderr, state)
	connection := &sshStdioConn{ctx: ctx, reader: stdout, writer: stdin, session: session, underlying: underlying, command: state, local: namedAddr("adapter"), remote: namedAddr(target.Ref)}
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()
	return connection, nil
}

type namedAddr string

func (a namedAddr) Network() string { return "docker-stdio" }
func (a namedAddr) String() string  { return string(a) }

type sshStdioConn struct {
	ctx        context.Context
	reader     io.Reader
	writer     io.WriteCloser
	session    *ssh.Session
	underlying net.Conn
	command    *sshCommandState
	local      net.Addr
	remote     net.Addr
	once       sync.Once
}

type sshCommandState struct {
	done chan struct{}
	err  error
}

func observeSSHCommand(session *ssh.Session, stderr io.Reader, state *sshCommandState) {
	body, readErr := io.ReadAll(io.LimitReader(stderr, (8<<10)+1))
	_, _ = io.Copy(io.Discard, stderr)
	waitErr := session.Wait()
	if readErr != nil {
		waitErr = readErr
	}
	state.err = classifyDockerStdioFailure(body, waitErr)
	zeroBytes(body)
	close(state.done)
}

func classifyDockerStdioFailure(stderr []byte, waitErr error) error {
	normalized := bytes.ToLower(stderr)
	defer zeroBytes(normalized)
	for _, marker := range [][]byte{[]byte("permission denied"), []byte("access is denied"), []byte("permission_denied")} {
		if bytes.Contains(normalized, marker) {
			return errDockerPermission
		}
	}
	return waitErr
}

func (c *sshStdioConn) Read(value []byte) (int, error) {
	read, err := c.reader.Read(value)
	if read != 0 || !errors.Is(err, io.EOF) || c.command == nil {
		return read, err
	}
	select {
	case <-c.command.done:
		if errors.Is(c.command.err, errDockerPermission) {
			return 0, errDockerPermission
		}
		return read, err
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}
func (c *sshStdioConn) Write(value []byte) (int, error)   { return c.writer.Write(value) }
func (c *sshStdioConn) LocalAddr() net.Addr               { return c.local }
func (c *sshStdioConn) RemoteAddr() net.Addr              { return c.remote }
func (c *sshStdioConn) SetDeadline(value time.Time) error { return c.underlying.SetDeadline(value) }
func (c *sshStdioConn) SetReadDeadline(value time.Time) error {
	return c.underlying.SetReadDeadline(value)
}
func (c *sshStdioConn) SetWriteDeadline(value time.Time) error {
	return c.underlying.SetWriteDeadline(value)
}
func (c *sshStdioConn) Close() error {
	var err error
	c.once.Do(func() {
		_ = c.writer.Close()
		err = c.session.Close()
	})
	return err
}

func engineTransport(dial func(context.Context, string, string) (net.Conn, error)) *http.Transport {
	return &http.Transport{
		Proxy: nil, DialContext: dial, DisableCompression: true, ForceAttemptHTTP2: false,
		MaxConnsPerHost: 1, MaxIdleConns: 1, MaxIdleConnsPerHost: 1,
		IdleConnTimeout: 5 * time.Second, ResponseHeaderTimeout: 40 * time.Second,
	}
}

func engineHTTPClient(transport *http.Transport) *http.Client {
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("redirect rejected") },
	}
}

func safeEndpointIdentity(kind, ref string, revision int64, endpoint string) string {
	digest := sha256.Sum256([]byte("hl263-context-endpoint/v1\x00" + kind + "\x00" + ref + "\x00" + strconv.FormatInt(revision, 10) + "\x00" + endpoint))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
