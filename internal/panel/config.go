// Package panel owns the independent web process and ephemeral web sessions.
package panel

import (
	"errors"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/youtrack"
)

type Config struct {
	Listen, Origin, YouTrackURL, ProjectID, ProjectKey, OwnerLogin string
	Writes                                                         bool
	CursorPython, CursorWorker, CursorModel, CursorKey             string
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
	if v, ok := lookup("PANEL_WRITES_ENABLED"); ok {
		if v != "true" && v != "false" {
			return Config{}, errors.New("invalid write flag")
		}
		c.Writes = v == "true"
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
