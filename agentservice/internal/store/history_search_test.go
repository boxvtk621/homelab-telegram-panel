package store

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

func TestParseHistorySearchQueryANDWordsAndQuotedPhrases(t *testing.T) {
	archived := false
	spec, err := ParseHistorySearchQuery(model.HistorySearchQuery{
		Q:      "  Ошибка   UUID \"Exact   Phrase\"  ",
		NodeID: "10000000-0000-4000-8000-000000000001", Archived: &archived,
	})
	if err != nil || spec.Words != "ошибка uuid" || len(spec.Phrases) != 1 || spec.Phrases[0] != "exact phrase" ||
		spec.Query.Q != `ошибка uuid "exact phrase"` {
		t.Fatalf("spec=%+v err=%v", spec, err)
	}
	first := HistorySearchQueryHash(spec)
	second, err := ParseHistorySearchQuery(model.HistorySearchQuery{
		Q: "ошибка uuid \"exact phrase\"", NodeID: spec.Query.NodeID, Archived: &archived,
	})
	if err != nil || first != HistorySearchQueryHash(second) {
		t.Fatalf("canonical hash drift: first=%s second=%s err=%v", first, HistorySearchQueryHash(second), err)
	}
}

func TestParseHistorySearchQueryRejectsEmptyOrUnclosedPhrase(t *testing.T) {
	for _, value := range []string{"", "   ", `word "open`, `word ""`} {
		if _, err := ParseHistorySearchQuery(model.HistorySearchQuery{Q: value}); err == nil {
			t.Fatalf("accepted invalid query %q", value)
		}
	}
}

func TestTruncateUTF8PreservesRuneBoundary(t *testing.T) {
	value := truncateUTF8("абвг", 5)
	if value != "аб" {
		t.Fatalf("truncated=%q", value)
	}
}

func TestSearchTextSegmentsBoundVectorInputAndOverlapPhrase(t *testing.T) {
	text := []byte(strings.Repeat("я", 140_000) + " exact boundary phrase " + strings.Repeat("z", 300_000))
	segments := searchTextSegments(text)
	if len(segments) < 2 {
		t.Fatalf("segments=%d", len(segments))
	}
	found := false
	for _, segment := range segments {
		if len(segment) > 256*1024 || !utf8.Valid(segment) {
			t.Fatalf("invalid segment bytes=%d", len(segment))
		}
		if strings.Contains(string(segment), "exact boundary phrase") {
			found = true
		}
	}
	if !found {
		t.Fatal("overlap lost exact phrase")
	}
}
