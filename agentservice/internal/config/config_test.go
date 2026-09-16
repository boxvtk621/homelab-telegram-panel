package config

import "testing"

func lookup(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}

func TestLoadSeparatesDatabaseAndImportSecretsFromPanel(t *testing.T) {
	database := "postgres://agent:test@127.0.0.1:5432/agent_service?sslmode=disable"
	if _, err := Load("migrate", lookup(map[string]string{"AGENT_SERVICE_DATABASE_URL": database})); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("serve", lookup(map[string]string{
		"AGENT_SERVICE_DATABASE_URL":          database,
		"AGENT_SERVICE_SOCKET":                "/tmp/agent-service.sock",
		"AGENT_SERVICE_WORKER_TOKEN":          "test-worker-token-0000000000000001",
		"AGENT_SERVICE_SIGNER_PUBLIC_KEY":     "/tmp/signer.pem",
		"AGENT_SERVICE_DOCKER_ADAPTER_SOCKET": "/tmp/docker-adapter.sock",
		"AGENT_SERVICE_DOCKER_ADAPTER_TOKEN":  "adapter-token-00000000000000000001",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("import", lookup(map[string]string{
		"AGENT_SERVICE_DATABASE_URL":      database,
		"AGENT_SERVICE_REGISTRY":          "/tmp/registry.json",
		"AGENT_SERVICE_SIGNER_PUBLIC_KEY": "/tmp/signer.pem",
		"AGENT_SERVICE_IMPORT_SNAPSHOT":   "/tmp/import.json",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := Load("serve", lookup(map[string]string{
		"AGENT_SERVICE_DATABASE_URL": database,
		"AGENT_SERVICE_SOCKET":       "relative.sock",
	})); err == nil {
		t.Fatal("relative private socket accepted")
	}
	if _, err := Load("serve", lookup(map[string]string{
		"AGENT_SERVICE_DATABASE_URL":      database,
		"AGENT_SERVICE_SOCKET":            "/tmp/agent-service.sock",
		"AGENT_SERVICE_SIGNER_PUBLIC_KEY": "/tmp/signer.pem",
	})); err == nil {
		t.Fatal("registry operation signer accepted without worker authorization")
	}
	if _, err := Load("serve", lookup(map[string]string{
		"AGENT_SERVICE_DATABASE_URL":          database,
		"AGENT_SERVICE_SOCKET":                "/tmp/agent-service.sock",
		"AGENT_SERVICE_DOCKER_ADAPTER_SOCKET": "/tmp/docker-adapter.sock",
	})); err == nil {
		t.Fatal("docker adapter socket accepted without its service token")
	}
}
