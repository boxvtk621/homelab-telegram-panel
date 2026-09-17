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
)

type historyBackend interface {
	ReadHistoryReplica(context.Context, string, string, int, string) (historyreplica.ReadPage, error)
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
