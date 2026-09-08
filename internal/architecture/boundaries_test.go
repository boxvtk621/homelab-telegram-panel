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
	allowed := map[string]bool{"mobileauth": true, "mobilecontract": true, "mobilecontrollerclient": true, "mobilegateway": true, "mobilegatewayassets": true, "mobilegatewaybootstrap": true, "mobilegatewayconfig": true, "observability": true, "buildinfo": true, "identity": true}
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
