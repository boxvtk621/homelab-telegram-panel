package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historyreplica"
	"github.com/boxvtk621/homelab-telegram-panel/internal/historysearch"
)

type historyBackend interface {
	ReadHistoryReplica(context.Context, string, string, int, string) (historyreplica.ReadPage, error)
	SearchHistory(context.Context, string, historysearch.Query, int, string) (historysearch.Page, error)
	HistoryEntryByID(context.Context, string, string, string) (agentserviceclient.HistoryEntryLookup, error)
	HistoryReceiptByOrigin(context.Context, string, string, string) (historyreplica.ReceiptLookup, error)
	HistoryTextManifest(context.Context, string, string, string) (historyreplica.TextManifest, error)
	HistoryTextChunk(context.Context, string, string, string, int64) (historyreplica.TextChunk, error)
}

func (s *Server) historyHTTP(w http.ResponseWriter, r *http.Request, current session) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v2/history/") {
		return false
	}
	if r.Method != http.MethodGet {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return true
	}
	if s.history == nil {
		fail(w, http.StatusNotFound, "history_not_configured")
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v2/history/"), "/")
	if !s.harnessPermit(w, s.general) {
		return true
	}
	defer func() { <-s.general }()
	var result any
	var err error
	switch {
	case len(parts) == 1 && parts[0] == "search":
		result, err = s.historySearch(r, current.ownerID)
	case len(parts) == 4 && parts[0] == "dialogs" && parts[2] == "entries" && r.URL.RawQuery == "":
		if !inventoryNodeID.MatchString(parts[1]) || !inventoryNodeID.MatchString(parts[3]) {
			err = errors.New("invalid history entry")
		} else {
			result, err = s.history.HistoryEntryByID(r.Context(), current.ownerID, parts[1], parts[3])
		}
	case len(parts) == 2 && parts[0] == "dialogs":
		result, err = s.historyDialog(r, current.ownerID, parts[1])
	case len(parts) == 3 && parts[0] == "receipts" && inventoryNodeID.MatchString(parts[1]) && inventoryNodeID.MatchString(parts[2]) && r.URL.RawQuery == "":
		result, err = s.history.HistoryReceiptByOrigin(r.Context(), current.ownerID, parts[1], parts[2])
	case len(parts) == 3 && parts[0] == "texts" && inventoryNodeID.MatchString(parts[1]) && inventoryNodeID.MatchString(parts[2]) && r.URL.RawQuery == "":
		result, err = s.history.HistoryTextManifest(r.Context(), current.ownerID, parts[1], parts[2])
	case len(parts) == 5 && parts[0] == "texts" && parts[3] == "chunks" &&
		inventoryNodeID.MatchString(parts[1]) && inventoryNodeID.MatchString(parts[2]) && r.URL.RawQuery == "":
		var index int64
		index, err = strconv.ParseInt(parts[4], 10, 64)
		if err == nil && (index < 0 || strconv.FormatInt(index, 10) != parts[4]) {
			err = errors.New("invalid chunk index")
		}
		if err == nil {
			result, err = s.history.HistoryTextChunk(r.Context(), current.ownerID, parts[1], parts[2], index)
		}
	default:
		fail(w, http.StatusNotFound, "not_found")
		return true
	}
	if err != nil {
		status, code := http.StatusBadRequest, "invalid_request"
		var fault *agentserviceclient.Fault
		if errors.As(err, &fault) {
			status, code = fault.Status, fault.Code
		}
		fail(w, status, code)
		return true
	}
	reply(w, http.StatusOK, result)
	return true
}

func (s *Server) historySearch(r *http.Request, ownerID string) (historysearch.Page, error) {
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return historysearch.Page{}, err
	}
	for key, values := range query {
		if (key != "q" && key != "limit" && key != "cursor" && key != "nodeId" && key != "logicalDialogId" && key != "archived") ||
			len(values) != 1 || values[0] == "" {
			return historysearch.Page{}, errors.New("invalid history search query")
		}
	}
	request := historysearch.Query{Q: query.Get("q"), NodeID: query.Get("nodeId"), LogicalDialogID: query.Get("logicalDialogId")}
	if request.Q == "" || len([]byte(request.Q)) > historysearch.MaximumQueryBytes ||
		(request.NodeID != "" && !inventoryNodeID.MatchString(request.NodeID)) ||
		(request.LogicalDialogID != "" && !inventoryNodeID.MatchString(request.LogicalDialogID)) {
		return historysearch.Page{}, errors.New("invalid history search query")
	}
	if raw := query.Get("archived"); raw != "" {
		if raw != "true" && raw != "false" {
			return historysearch.Page{}, errors.New("invalid history archive filter")
		}
		value := raw == "true"
		request.Archived = &value
	}
	limit := 20
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > historysearch.MaximumPageSize || strconv.Itoa(limit) != raw {
			return historysearch.Page{}, errors.New("invalid history search limit")
		}
	}
	cursor := query.Get("cursor")
	if len(cursor) > 8192 {
		return historysearch.Page{}, errors.New("invalid history search cursor")
	}
	return s.history.SearchHistory(r.Context(), ownerID, request, limit, cursor)
}

func (s *Server) historyDialog(r *http.Request, ownerID, logicalDialogID string) (historyreplica.ReadPage, error) {
	if !inventoryNodeID.MatchString(logicalDialogID) {
		return historyreplica.ReadPage{}, errors.New("invalid logical dialog")
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return historyreplica.ReadPage{}, err
	}
	for key, values := range query {
		if (key != "limit" && key != "cursor") || len(values) != 1 || values[0] == "" {
			return historyreplica.ReadPage{}, errors.New("invalid history query")
		}
	}
	limit := 100
	if value := query.Get("limit"); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > historyreplica.MaximumPageSize || strconv.Itoa(limit) != value {
			return historyreplica.ReadPage{}, errors.New("invalid history limit")
		}
	}
	cursor := query.Get("cursor")
	if len(cursor) > 8192 {
		return historyreplica.ReadPage{}, errors.New("invalid history cursor")
	}
	return s.history.ReadHistoryReplica(r.Context(), ownerID, logicalDialogID, limit, cursor)
}
