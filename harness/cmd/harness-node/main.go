package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
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

	"github.com/boxvtk621/homelab-telegram-panel/harness/adapters/codex"
	"github.com/boxvtk621/homelab-telegram-panel/harness/adapters/cursor"
	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	harnessserver "github.com/boxvtk621/homelab-telegram-panel/harness/server"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
)

type config struct {
	Listen                   string        `json:"listen"`
	NodeID                   string        `json:"nodeId"`
	OwnerID                  string        `json:"ownerId"`
	DataDir                  string        `json:"dataDir"`
	RegistryVersion          int64         `json:"registryVersion"`
	CertificateFile          string        `json:"certificateFile"`
	KeyFile                  string        `json:"keyFile"`
	ClientCAFile             string        `json:"clientCAFile"`
	GatewayCertificateSHA256 string        `json:"gatewayCertificateSHA256"`
	PolicyFile               string        `json:"policyFile"`
	ToolManifestFile         string        `json:"toolManifestFile"`
	PolicyRevision           string        `json:"policyRevision"`
	Adapter                  string        `json:"adapter"`
	Cursor                   *cursorConfig `json:"cursor,omitempty"`
	Codex                    *codexConfig  `json:"codex,omitempty"`
}

type cursorConfig struct {
	NodeExecutable   string `json:"nodeExecutable"`
	WorkerEntrypoint string `json:"workerEntrypoint"`
	StateDir         string `json:"stateDir"`
	APIKeyFile       string `json:"apiKeyFile"`
	Model            string `json:"model"`
}

type codexConfig struct {
	Executable string `json:"executable"`
	StateDir   string `json:"stateDir"`
	WorkingDir string `json:"workingDir"`
	CodexHome  string `json:"codexHome"`
	HomeDir    string `json:"homeDir"`
	Model      string `json:"model"`
	Effort     string `json:"effort"`
}

type providerAdapter interface {
	harnessadapter.Adapter
	Close() error
}

func main() {
	path := flag.String("config", "", "path to the node JSON configuration")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 || os.Geteuid() == 0 {
		fmt.Fprintln(os.Stderr, "HARNESS_CONFIG_INVALID")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if err := serve(ctx, *path); err != nil {
		// Provider errors and configuration can contain credentials or prompts.
		fmt.Fprintln(os.Stderr, "HARNESS_START_OR_SERVE_FAILED")
		os.Exit(1)
	}
}

func boundedFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, errors.New("file exceeds limit")
	}
	return content, nil
}

func serve(ctx context.Context, path string) error {
	raw, err := boundedFile(path, 64<<10)
	if err != nil {
		return err
	}
	var cfg config
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("multiple configuration values")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil || net.ParseIP(host) == nil || port == "" {
		return errors.New("explicit listen IP and port required")
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertificateFile, cfg.KeyFile)
	if err != nil {
		return err
	}
	caPEM, err := boundedFile(cfg.ClientCAFile, 64<<10)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return errors.New("client CA is invalid")
	}
	policies := filePolicy{cfg.PolicyFile, cfg.ToolManifestFile, cfg.PolicyRevision}
	if _, err := policies.Current(ctx, cfg.NodeID); err != nil {
		return err
	}
	artifacts := node.NewArtifactIngress()
	adapter, err := openProviderAdapter(cfg, artifacts)
	if err != nil {
		return err
	}
	defer adapter.Close()
	authority, err := node.Open(ctx, node.Config{
		DataDir: cfg.DataDir, NodeID: cfg.NodeID, OwnerID: cfg.OwnerID,
		RegistryVersion: cfg.RegistryVersion, Adapter: adapter, Policies: policies, Artifacts: artifacts,
	})
	if err != nil {
		return err
	}
	defer authority.Close()
	handler, err := harnessserver.New(harnessserver.Config{
		NodeID: cfg.NodeID, GatewayCertificateSHA256: cfg.GatewayCertificateSHA256,
	}, authority)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.Listen, Handler: handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10,
		// SSE outlives individual HTTP commands and can span long agent runs.
		WriteTimeout: 0, ErrorLog: log.New(io.Discard, "", 0),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate},
			ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert},
	}
	done := make(chan struct{})
	defer close(done)
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
	fmt.Fprintln(os.Stdout, "HARNESS_STARTING")
	err = server.ListenAndServeTLS("", "")
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func openProviderAdapter(cfg config, artifacts node.ArtifactSink) (providerAdapter, error) {
	switch selectedAdapter(cfg) {
	case string(harnessadapter.KindCursor):
		if cfg.Cursor == nil || cfg.Codex != nil {
			return nil, errors.New("exactly one Cursor adapter config is required")
		}
		secretInfo, err := os.Lstat(cfg.Cursor.APIKeyFile)
		if err != nil || !secretInfo.Mode().IsRegular() || secretInfo.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("private Cursor key file required")
		}
		key, err := boundedFile(cfg.Cursor.APIKeyFile, 4096)
		if err != nil {
			return nil, err
		}
		apiKey := strings.TrimSuffix(strings.TrimSuffix(string(key), "\n"), "\r")
		if apiKey == "" || strings.ContainsAny(apiKey, "\r\n\x00") {
			return nil, errors.New("Cursor key is invalid")
		}
		return cursor.New(cursor.Config{
			NodeExecutable: cfg.Cursor.NodeExecutable, WorkerEntrypoint: cfg.Cursor.WorkerEntrypoint,
			StateDir: cfg.Cursor.StateDir, APIKey: apiKey, Model: cfg.Cursor.Model,
			OperationTimeout: 30 * time.Second, MaxFrameBytes: 8 << 20,
		}, artifacts)
	case string(harnessadapter.KindCodex):
		if cfg.Codex == nil || cfg.Cursor != nil || !filepath.IsAbs(cfg.Codex.HomeDir) || !filepath.IsAbs(cfg.Codex.CodexHome) ||
			strings.ContainsAny(cfg.Codex.HomeDir+cfg.Codex.CodexHome, "\x00\r\n") {
			return nil, errors.New("exactly one Codex adapter config is required")
		}
		return codex.New(codex.Config{
			Executable: cfg.Codex.Executable, Environment: []string{
				"HOME=" + cfg.Codex.HomeDir,
				"CODEX_HOME=" + cfg.Codex.CodexHome,
				"PATH=/opt/codex/node_modules/.bin:/usr/local/bin:/usr/bin:/bin",
				"SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
			},
			StateDir: cfg.Codex.StateDir, WorkingDir: cfg.Codex.WorkingDir,
			Model: cfg.Codex.Model, Effort: cfg.Codex.Effort,
			OperationTimeout: 30 * time.Second, MaxFrameBytes: 8 << 20,
		}, artifacts)
	default:
		return nil, errors.New("valid adapter selector is required")
	}
}

func selectedAdapter(cfg config) string {
	if cfg.Adapter == "" && cfg.Cursor != nil && cfg.Codex == nil {
		// Cursor was the only adapter before the selector existed. Preserve
		// those already-enrolled host configs; all newly generated configs are
		// explicit and Codex never falls through this compatibility path.
		return string(harnessadapter.KindCursor)
	}
	return cfg.Adapter
}

type filePolicy struct{ contentPath, manifestPath, revision string }

func (source filePolicy) Current(ctx context.Context, _ string) (harnessadapter.PolicySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	content, err := boundedFile(source.contentPath, 64<<10)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	manifest, err := boundedFile(source.manifestPath, 64<<10)
	if err != nil {
		return harnessadapter.PolicySnapshot{}, err
	}
	var tools []json.RawMessage
	if json.Unmarshal(manifest, &tools) != nil || tools == nil || len(tools) != 0 {
		return harnessadapter.PolicySnapshot{}, errors.New("chat alpha requires an empty tool manifest")
	}
	contentHash, manifestHash := sha256.Sum256(content), sha256.Sum256(manifest)
	policy := harnessadapter.PolicySnapshot{
		Revision: source.revision, Content: content, ToolManifest: manifest,
		ContentHash: hex.EncodeToString(contentHash[:]), ToolManifestHash: hex.EncodeToString(manifestHash[:]),
		ApprovalMode: harnessadapter.ApprovalModeDeny,
	}
	policy.EffectiveHash = harnessadapter.EffectivePolicyHash(policy)
	return harnessadapter.PreparePolicySnapshot(policy)
}
