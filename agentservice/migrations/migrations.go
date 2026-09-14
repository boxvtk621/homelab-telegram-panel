// Package migrations exposes the additive R01 PostgreSQL migrations.
package migrations

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
)

//go:embed *.sql
var files embed.FS

type Migration struct {
	Version int64
	Name    string
	SQL     string
}

func All() ([]Migration, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	result := make([]Migration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(entry.Name(), "_")
		version, parseErr := strconv.ParseInt(prefix, 10, 64)
		body, readErr := files.ReadFile(entry.Name())
		if !ok || parseErr != nil || version < 1 || readErr != nil {
			return nil, fmt.Errorf("invalid migration %q", entry.Name())
		}
		result = append(result, Migration{Version: version, Name: entry.Name(), SQL: string(body)})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Version < result[j].Version })
	for index, migration := range result {
		if migration.Version != int64(index+1) {
			return nil, fmt.Errorf("migration sequence is not contiguous at %q", migration.Name)
		}
	}
	return result, nil
}
