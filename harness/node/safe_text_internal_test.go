package node

import (
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

func TestSafeTextAggregateLimit(t *testing.T) {
	tests := []struct {
		name       string
		captured   int64
		size       int64
		incomplete bool
		want       bool
	}{
		{name: "exact aggregate limit", captured: transcriptview.MaximumTextBytes - 1, size: 1, want: true},
		{name: "above aggregate limit", captured: transcriptview.MaximumTextBytes, size: 1},
		{name: "single source above limit", size: transcriptview.MaximumTextBytes + 1},
		{name: "producer incomplete", size: 1, incomplete: true},
		{name: "invalid negative accounting", captured: -1, size: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := safeTextCanComplete(test.captured, test.size, test.incomplete); got != test.want {
				t.Fatalf("safeTextCanComplete(%d,%d,%v)=%v want=%v", test.captured, test.size, test.incomplete, got, test.want)
			}
		})
	}
}
