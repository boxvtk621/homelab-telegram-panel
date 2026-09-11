// Package panel owns the independent web process and ephemeral web sessions.
package panel

import (
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
)

type Config struct {
	Listen, Origin, OwnerID                 string
	HarnessCommands                         bool
	BasePath                                string
	Harness                                 harnessclient.Paths
	HarnessRouterState, HarnessRouterSocket string
	TLSCertificate, TLSKey                  string
}

func Load(lookup func(string) (string, bool)) (Config, error) {
	c := Config{}
	for key, target := range map[string]*string{"PANEL_LISTEN": &c.Listen, "PANEL_PUBLIC_ORIGIN": &c.Origin} {
		v, ok := lookup(key)
		if !ok || v == "" || strings.TrimSpace(v) != v {
			return Config{}, errors.New("required Panel configuration is missing or invalid")
		}
		*target = v
	}
	host, port, err := net.SplitHostPort(c.Listen)
	n, e := strconv.Atoi(port)
	if err != nil || e != nil || n < 1024 || n > 65535 || net.ParseIP(host) == nil {
		return Config{}, errors.New("Panel listen address must be an explicit IP and unprivileged port")
	}
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" {
		return Config{}, errors.New("Panel requires an exact HTTPS public origin")
	}
	c.BasePath, _ = lookup("PANEL_BASE_PATH")
	c.TLSCertificate, _ = lookup("PANEL_TLS_CERTIFICATE")
	c.TLSKey, _ = lookup("PANEL_TLS_KEY")
	if (c.TLSCertificate == "") != (c.TLSKey == "") ||
		(c.TLSCertificate != "" && (!filepath.IsAbs(c.TLSCertificate) || !filepath.IsAbs(c.TLSKey))) {
		return Config{}, errors.New("Panel TLS requires explicit certificate and key paths")
	}
	if len(c.BasePath) > 128 || (c.BasePath != "" && !regexp.MustCompile(`^(/[A-Za-z0-9_-]+)+$`).MatchString(c.BasePath)) {
		return Config{}, errors.New("invalid Panel base path")
	}
	if value, ok := lookup("PANEL_OWNER_ID"); ok {
		if !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`).MatchString(value) {
			return Config{}, errors.New("invalid Panel owner identity")
		}
		c.OwnerID = value
	}
	commandFlag, flagConfigured := lookup("PANEL_HARNESS_COMMANDS_ENABLED")
	if !flagConfigured {
		// Transitional compatibility for the deployed alpha configuration. The
		// flag now gates Harness commands only; YouTrack writes are not exposed.
		commandFlag, flagConfigured = lookup("PANEL_WRITES_ENABLED")
	}
	if flagConfigured {
		if commandFlag != "true" && commandFlag != "false" {
			return Config{}, errors.New("invalid Harness command flag")
		}
		c.HarnessCommands = commandFlag == "true"
	}
	harnessPaths := map[string]*string{"PANEL_HARNESS_REGISTRY": &c.Harness.Registry, "PANEL_HARNESS_SIGNER_PUBLIC_KEY": &c.Harness.SignerPublicKey, "PANEL_HARNESS_CA": &c.Harness.CA, "PANEL_HARNESS_CLIENT_CERT": &c.Harness.ClientCertificate, "PANEL_HARNESS_CLIENT_KEY": &c.Harness.ClientKey}
	configured := 0
	for key, target := range harnessPaths {
		if value, ok := lookup(key); ok {
			if !filepath.IsAbs(value) || strings.TrimSpace(value) != value {
				return Config{}, errors.New("invalid Harness configuration path")
			}
			*target = value
			configured++
		}
	}
	if configured != 0 && configured != len(harnessPaths) {
		return Config{}, errors.New("incomplete Harness trust configuration")
	}
	c.HarnessRouterState, _ = lookup("PANEL_HARNESS_ROUTER_STATE")
	c.HarnessRouterSocket, _ = lookup("PANEL_HARNESS_ROUTER_SOCKET")
	routerConfigured := c.HarnessRouterState != "" || c.HarnessRouterSocket != ""
	if (configured == 0 && routerConfigured) || (configured != 0 && (!filepath.IsAbs(c.HarnessRouterState) || !filepath.IsAbs(c.HarnessRouterSocket) ||
		strings.TrimSpace(c.HarnessRouterState) != c.HarnessRouterState || strings.TrimSpace(c.HarnessRouterSocket) != c.HarnessRouterSocket ||
		filepath.Dir(c.HarnessRouterState) != filepath.Dir(c.HarnessRouterSocket) || c.HarnessRouterState == c.HarnessRouterSocket)) {
		return Config{}, errors.New("invalid Harness Router state configuration")
	}
	// Old deployment variables are never a fallback to a Controller transport.
	for _, key := range []string{"FIXIK_NEXT_MOBILE_CONTROLLER_BUSINESS_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_HEALTH_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_CONTROL_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_RECOVERY_SOCKET", "FIXIK_NEXT_MOBILE_TELEGRAM_BOT_ID"} {
		if _, ok := lookup(key); ok {
			return Config{}, errors.New("legacy bot configuration is not supported")
		}
	}
	return c, nil
}
