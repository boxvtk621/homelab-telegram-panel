package panel

import "testing"

func TestNoBotOrSecretConfigurationNeeded(t *testing.T) {
	env := map[string]string{"PANEL_LISTEN": "0.0.0.0:18080", "PANEL_PUBLIC_ORIGIN": "https://panel.example.test", "PANEL_YOUTRACK_URL": "https://youtrack.example.test", "PANEL_PROJECT_ID": "0-1", "PANEL_PROJECT_KEY": "HL", "PANEL_OWNER_LOGIN": "owner"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	if cfg, err := Load(lookup); err != nil || cfg.Writes {
		t.Fatal(cfg, err)
	}
	for _, v := range []string{"/panel", "/tools/panel"} {
		env["PANEL_BASE_PATH"] = v
		if cfg, err := Load(lookup); err != nil || cfg.BasePath != v {
			t.Fatal("valid mount rejected", err)
		}
	}
	for _, v := range []string{"/", "/panel/", "//panel", "/panel/..", "/panel%2f", "/panel?x", "https://evil.test"} {
		env["PANEL_BASE_PATH"] = v
		if _, err := Load(lookup); err == nil {
			t.Fatal("unsafe mount accepted", v)
		}
	}
	delete(env, "PANEL_BASE_PATH")
	env["PANEL_TLS_CERTIFICATE"] = "/synthetic/panel.crt"
	if _, err := Load(lookup); err == nil {
		t.Fatal("partial TLS configuration accepted")
	}
	env["PANEL_TLS_KEY"] = "/synthetic/panel.key"
	if cfg, err := Load(lookup); err != nil || cfg.TLSKey != env["PANEL_TLS_KEY"] {
		t.Fatal("complete TLS paths rejected", err)
	}
	delete(env, "PANEL_TLS_CERTIFICATE")
	delete(env, "PANEL_TLS_KEY")
	for _, key := range []string{"PANEL_OWNER_LOGIN", "PANEL_YOUTRACK_URL", "PANEL_PUBLIC_ORIGIN", "PANEL_PROJECT_ID"} {
		before := env[key]
		delete(env, key)
		if _, err := Load(lookup); err == nil {
			t.Error("missing boundary allowed", key)
		}
		env[key] = before
	}
	env["FIXIK_NEXT_MOBILE_CONTROLLER_BUSINESS_SOCKET"] = "/some/socket"
	if _, err := Load(lookup); err == nil {
		t.Fatal("legacy deployment silently accepted")
	}
}

func TestHarnessTrustPathsAreExplicitAndAllOrNone(t *testing.T) {
	env := map[string]string{"PANEL_LISTEN": "0.0.0.0:18080", "PANEL_PUBLIC_ORIGIN": "https://panel.example.test", "PANEL_YOUTRACK_URL": "https://youtrack.example.test", "PANEL_PROJECT_ID": "0-1", "PANEL_PROJECT_KEY": "HL", "PANEL_OWNER_LOGIN": "owner"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	keys := []string{"PANEL_HARNESS_REGISTRY", "PANEL_HARNESS_SIGNER_PUBLIC_KEY", "PANEL_HARNESS_CA", "PANEL_HARNESS_CLIENT_CERT", "PANEL_HARNESS_CLIENT_KEY"}
	for _, key := range keys {
		env[key] = "/synthetic/" + key
	}
	env["PANEL_HARNESS_ROUTER_STATE"] = "/synthetic/router/state.json"
	env["PANEL_HARNESS_ROUTER_SOCKET"] = "/synthetic/router/control.sock"
	if cfg, err := Load(lookup); err != nil || cfg.Harness.Registry != env[keys[0]] {
		t.Fatal("complete explicit paths", err)
	}
	for _, key := range keys {
		saved := env[key]
		delete(env, key)
		if _, err := Load(lookup); err == nil {
			t.Fatal("partial trust configuration accepted", key)
		}
		env[key] = "relative/path"
		if _, err := Load(lookup); err == nil {
			t.Fatal("relative trust path accepted", key)
		}
		env[key] = saved
	}
	for _, key := range []string{"PANEL_HARNESS_ROUTER_STATE", "PANEL_HARNESS_ROUTER_SOCKET"} {
		saved := env[key]
		delete(env, key)
		if _, err := Load(lookup); err == nil {
			t.Fatal("partial Router state configuration accepted", key)
		}
		env[key] = saved
	}
	env["PANEL_HARNESS_ROUTER_SOCKET"] = "/other/control.sock"
	if _, err := Load(lookup); err == nil {
		t.Fatal("Router state and control socket in different directories accepted")
	}
	delete(env, "PANEL_HARNESS_ROUTER_SOCKET")
	delete(env, "PANEL_HARNESS_ROUTER_STATE")
	for _, key := range keys {
		delete(env, key)
	}
	env["PANEL_HARNESS_ROUTER_STATE"] = "/synthetic/router/state.json"
	env["PANEL_HARNESS_ROUTER_SOCKET"] = "/synthetic/router/control.sock"
	if _, err := Load(lookup); err == nil {
		t.Fatal("Router state without Harness trust accepted")
	}
}
