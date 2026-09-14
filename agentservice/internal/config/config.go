// Package config validates agent-service configuration without logging secrets.
package config

import (
	"errors"
	"net/url"
	"path/filepath"
	"strings"
)

type Config struct {
	DatabaseURL     string
	Socket          string
	Registry        string
	SignerPublicKey string
	ImportSnapshot  string
	WorkerToken     string
}

func Load(command string, lookup func(string) (string, bool)) (Config, error) {
	var result Config
	result.DatabaseURL, _ = lookup("AGENT_SERVICE_DATABASE_URL")
	if !validDatabaseURL(result.DatabaseURL) {
		return Config{}, errors.New("database configuration is missing or invalid")
	}
	switch command {
	case "migrate":
		return result, nil
	case "serve":
		result.Socket, _ = lookup("AGENT_SERVICE_SOCKET")
		result.WorkerToken, _ = lookup("AGENT_SERVICE_WORKER_TOKEN")
		result.SignerPublicKey, _ = lookup("AGENT_SERVICE_SIGNER_PUBLIC_KEY")
		if !validPath(result.Socket) || len(result.Socket) > 100 {
			return Config{}, errors.New("service socket is missing or invalid")
		}
		if result.WorkerToken != "" && (!validWorkerToken(result.WorkerToken) || len(result.WorkerToken) < 32) {
			return Config{}, errors.New("worker token is invalid")
		}
		if result.SignerPublicKey != "" && (!validPath(result.SignerPublicKey) || result.WorkerToken == "") {
			return Config{}, errors.New("registry operation signer is invalid")
		}
		return result, nil
	case "import":
		result.Registry, _ = lookup("AGENT_SERVICE_REGISTRY")
		result.SignerPublicKey, _ = lookup("AGENT_SERVICE_SIGNER_PUBLIC_KEY")
		result.ImportSnapshot, _ = lookup("AGENT_SERVICE_IMPORT_SNAPSHOT")
		if !validPath(result.Registry) || !validPath(result.SignerPublicKey) ||
			!validPath(result.ImportSnapshot) || result.Registry == result.SignerPublicKey ||
			result.Registry == result.ImportSnapshot || result.SignerPublicKey == result.ImportSnapshot {
			return Config{}, errors.New("import paths are missing or invalid")
		}
		return result, nil
	default:
		return Config{}, errors.New("unsupported command")
	}
}

func validWorkerToken(value string) bool {
	if len(value) > 128 || strings.TrimSpace(value) != value {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._:@-", character) {
			continue
		}
		return false
	}
	return value != ""
}

func validDatabaseURL(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "postgres" || parsed.Scheme == "postgresql") &&
		parsed.Hostname() != "" && parsed.User != nil && parsed.Path != "" && parsed.Fragment == ""
}

func validPath(value string) bool {
	return value != "" && filepath.IsAbs(value) && filepath.Clean(value) == value &&
		strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n")
}
