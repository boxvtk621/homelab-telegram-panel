package mobilegatewayconfig

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

func TestLoadAcceptsOnlyExplicitStandaloneBoundary(t *testing.T) {
	t.Parallel()

	configuration, err := Load(mapLookup(validEnvironment()))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if configuration.ListenAddress != "127.0.0.1:18080" || configuration.PublicOrigin != "https://mobile.example.test" {
		t.Fatalf("unexpected network boundary: %#v", configuration)
	}
	if configuration.ControllerBusinessSocket != "/run/fixik-next/controller-business.sock" ||
		configuration.ControllerHealthSocket != "/run/fixik-next/controller-health.sock" ||
		configuration.ControllerControlSocket != "/run/fixik-next/controller-control.sock" ||
		configuration.ControllerRecoverySocket != "/run/fixik-next/controller-recovery.sock" {
		t.Fatalf("unexpected Controller sockets: %#v", configuration)
	}
	if configuration.TelegramBotID != 100001 || configuration.TelegramOwnerID != 200002 ||
		configuration.TelegramEnvironment != mobileauth.EnvironmentTest {
		t.Fatalf("unexpected Telegram boundary: %#v", configuration)
	}
	if configuration.ShutdownTimeout != defaultShutdownTimeout {
		t.Fatalf("ShutdownTimeout = %s, want %s", configuration.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestLoadRequiresEveryAuthorityAndNetworkValue(t *testing.T) {
	t.Parallel()

	required := []string{
		EnvListenAddress,
		EnvPublicOrigin,
		EnvControllerBusinessSocket,
		EnvControllerHealthSocket,
		EnvControllerControlSocket,
		EnvControllerRecoverySocket,
		EnvTelegramBotID,
		EnvTelegramOwnerID,
		EnvTelegramEnvironment,
	}
	for _, key := range required {
		key := key
		t.Run(key, func(t *testing.T) {
			t.Parallel()
			values := validEnvironment()
			delete(values, key)
			if _, err := Load(mapLookup(values)); err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load() error = %v, want missing %s", err, key)
			}
		})
	}
}

func TestLoadRejectsAmbiguousOrUnsafeValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		key   string
		value string
	}{
		{name: "hostname listener", key: EnvListenAddress, value: "localhost:18080"},
		{name: "wildcard listener", key: EnvListenAddress, value: "0.0.0.0:18080"},
		{name: "zero listener port", key: EnvListenAddress, value: "127.0.0.1:0"},
		{name: "noncanonical listener", key: EnvListenAddress, value: "127.0.0.1:018080"},
		{name: "HTTP public origin", key: EnvPublicOrigin, value: "http://mobile.example.test"},
		{name: "public origin path", key: EnvPublicOrigin, value: "https://mobile.example.test/app"},
		{name: "public origin credentials", key: EnvPublicOrigin, value: "https://owner@mobile.example.test"},
		{name: "public origin invalid port", key: EnvPublicOrigin, value: "https://mobile.example.test:99999"},
		{name: "public origin noncanonical port", key: EnvPublicOrigin, value: "https://mobile.example.test:0443"},
		{name: "relative business socket", key: EnvControllerBusinessSocket, value: "run/business.sock"},
		{name: "relative health socket", key: EnvControllerHealthSocket, value: "run/health.sock"},
		{name: "unclean control socket", key: EnvControllerControlSocket, value: "/run/fixik-next/../control.sock"},
		{name: "unclean recovery socket", key: EnvControllerRecoverySocket, value: "/run/fixik-next/../recovery.sock"},
		{name: "zero bot", key: EnvTelegramBotID, value: "0"},
		{name: "noncanonical owner", key: EnvTelegramOwnerID, value: "+200002"},
		{name: "too large owner", key: EnvTelegramOwnerID, value: "4503599627370496"},
		{name: "unreviewed environment", key: EnvTelegramEnvironment, value: "staging"},
		{name: "surrounding whitespace", key: EnvTelegramEnvironment, value: " test"},
		{name: "empty optional timeout", key: EnvShutdownTimeoutMS, value: ""},
		{name: "oversized timeout", key: EnvShutdownTimeoutMS, value: "60001"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			values := validEnvironment()
			values[test.key] = test.value
			if _, err := Load(mapLookup(values)); err == nil {
				t.Fatalf("Load() accepted %s=%q", test.key, test.value)
			}
		})
	}
}

func TestLoadRequiresPhysicallyDistinctControllerSockets(t *testing.T) {
	t.Parallel()

	keys := []string{
		EnvControllerBusinessSocket, EnvControllerHealthSocket,
		EnvControllerControlSocket, EnvControllerRecoverySocket,
	}
	for left := range keys {
		for right := left + 1; right < len(keys); right++ {
			values := validEnvironment()
			values[keys[right]] = values[keys[left]]
			if _, err := Load(mapLookup(values)); err == nil {
				t.Fatalf("Load() accepted aliased classes %s/%s", keys[left], keys[right])
			}
		}
	}
}

func TestLoadAcceptsBoundedTimeoutAndProductionKeySelection(t *testing.T) {
	t.Parallel()

	values := validEnvironment()
	values[EnvShutdownTimeoutMS] = "2500"
	values[EnvTelegramEnvironment] = "production"
	configuration, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if configuration.ShutdownTimeout != 2500*time.Millisecond || configuration.TelegramEnvironment != mobileauth.EnvironmentProduction {
		t.Fatalf("unexpected configuration: %#v", configuration)
	}
}

func TestLoadAcceptsExactHTTPSOriginPort(t *testing.T) {
	t.Parallel()

	values := validEnvironment()
	values[EnvPublicOrigin] = "https://mobile.example.test:8443"
	configuration, err := Load(mapLookup(values))
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if configuration.PublicOrigin != values[EnvPublicOrigin] {
		t.Fatalf("PublicOrigin = %q", configuration.PublicOrigin)
	}
}

func TestLoadNeverRequestsSecretsOrStateCredentials(t *testing.T) {
	t.Parallel()

	values := validEnvironment()
	lookup := func(key string) (string, bool) {
		upper := strings.ToUpper(key)
		for _, forbidden := range []string{"TOKEN", "DATABASE", "WORKER", "CREDENTIAL", "PRIVATE_KEY", "PUBLIC_KEY", "FORWARDED"} {
			if strings.Contains(upper, forbidden) {
				t.Fatalf("configuration attempted to read forbidden key %q", key)
			}
		}
		value, ok := values[key]
		return value, ok
	}
	if _, err := Load(lookup); err != nil {
		t.Fatalf("Load(): %v", err)
	}

	configurationType := reflect.TypeOf(Config{})
	for index := 0; index < configurationType.NumField(); index++ {
		name := strings.ToUpper(configurationType.Field(index).Name)
		for _, forbidden := range []string{"TOKEN", "DATABASE", "WORKER", "CREDENTIAL", "PRIVATEKEY", "PUBLICKEY", "PROXY"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("Config contains forbidden field %q", configurationType.Field(index).Name)
			}
		}
	}
}

func validEnvironment() map[string]string {
	return map[string]string{
		EnvListenAddress:            "127.0.0.1:18080",
		EnvPublicOrigin:             "https://mobile.example.test",
		EnvControllerBusinessSocket: "/run/fixik-next/controller-business.sock",
		EnvControllerHealthSocket:   "/run/fixik-next/controller-health.sock",
		EnvControllerControlSocket:  "/run/fixik-next/controller-control.sock",
		EnvControllerRecoverySocket: "/run/fixik-next/controller-recovery.sock",
		EnvTelegramBotID:            "100001",
		EnvTelegramOwnerID:          "200002",
		EnvTelegramEnvironment:      "test",
	}
}

func mapLookup(values map[string]string) LookupEnv {
	return func(key string) (string, bool) {
		value, ok := values[key]
		return value, ok
	}
}
