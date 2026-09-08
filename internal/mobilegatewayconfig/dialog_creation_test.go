package mobilegatewayconfig

import "testing"

func TestDialogCreationRequiresExactOptIn(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"", "false", "true", "TRUE", "1", " true "} {
		env := validEnvironment()
		if value != "" {
			env[EnvDialogCreationEnabled] = value
		}
		cfg, err := Load(mapLookup(env))
		valid := value == "" || value == "false" || value == "true"
		if valid && (err != nil || cfg.DialogCreationEnabled != (value == "true")) {
			t.Fatalf("opt-in %q: %+v %v", value, cfg, err)
		}
		if !valid && err == nil {
			t.Fatalf("accepted ambiguous opt-in %q", value)
		}
	}
}
