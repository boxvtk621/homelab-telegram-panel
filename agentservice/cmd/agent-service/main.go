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
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/config"
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
		return serve(ctx, cfg.Socket, database, output)
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

func serve(ctx context.Context, socket string, database *store.Store, output io.Writer) int {
	if database.CheckSchema(ctx) != nil {
		_, _ = fmt.Fprintln(output, "SCHEMA_NOT_READY")
		return 1
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		_, _ = fmt.Fprintln(output, "SOCKET_UNAVAILABLE")
		return 1
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		_, _ = fmt.Fprintln(output, "SOCKET_UNAVAILABLE")
		return 1
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err := os.Chmod(socket, 0o600); err != nil {
		_, _ = fmt.Fprintln(output, "SOCKET_UNAVAILABLE")
		return 1
	}
	handler, err := httpapi.New(database)
	if err != nil {
		_, _ = fmt.Fprintln(output, "SERVICE_INVALID")
		return 1
	}
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
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
