package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
)

func TestSelectedAdapterPreservesLegacyCursorConfigOnly(t *testing.T) {
	tests := []struct {
		name string
		cfg  config
		want string
	}{
		{name: "legacy cursor", cfg: config{Cursor: &cursorConfig{}}, want: "cursor"},
		{name: "explicit cursor", cfg: config{Adapter: "cursor", Cursor: &cursorConfig{}}, want: "cursor"},
		{name: "explicit codex", cfg: config{Adapter: "codex", Codex: &codexConfig{}}, want: "codex"},
		{name: "missing selector for codex", cfg: config{Codex: &codexConfig{}}, want: ""},
		{name: "ambiguous legacy config", cfg: config{Cursor: &cursorConfig{}, Codex: &codexConfig{}}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectedAdapter(test.cfg); got != test.want {
				t.Fatalf("selected adapter = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFilePolicyPreservesLegacyDeny(t *testing.T) {
	for _, test := range []struct {
		name, manifest string
	}{
		{name: "compact", manifest: "[]\n"},
		{name: "formatted", manifest: "[  ]\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			contentPath, manifestPath := filepath.Join(root, "policy.txt"), filepath.Join(root, "tools.json")
			if err := os.WriteFile(contentPath, []byte("policy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, []byte(test.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			policy, err := (filePolicy{contentPath: contentPath, manifestPath: manifestPath, revision: "test@1"}).Current(context.Background(), "node")
			if err != nil || policy.ApprovalMode != harnessadapter.ApprovalModeDeny {
				t.Fatalf("policy = %#v, %v", policy, err)
			}
		})
	}
}

func TestFilePolicyAcceptsOnlyExactExplicitOnceManifestForAdapter(t *testing.T) {
	for _, test := range []struct{ name, adapter, manifest string }{
		{name: "codex", adapter: "codex", manifest: codexExplicitToolManifest},
		{name: "cursor", adapter: "cursor", manifest: cursorExplicitToolManifest},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			contentPath, manifestPath := filepath.Join(root, "policy.txt"), filepath.Join(root, "tools.json")
			if err := os.WriteFile(contentPath, []byte("policy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, []byte(test.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			policy, err := (filePolicy{contentPath: contentPath, manifestPath: manifestPath, revision: "agent-tools-v1", approvalMode: "explicit_once", adapter: test.adapter}).Current(context.Background(), "node")
			if err != nil || policy.ApprovalMode != harnessadapter.ApprovalModeExplicitOnce || string(policy.ToolManifest) != test.manifest {
				t.Fatalf("policy = %#v, %v", policy, err)
			}
		})
	}
}

func TestFilePolicyFailsClosedForMismatchedModeOrManifest(t *testing.T) {
	for _, test := range []struct{ name, adapter, mode, manifest string }{
		{name: "explicit missing lf", adapter: "codex", mode: "explicit_once", manifest: strings.TrimSuffix(codexExplicitToolManifest, "\n")},
		{name: "wrong adapter", adapter: "cursor", mode: "explicit_once", manifest: codexExplicitToolManifest},
		{name: "wrong order", adapter: "codex", mode: "explicit_once", manifest: `[{"name":"codex.file_change"},{"name":"codex.command"}]` + "\n"},
		{name: "unknown tool", adapter: "codex", mode: "explicit_once", manifest: `[{"name":"unsafe"}]`},
		{name: "deny nonempty", adapter: "codex", mode: "deny", manifest: codexExplicitToolManifest},
		{name: "invalid mode", adapter: "codex", mode: "allow", manifest: "[]\n"},
		{name: "invalid", adapter: "codex", mode: "deny", manifest: `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			contentPath, manifestPath := filepath.Join(root, "policy.txt"), filepath.Join(root, "tools.json")
			if err := os.WriteFile(contentPath, []byte("policy\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, []byte(test.manifest), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := (filePolicy{contentPath: contentPath, manifestPath: manifestPath, revision: "test@1", approvalMode: test.mode, adapter: test.adapter}).Current(context.Background(), "node"); err == nil {
				t.Fatal("unsafe manifest was accepted")
			}
		})
	}
}
