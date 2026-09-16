// Command homelab-docker-adapter is the separate private R02 adapter process.
// R02 executes only a durable local fixture effect. R07 adds read-only Docker
// host probes and write-only credential provisioning; lifecycle effects remain
// later roadmap stages.
package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/dockeradapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/operationclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(executeIOContext(ctx, os.Args[1:], os.LookupEnv, os.Stdin, os.Stdout))
}

func execute(args []string, lookup func(string) (string, bool), output io.Writer) int {
	return executeIO(args, lookup, bytes.NewReader(nil), output)
}

func executeIO(args []string, lookup func(string) (string, bool), input io.Reader, output io.Writer) int {
	return executeIOContext(context.Background(), args, lookup, input, output)
}

func executeIOContext(ctx context.Context, args []string, lookup func(string) (string, bool), input io.Reader, output io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	if args[0] == "version" {
		_, _ = fmt.Fprintln(output, "homelab-docker-adapter", version)
		return 0
	}
	if args[0] == "host-serve" {
		return serveHostService(ctx, lookup, output)
	}
	if args[0] == "secret-put" || args[0] == "host-probe" {
		return executeHostCommand(args[0], lookup, input, output)
	}
	if args[0] != "journal-init" && args[0] != "journal-check" && args[0] != "fixture-once" {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	directory, directoryOK := lookup("DOCKER_ADAPTER_STATE_DIR")
	daemonID, daemonOK := lookup("DOCKER_ADAPTER_DAEMON_ID")
	instanceID, instanceOK := lookup("DOCKER_ADAPTER_INSTANCE_ID")
	if !directoryOK || !daemonOK || !instanceOK {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	if args[0] == "fixture-once" {
		return executeFixture(lookup, output, directory, daemonID, instanceID)
	}
	if args[0] == "journal-init" {
		executor, err := dockeradapter.Initialize(directory, daemonID, instanceID)
		if err != nil {
			_, _ = fmt.Fprintln(output, "JOURNAL_INITIALIZATION_REJECTED")
			return 1
		}
		executor.Close()
		_, _ = fmt.Fprintln(output, "JOURNAL_INITIALIZED")
		return 0
	}
	executor, err := dockeradapter.Open(directory, daemonID, instanceID)
	if err != nil {
		_, _ = fmt.Fprintln(output, "JOURNAL_UNAVAILABLE")
		return 1
	}
	executor.Close()
	_, _ = fmt.Fprintln(output, "JOURNAL_OK")
	return 0
}

func serveHostService(ctx context.Context, lookup func(string) (string, bool), output io.Writer) int {
	socket, socketOK := lookup("DOCKER_ADAPTER_SOCKET")
	token, tokenOK := lookup("DOCKER_ADAPTER_TOKEN")
	secretDirectory, directoryOK := lookup("DOCKER_ADAPTER_SECRET_DIR")
	masterKeyPath, keyOK := lookup("DOCKER_ADAPTER_MASTER_KEY_FILE")
	targetsPath, targetsOK := lookup("DOCKER_ADAPTER_TARGETS")
	if !socketOK || !tokenOK || !directoryOK || !keyOK || !targetsOK {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	masterKey, err := readMasterKey(masterKeyPath)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer zero(masterKey)
	secrets, err := dockeradapter.NewSecretStore(secretDirectory, masterKey)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer secrets.Close()
	targets, err := dockeradapter.NewFileTargets(targetsPath)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	handler, err := dockeradapter.NewHostService(token, secrets, dockeradapter.Prober{
		Connector: dockeradapter.Connector{Targets: targets, Credentials: secrets},
		Registry:  dockeradapter.RegistryCredentialProbe{Store: secrets}, Timeout: 45 * time.Second,
	})
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	listener, cleanup, err := openHostServiceListener(socket)
	if err != nil {
		_, _ = fmt.Fprintln(output, "SOCKET_UNAVAILABLE")
		return 1
	}
	defer cleanup()
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 20 * time.Second,
		WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	_, _ = fmt.Fprintln(output, "DOCKER_ADAPTER_HOST_SERVICE_STARTED")
	err = server.Serve(listener)
	close(done)
	if err != nil && err != http.ErrServerClosed {
		_, _ = fmt.Fprintln(output, "DOCKER_ADAPTER_HOST_SERVICE_FAILED")
		return 1
	}
	return 0
}

func openHostServiceListener(socket string) (net.Listener, func(), error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 || strings.ContainsAny(socket, "\x00\r\n") {
		return nil, nil, errors.New("invalid socket path")
	}
	directoryInfo, err := os.Lstat(filepath.Dir(socket))
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm() != 0o700 {
		return nil, nil, errors.New("private socket directory required")
	}
	stat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, nil, errors.New("private socket directory required")
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		return nil, nil, errors.New("socket path already exists")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, nil, err
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		_ = os.Remove(socket)
		return nil, nil, err
	}
	created, err := os.Lstat(socket)
	if err != nil || created.Mode()&os.ModeSocket == 0 || created.Mode().Perm() != 0o600 {
		_ = listener.Close()
		_ = os.Remove(socket)
		return nil, nil, errors.New("private socket unavailable")
	}
	cleanup := func() {
		_ = listener.Close()
		current, currentErr := os.Lstat(socket)
		if currentErr == nil && os.SameFile(created, current) {
			_ = os.Remove(socket)
		}
	}
	return listener, cleanup, nil
}

func executeHostCommand(command string, lookup func(string) (string, bool), input io.Reader, output io.Writer) int {
	owner, ownerOK := lookup("DOCKER_ADAPTER_OWNER")
	secretDirectory, directoryOK := lookup("DOCKER_ADAPTER_SECRET_DIR")
	masterKeyPath, keyOK := lookup("DOCKER_ADAPTER_MASTER_KEY_FILE")
	if !ownerOK || !directoryOK || !keyOK {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	masterKey, err := readMasterKey(masterKeyPath)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer zero(masterKey)
	secrets, err := dockeradapter.NewSecretStore(secretDirectory, masterKey)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer secrets.Close()
	if command == "secret-put" {
		kind, ok := lookup("DOCKER_ADAPTER_SECRET_KIND")
		operationID, operationOK := lookup("DOCKER_ADAPTER_OPERATION_ID")
		if !ok || !operationOK {
			_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
			return 2
		}
		return provisionSecret(owner, operationID, kind, secrets, input, output)
	}
	targetsPath, targetsOK := lookup("DOCKER_ADAPTER_TARGETS")
	if !targetsOK {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	targets, err := dockeradapter.NewFileTargets(targetsPath)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	raw, err := io.ReadAll(io.LimitReader(input, (16<<10)+1))
	if err != nil || len(raw) == 0 || len(raw) > 16<<10 || !strictjson.Valid(raw) {
		_, _ = fmt.Fprintln(output, "PROBE_INPUT_INVALID")
		return 2
	}
	defer zero(raw)
	var descriptor dockeradapter.HostDescriptor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&descriptor) != nil || decoder.Decode(new(any)) != io.EOF || dockeradapter.ValidateHostDescriptor(descriptor) != nil {
		_, _ = fmt.Fprintln(output, "PROBE_INPUT_INVALID")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	observation := (dockeradapter.Prober{
		Connector: dockeradapter.Connector{Targets: targets, Credentials: secrets},
		Registry:  dockeradapter.RegistryCredentialProbe{Store: secrets}, Timeout: 45 * time.Second,
	}).Probe(ctx, owner, descriptor)
	if json.NewEncoder(output).Encode(observation) != nil {
		return 1
	}
	if observation.Availability != "ready" {
		return 1
	}
	return 0
}

func provisionSecret(owner, operationID, kind string, store *dockeradapter.SecretStore, input io.Reader, output io.Writer) int {
	raw, err := io.ReadAll(io.LimitReader(input, (96<<10)+1))
	if err != nil || len(raw) == 0 || len(raw) > 96<<10 || !strictjson.Valid(raw) {
		_, _ = fmt.Fprintln(output, "SECRET_INPUT_INVALID")
		return 2
	}
	defer zero(raw)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var provision dockeradapter.SecretProvision
	switch kind {
	case "ssh":
		var value struct {
			PrivateKey []byte `json:"privateKey"`
			Passphrase []byte `json:"passphrase,omitempty"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
			_, _ = fmt.Fprintln(output, "SECRET_INPUT_INVALID")
			return 2
		}
		provision, err = store.ProvisionSSH(ctx, owner, operationID, value.PrivateKey, value.Passphrase)
		zero(value.PrivateKey)
		zero(value.Passphrase)
	case "registry":
		var value struct {
			Payload []byte `json:"payload"`
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
			_, _ = fmt.Fprintln(output, "SECRET_INPUT_INVALID")
			return 2
		}
		provision, err = store.ProvisionRegistry(ctx, owner, operationID, value.Payload)
		zero(value.Payload)
	default:
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	if err != nil {
		_, _ = fmt.Fprintln(output, "SECRET_STORE_UNAVAILABLE")
		return 1
	}
	if json.NewEncoder(output).Encode(provision) != nil {
		return 1
	}
	return 0
}

func readMasterKey(path string) ([]byte, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid master key path")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() < 32 || info.Size() > 65 {
		return nil, errors.New("private master key required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("private master key required")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("private master key required")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private master key required")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 66))
	if err != nil {
		return nil, errors.New("private master key required")
	}
	if len(raw) == 32 {
		return raw, nil
	}
	hexValue := raw
	if len(hexValue) == 65 && hexValue[64] == '\n' {
		hexValue = hexValue[:64]
	}
	if len(hexValue) != 64 {
		zero(raw)
		return nil, errors.New("private master key required")
	}
	decoded := make([]byte, hex.DecodedLen(len(hexValue)))
	written, err := hex.Decode(decoded, hexValue)
	zero(raw)
	if err != nil || written != 32 {
		zero(decoded)
		return nil, errors.New("private master key required")
	}
	return decoded[:written], nil
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func executeFixture(lookup func(string) (string, bool), output io.Writer, directory, daemonID, instanceID string) int {
	socket, socketOK := lookup("AGENT_SERVICE_SOCKET")
	ownerID, ownerOK := lookup("AGENT_SERVICE_OWNER")
	workerToken, workerTokenOK := lookup("AGENT_SERVICE_WORKER_TOKEN")
	workerID, workerOK := lookup("DOCKER_ADAPTER_WORKER_ID")
	loseAck, loseAckSet := lookup("DOCKER_ADAPTER_FIXTURE_LOSE_ACK")
	if !socketOK || !ownerOK || !workerTokenOK || !workerOK || loseAckSet && loseAck != "1" {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	executor, err := dockeradapter.Open(directory, daemonID, instanceID)
	if err != nil {
		_, _ = fmt.Fprintln(output, "JOURNAL_UNAVAILABLE")
		return 1
	}
	defer executor.Close()
	backend, err := dockeradapter.OpenFixtureBackend(directory, daemonID, instanceID, len(executor.Snapshot()) == 0)
	if err != nil {
		_, _ = fmt.Fprintln(output, "FIXTURE_STATE_UNAVAILABLE")
		return 1
	}
	if loseAckSet {
		backend.LoseNextAcknowledgement()
	}
	client, err := operationclient.NewWorker(socket, workerToken)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	work, err := client.Claim(ctx, ownerID, instanceID, workerID, 30*time.Second)
	if err != nil {
		var fault *operationclient.Fault
		if errors.As(err, &fault) && fault.Status == 404 {
			_, _ = fmt.Fprintln(output, "NO_OPERATION")
			return 0
		}
		_, _ = fmt.Fprintln(output, "CLAIM_UNAVAILABLE")
		return 1
	}
	if work.Intent.Target.HostID != daemonID || work.Intent.Target.NodeID != instanceID ||
		work.Intent.Kind != "adapter.fixture" || work.Intent.Step.Action != "adapter.fixture.apply" {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	request, err := operationclient.AdapterRequest(work)
	if err != nil {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	authority, err := operationclient.NewRemoteAuthority(client, ownerID, work)
	if err != nil {
		_, _ = fmt.Fprintln(output, "WORK_REJECTED")
		return 1
	}
	execution, executeErr := executeClaimedWork(ctx, executor, authority, operationclient.AdapterProof(work.Proof), backend, request, work.EffectState)
	proof := authority.Proof()
	if executeErr != nil && execution.JournalState == "sent" && execution.Proof.OperationID == work.Intent.OperationID {
		code := "effect_unknown"
		if _, err := client.Advance(ctx, ownerID, proof, "reconciling", "unknown", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_UNKNOWN")
		return 1
	}
	if execution.JournalState == "failed" && execution.Proof.OperationID == work.Intent.OperationID {
		code := execution.Result.ReceiptID
		if _, err := client.Advance(ctx, ownerID, proof, "failed", "failed", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	if executeErr != nil {
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	effectState, known := completionEffectState(work.EffectState, execution)
	if !known {
		_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
		return 1
	}
	code := execution.Result.ReceiptID
	if execution.Result.Outcome != "applied" {
		if _, err := client.Advance(ctx, ownerID, proof, "failed", "failed", &code); err != nil {
			_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
			return 1
		}
		_, _ = fmt.Fprintln(output, "OPERATION_FAILED")
		return 1
	}
	status, err := client.Advance(ctx, ownerID, proof, "succeeded", effectState, &code)
	if err != nil || status.Phase != "succeeded" || status.EffectState != effectState {
		_, _ = fmt.Fprintln(output, "RESULT_UNKNOWN")
		return 1
	}
	_, _ = fmt.Fprintf(output, "OPERATION_OK id=%s state=%s\n", work.Intent.OperationID, effectState)
	return 0
}

func executeClaimedWork(ctx context.Context, executor *dockeradapter.Executor, authority dockeradapter.Authority, proof dockeradapter.AuthorityProof, backend dockeradapter.Backend, request dockeradapter.Request, effectState string) (dockeradapter.Execution, error) {
	if effectState == "not_sent" {
		return executor.Execute(ctx, authority, proof, backend, request)
	}
	return executor.Recover(ctx, authority, proof, backend, request)
}

func completionEffectState(claimedEffectState string, execution dockeradapter.Execution) (string, bool) {
	effectState := execution.JournalState
	if execution.Replayed && claimedEffectState == "unknown" && effectState == "acknowledged" {
		// The exact acknowledged journal survived an ambiguous local completion,
		// while Agent Service conservatively retained unknown. Publishing it as
		// reconciled preserves the generic prohibition on unknown -> acknowledged.
		effectState = "reconciled"
	}
	return effectState, effectState == "acknowledged" || effectState == "reconciled"
}
