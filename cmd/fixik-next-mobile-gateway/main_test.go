package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayDependenciesUseEmbeddedMobileWorkspace(t *testing.T) {
	t.Parallel()

	dependencies, err := gatewayDependencies(1000)
	if err != nil {
		t.Fatalf("gatewayDependencies(): %v", err)
	}
	if dependencies.StaticHandler == nil || dependencies.EffectiveUID != 1000 {
		t.Fatalf("static handler/effective UID = %v/%d", dependencies.StaticHandler, dependencies.EffectiveUID)
	}

	index := httptest.NewRecorder()
	dependencies.StaticHandler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/", nil))
	if index.Code != http.StatusOK || index.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("embedded index status/cache = %d/%q; body: %s", index.Code, index.Header().Get("Cache-Control"), index.Body.String())
	}
	if strings.Contains(index.Body.String(), `telegram-web-app`) || strings.Contains(index.Body.String(), `src="http`) {
		t.Fatal("independent web index must not load Telegram or remote application scripts")
	}

	unknownAPI := httptest.NewRecorder()
	dependencies.StaticHandler.ServeHTTP(unknownAPI, httptest.NewRequest(http.MethodGet, "https://mobile.example.test/api/v1/unknown", nil))
	if unknownAPI.Code != http.StatusNotFound {
		t.Fatalf("static handler claimed unknown API path with status %d", unknownAPI.Code)
	}
}
