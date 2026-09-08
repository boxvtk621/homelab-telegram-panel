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
