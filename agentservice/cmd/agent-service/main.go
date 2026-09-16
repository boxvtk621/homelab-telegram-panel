package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/config"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/hostadapterclient"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/httpapi"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/importer"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(execute(ctx, os.Args[1:], os.LookupEnv, os.Stdout))
}

func execute(ctx context.Context, args []string, lookup func(string) (string, bool), output io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	if args[0] == "version" {
		_, _ = fmt.Fprintln(output, "homelab-agent-service", version)
		return 0
	}
	if args[0] != "migrate" && args[0] != "import" && args[0] != "serve" {
		_, _ = fmt.Fprintln(output, "COMMAND_INVALID")
		return 2
	}
	cfg, err := config.Load(args[0], lookup)
	if err != nil {
		_, _ = fmt.Fprintln(output, "CONFIG_INVALID")
		return 2
	}
	database, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		_, _ = fmt.Fprintln(output, "DATABASE_UNAVAILABLE")
		return 1
	}
	defer database.Close()
	switch args[0] {
	case "migrate":
		if database.Migrate(ctx) != nil {
			_, _ = fmt.Fprintln(output, "MIGRATION_FAILED")
			return 1
		}
		_, _ = fmt.Fprintln(output, "MIGRATION_OK")
		return 0
	case "import":
		return importRegistry(ctx, cfg, database, output)
	case "serve":
		return serve(ctx, cfg, database, output)
	}
	return 2
}

func importRegistry(ctx context.Context, cfg config.Config, database *store.Store, output io.Writer) int {
	registryBytes, err := readBounded(cfg.Registry, 256<<10)
	if err != nil {
		_, _ = fmt.Fprintln(output, "IMPORT_INPUT_INVALID")
		return 2
	}
	signerBytes, err := readBounded(cfg.SignerPublicKey, 16<<10)
	if err != nil {
		_, _ = fmt.Fprintln(output, "IMPORT_INPUT_INVALID")
		return 2
	}
	snapshotBytes, err := readBounded(cfg.ImportSnapshot, 4<<20)
	if err != nil {
		_, _ = fmt.Fprintln(output, "IMPORT_INPUT_INVALID")
		return 2
	}
	verified, err := registry.Verify(registryBytes, signerBytes)
	if err != nil {
		_, _ = fmt.Fprintln(output, "REGISTRY_REJECTED")
		return 2
	}
	snapshot, err := importer.DecodeSnapshot(snapshotBytes, verified.Manifest)
	if err != nil {
		_, _ = fmt.Fprintln(output, "IMPORT_INPUT_INVALID")
		return 2
	}
	result, err := database.Import(ctx, verified, snapshot)
	if err != nil {
		_, _ = fmt.Fprintln(output, "IMPORT_FAILED")
		return 1
	}
	_, _ = fmt.Fprintf(output, "IMPORT_OK nodes=%d dialogs=%d created=%d\n", result.NodesSeen, result.DialogsSeen, result.MappingsCreated)
	return 0
}

func serve(ctx context.Context, cfg config.Config, database *store.Store, output io.Writer) int {
	if database.CheckSchema(ctx) != nil {
		_, _ = fmt.Fprintln(output, "SCHEMA_NOT_READY")
		return 1
	}
	var registrySigner []byte
	var err error
	if cfg.SignerPublicKey != "" {
		registrySigner, err = readBounded(cfg.SignerPublicKey, 16<<10)
		if err != nil || registry.ValidateSigner(registrySigner) != nil {
			_, _ = fmt.Fprintln(output, "SERVICE_INVALID")
			return 1
		}
	}
	listener, serviceLock, err := openServiceListener(cfg.Socket)
	if err != nil {
		_, _ = fmt.Fprintln(output, "SOCKET_UNAVAILABLE")
		return 1
	}
	defer func() {
		_ = listener.Close()
		serviceLock.removeSocket()
		serviceLock.close()
	}()
	var service *httpapi.Server
	if cfg.WorkerToken == "" {
		service, err = httpapi.New(database)
	} else {
		service, err = httpapi.NewWithCapabilities(database, cfg.WorkerToken, registrySigner)
	}
	if err != nil {
		_, _ = fmt.Fprintln(output, "SERVICE_INVALID")
		return 1
	}
	var adapter *hostadapterclient.Client
	if cfg.DockerAdapterSocket != "" {
		adapter, err = hostadapterclient.New(cfg.DockerAdapterSocket, cfg.DockerAdapterToken)
		if err != nil || service.SetHostAdapter(adapter) != nil {
			_, _ = fmt.Fprintln(output, "SERVICE_INVALID")
			return 1
		}
		defer adapter.Close()
	}
	server := &http.Server{
		Handler: service, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	_, _ = fmt.Fprintln(output, "AGENT_SERVICE_STARTED")
	err = server.Serve(listener)
	close(done)
	if err != nil && err != http.ErrServerClosed {
		_, _ = fmt.Fprintln(output, "AGENT_SERVICE_FAILED")
		return 1
	}
	return 0
}

type listenerLock struct {
	file       *os.File
	socketPath string
	socketInfo os.FileInfo
}

func (lock *listenerLock) removeSocket() {
	if lock == nil || lock.socketInfo == nil {
		return
	}
	current, err := os.Lstat(lock.socketPath)
	if err == nil && os.SameFile(current, lock.socketInfo) {
		_ = os.Remove(lock.socketPath)
	}
}

func (lock *listenerLock) close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

// openServiceListener recovers only an owner-private stale Unix socket while
// holding a persistent singleton lock. A live process or an unexpected path is
// never removed. The lock file intentionally survives a crash and restart.
func openServiceListener(socket string) (net.Listener, *listenerLock, error) {
	directory := filepath.Dir(socket)
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 || directoryInfo.Mode().Perm() != 0o700 {
		return nil, nil, fmt.Errorf("private socket directory required")
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || int(directoryStat.Uid) != os.Geteuid() {
		return nil, nil, fmt.Errorf("private socket directory required")
	}
	lockPath := socket + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, nil, err
	}
	lock := &listenerLock{file: lockFile}
	lockInfo, pathErr := os.Lstat(lockPath)
	openedInfo, openedErr := lockFile.Stat()
	if pathErr != nil || openedErr != nil {
		lock.close()
		return nil, nil, fmt.Errorf("service singleton unavailable")
	}
	lockStat, statOK := lockInfo.Sys().(*syscall.Stat_t)
	if !statOK || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 ||
		int(lockStat.Uid) != os.Geteuid() || !os.SameFile(lockInfo, openedInfo) || syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.close()
		return nil, nil, fmt.Errorf("service singleton unavailable")
	}
	if socketInfo, statErr := os.Lstat(socket); statErr == nil {
		socketStat, socketOK := socketInfo.Sys().(*syscall.Stat_t)
		if !socketOK || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 || int(socketStat.Uid) != os.Geteuid() {
			lock.close()
			return nil, nil, fmt.Errorf("unexpected service socket path")
		}
		connection, dialErr := net.DialTimeout("unix", socket, 100*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			lock.close()
			return nil, nil, fmt.Errorf("service already running")
		}
		current, currentErr := os.Lstat(socket)
		if currentErr != nil || !os.SameFile(socketInfo, current) || os.Remove(socket) != nil {
			lock.close()
			return nil, nil, fmt.Errorf("stale service socket changed")
		}
	} else if !os.IsNotExist(statErr) {
		lock.close()
		return nil, nil, fmt.Errorf("service socket unavailable")
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		lock.close()
		return nil, nil, err
	}
	unixListener, ok := listener.(*net.UnixListener)
	if !ok {
		_ = listener.Close()
		lock.close()
		return nil, nil, fmt.Errorf("unexpected service listener")
	}
	unixListener.SetUnlinkOnClose(false)
	createdInfo, err := os.Lstat(socket)
	createdStat, createdOwnerOK := createdInfoSys(createdInfo)
	if err != nil || createdInfo.Mode()&os.ModeSocket == 0 || !createdOwnerOK || int(createdStat.Uid) != os.Geteuid() {
		_ = listener.Close()
		lock.close()
		return nil, nil, fmt.Errorf("cannot inspect service socket")
	}
	lock.socketPath, lock.socketInfo = socket, createdInfo
	if err := os.Chmod(socket, 0o600); err != nil {
		_ = listener.Close()
		lock.removeSocket()
		lock.close()
		return nil, nil, err
	}
	socketInfo, err := os.Lstat(socket)
	if err != nil || !os.SameFile(createdInfo, socketInfo) || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 {
		_ = listener.Close()
		lock.removeSocket()
		lock.close()
		return nil, nil, fmt.Errorf("cannot inspect service socket")
	}
	return listener, lock, nil
}

func createdInfoSys(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, fmt.Errorf("input exceeds limit")
	}
	return data, nil
}
