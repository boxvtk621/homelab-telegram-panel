package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestPanelHasNoControllerOrExternalGoDependencies(t *testing.T) {
	root := filepath.Join("..", "..")
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(module), "replace") || !strings.Contains(string(module), "require golang.org/x/crypto v0.55.0") {
		t.Fatal("root module may only add the pinned adapter SSH dependency and no sibling repository")
	}
	allowed := map[string]bool{"mobileauth": true, "mobilecontract": true, "mobilecontrollerclient": true, "mobilegateway": true, "mobilegatewayassets": true, "mobilegatewaybootstrap": true, "mobilegatewayconfig": true, "observability": true, "buildinfo": true, "identity": true, "panel": true, "youtrack": true, "strictjson": true, "harnessprotocol": true, "harnessbarrier": true, "harnessadapter": true}
	allowed["cursoragent"] = true
	allowed["harnessclient"] = true
	allowed["harnessrouter"] = true
	allowed["harnesstunnel"] = true
	allowed["agentserviceclient"] = true
	allowed["dockeradapter"] = true
	allowed["operationclient"] = true
	allowed["transcriptview"] = true
	allowed["historyreplica"] = true
	allowed["historysync"] = true
	allowed["logicaldelete"] = true
	for _, dir := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if local, ok := strings.CutPrefix(name, "github.com/boxvtk621/homelab-telegram-panel/internal/"); ok {
					if !allowed[local] {
						t.Errorf("%s imports forbidden local package %s", path, local)
					}
				} else if strings.Contains(strings.Split(name, "/")[0], ".") &&
					!(name == "golang.org/x/crypto/ssh" && strings.Contains(path, string(filepath.Separator)+"internal"+string(filepath.Separator)+"dockeradapter"+string(filepath.Separator))) {
					t.Errorf("%s imports external/controller dependency %s", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// Legacy sources remain for parity review, but cannot be linked into the Panel
// executable. Traverse source imports rather than trusting package names/docs.
func TestExecutableCannotReachLegacyOrDirectProviderBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var visit func(string)
	visit = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			p := filepath.Join(root, dir, e.Name())
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range f.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if local, ok := strings.CutPrefix(name, "github.com/boxvtk621/homelab-telegram-panel/"); ok {
					switch local {
					case "internal/panel", "internal/strictjson", "internal/buildinfo", "internal/mobilegatewayassets", "internal/harnessclient", "internal/harnessrouter", "internal/harnessprotocol", "internal/harnessbarrier", "internal/harnesstunnel", "internal/transcriptview", "internal/agentserviceclient", "internal/historyreplica", "internal/historysync", "internal/logicaldelete":
						visit(local)
					default:
						t.Errorf("runtime imports forbidden legacy dependency: %s -> %s", p, local)
					}
				}
			}
		}
	}
	visit("cmd/fixik-next-mobile-gateway")
	for _, p := range []string{"internal/panel", "internal/harnessrouter"} {
		if !seen[p] {
			t.Error("independent runtime missing", p)
		}
	}
}

func TestWebBundleAndComposeHaveNoBotConnection(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, p := range []string{"compose.yaml", "web/mobile-workspace/src/main.tsx", "web/mobile-workspace/src/panel.tsx", "web/mobile-workspace/src/panel-api.ts", "internal/mobilegatewayassets/dist/index.html"} {
		data, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"network_mode:", "depends_on:", "volumes:", "CONTROLLER_", "TELEGRAM_BOT_ID", "PANEL_YOUTRACK_", "PANEL_CURSOR_", "/internal/mobile/", "/api/v1/", "/api/v2/issues", "/api/v2/articles", "Персональный API-токен YouTrack", "telegram-web-app.js", "@/components/mobile-workspace"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("%s contains obsolete coupling %s", p, forbidden)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "internal/mobilegatewayassets/dist/telegram-web-app.js")); !os.IsNotExist(err) {
		t.Fatal("Telegram SDK must not be shipped in independent bundle")
	}
}

func TestAgentServiceIsASeparateDataBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	module, err := os.ReadFile(filepath.Join(root, "agentservice", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(module), "/agentservice") || !strings.Contains(string(module), "github.com/jackc/pgx/v5") {
		t.Fatal("agent-service PostgreSQL dependency is not isolated in its own module")
	}
	for _, directory := range []string{"internal/panel", "internal/agentserviceclient", "cmd/fixik-next-mobile-gateway"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, forbidden := range []string{"github.com/jackc/pgx", "AGENT_SERVICE_DATABASE_URL", "docker.sock", "SIGNER_PRIVATE"} {
				if strings.Contains(string(data), forbidden) {
					t.Errorf("%s crosses the agent-service boundary via %s", path, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	for _, directory := range []string{"agentservice/internal/importer", "agentservice/internal/registry"} {
		entries, err := os.ReadDir(filepath.Join(root, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, directory, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, imp := range file.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if name == "net" || name == "net/http" || name == "os/exec" {
					t.Errorf("read-only importer has an effectful import: %s -> %s", path, name)
				}
			}
		}
	}

	migration, err := os.ReadFile(filepath.Join(root, "agentservice", "migrations", "001_r01_foundation.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"generation", "checkpoint_id", "operations", "action_journal", "desired_state", "router_projection"} {
		if strings.Contains(strings.ToLower(string(migration)), forbidden) {
			t.Errorf("R01 migration contains deferred R02 field %s", forbidden)
		}
	}
}

func TestDockerAdapterIsASeparateEffectBoundary(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, directory := range []string{"internal/panel", "internal/agentserviceclient", "cmd/fixik-next-mobile-gateway"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, forbidden := range []string{"internal/dockeradapter", "internal/operationclient", "DOCKER_ADAPTER_", "operation-workers", "docker.sock"} {
				if strings.Contains(string(data), forbidden) {
					t.Errorf("%s crosses the adapter boundary via %s", path, forbidden)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, directory := range []string{"internal/panel", "internal/agentserviceclient", "cmd/fixik-next-mobile-gateway"} {
		err := filepath.WalkDir(filepath.Join(root, directory), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				name, _ := strconv.Unquote(imp.Path.Value)
				if name == "golang.org/x/crypto/ssh" || name == "os/exec" {
					t.Errorf("consumer crosses Docker transport boundary: %s -> %s", path, name)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{
		filepath.Join(root, "internal", "dockeradapter", "executor.go"),
		filepath.Join(root, "internal", "dockeradapter", "fixture_backend.go"),
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"os/exec", "net/http", "github.com/docker", "jackc/pgx", "internal/panel"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("R02 fixture adapter contains a deferred effect dependency: %s -> %s", path, forbidden)
			}
		}
	}
}
