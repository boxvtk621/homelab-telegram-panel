package main

import "testing"

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
