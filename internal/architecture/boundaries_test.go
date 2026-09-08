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
	if strings.Contains(string(module), "require") || strings.Contains(string(module), "replace") {
		t.Fatal("panel must build without external Go modules or a sibling repository")
	}
	allowed := map[string]bool{"mobileauth": true, "mobilecontract": true, "mobilecontrollerclient": true, "mobilegateway": true, "mobilegatewayassets": true, "mobilegatewaybootstrap": true, "mobilegatewayconfig": true, "observability": true, "buildinfo": true, "identity": true, "panel": true, "youtrack": true, "strictjson": true}
	allowed["cursoragent"] = true
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
				} else if strings.Contains(strings.Split(name, "/")[0], ".") {
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

// Legacy sources remain for parity review, but cannot be linked into the only
// executable. Traverse source imports rather than trusting package names/docs.
func TestExecutableCannotReachLegacyBotBoundary(t *testing.T) {
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
					case "internal/panel", "internal/youtrack", "internal/strictjson", "internal/buildinfo", "internal/mobilegatewayassets", "internal/cursoragent":
						visit(local)
					default:
						t.Errorf("runtime imports forbidden legacy dependency: %s -> %s", p, local)
					}
				}
			}
		}
	}
	visit("cmd/fixik-next-mobile-gateway")
	for _, p := range []string{"internal/panel", "internal/youtrack"} {
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
		for _, forbidden := range []string{"network_mode:", "depends_on:", "volumes:", "CONTROLLER_", "TELEGRAM_BOT_ID", "/internal/mobile/", "/api/v1/", "telegram-web-app.js", "@/components/mobile-workspace"} {
			if strings.Contains(string(data), forbidden) {
				t.Errorf("%s contains obsolete coupling %s", p, forbidden)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "internal/mobilegatewayassets/dist/telegram-web-app.js")); !os.IsNotExist(err) {
		t.Fatal("Telegram SDK must not be shipped in independent bundle")
	}
}
