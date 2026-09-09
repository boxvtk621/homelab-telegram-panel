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
	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
)

type Config struct {
	Listen, Origin, YouTrackURL, ProjectID, ProjectKey, OwnerLogin string
	Writes                                                         bool
	CursorPython, CursorWorker, CursorModel, CursorKey             string
	BasePath                                                       string
	Harness                                                        harnessclient.Paths
	TLSCertificate, TLSKey                                         string
}

func Load(lookup func(string) (string, bool)) (Config, error) {
	c := Config{}
	for key, target := range map[string]*string{"PANEL_LISTEN": &c.Listen, "PANEL_PUBLIC_ORIGIN": &c.Origin, "PANEL_YOUTRACK_URL": &c.YouTrackURL, "PANEL_PROJECT_ID": &c.ProjectID, "PANEL_PROJECT_KEY": &c.ProjectKey, "PANEL_OWNER_LOGIN": &c.OwnerLogin} {
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
	if v, ok := lookup("PANEL_WRITES_ENABLED"); ok {
		if v != "true" && v != "false" {
			return Config{}, errors.New("invalid write flag")
		}
		c.Writes = v == "true"
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
	client, err := youtrack.New(c.YouTrackURL, c.ProjectID, c.ProjectKey)
	if err != nil {
		return Config{}, errors.New("invalid YouTrack boundary")
	}
	client.Close()
	// Independently provisioned Panel credential; no Fixik config fallback.
	c.CursorKey, _ = lookup("PANEL_CURSOR_API_KEY")
	if c.CursorKey != "" {
		c.CursorPython, _ = lookup("PANEL_CURSOR_PYTHON")
		c.CursorWorker, _ = lookup("PANEL_CURSOR_WORKER")
		c.CursorModel, _ = lookup("PANEL_CURSOR_MODEL")
		if !filepath.IsAbs(c.CursorPython) || !filepath.IsAbs(c.CursorWorker) || strings.TrimSpace(c.CursorModel) == "" || len(c.CursorModel) > 128 || len(c.CursorKey) > 4096 || strings.ContainsAny(c.CursorKey, "\r\n\x00") {
			return Config{}, errors.New("invalid Cursor SDK configuration")
		}
	}
	// Old deployment variables are never a fallback to a Controller transport.
	for _, key := range []string{"FIXIK_NEXT_MOBILE_CONTROLLER_BUSINESS_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_HEALTH_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_CONTROL_SOCKET", "FIXIK_NEXT_MOBILE_CONTROLLER_RECOVERY_SOCKET", "FIXIK_NEXT_MOBILE_TELEGRAM_BOT_ID"} {
		if _, ok := lookup(key); ok {
			return Config{}, errors.New("legacy bot configuration is not supported")
		}
	}
	return c, nil
}
