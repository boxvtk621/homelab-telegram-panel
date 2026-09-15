package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
)

var inventoryNodeID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type inventoryBackend interface {
	Inventory(context.Context, string, int, string) (agentserviceclient.Page, error)
	DialogBindings(context.Context, string, string, int, string) (agentserviceclient.DialogPage, error)
	Close()
}

func (s *Server) inventoryHTTP(w http.ResponseWriter, r *http.Request, current session) {
	if s.inventory == nil {
		fail(w, http.StatusNotFound, "inventory_not_configured")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	limit := 50
	if values, present := query["limit"]; present {
		raw := values[0]
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	cursor := query.Get("cursor")
	if values, present := query["cursor"]; present && (values[0] == "" || len(cursor) > 512) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !s.harnessPermit(w, s.general) {
		return
	}
	defer func() { <-s.general }()
	page, err := s.inventory.Inventory(r.Context(), current.ownerID, limit, cursor)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "inventory_unavailable"
		var fault *agentserviceclient.Fault
		if errors.As(err, &fault) {
			status, code = fault.Status, fault.Code
		}
		fail(w, status, code)
		return
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) dialogBindingsHTTP(w http.ResponseWriter, r *http.Request, current session, nodeID string) {
	if s.inventory == nil {
		fail(w, http.StatusNotFound, "inventory_not_configured")
		return
	}
	if r.URL.RawPath != "" || !inventoryNodeID.MatchString(nodeID) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	limit := 50
	if values, present := query["limit"]; present {
		raw := values[0]
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != raw {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	cursor := query.Get("cursor")
	if values, present := query["cursor"]; present && (values[0] == "" || len(cursor) > 512) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if !s.harnessPermit(w, s.general) {
		return
	}
	defer func() { <-s.general }()
	page, err := s.inventory.DialogBindings(r.Context(), current.ownerID, nodeID, limit, cursor)
	if err != nil {
		status, code := http.StatusServiceUnavailable, "inventory_unavailable"
		var fault *agentserviceclient.Fault
		if errors.As(err, &fault) {
			status, code = fault.Status, fault.Code
		}
		fail(w, status, code)
		return
	}
	reply(w, http.StatusOK, page)
}
