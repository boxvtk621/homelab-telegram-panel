package panel

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
)

type configurationBackend interface {
	ConfigurationDraft(context.Context, string, string) (agentserviceclient.ConfigurationDraft, error)
	ValidateConfiguration(context.Context, string, agentserviceclient.ConfigurationInput) (agentserviceclient.ConfigurationValidation, error)
	SaveConfigurationDraft(context.Context, string, string, agentserviceclient.ConfigurationSave) (agentserviceclient.ConfigurationDraft, *agentserviceclient.ConfigurationDraft, error)
}

func (s *Server) configurationHTTP(w http.ResponseWriter, r *http.Request, current session) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/v2/configuration-drafts") {
		return false
	}
	if s.configuration == nil {
		fail(w, http.StatusNotFound, "inventory_not_configured")
		return true
	}
	if !s.harnessPermit(w, s.general) {
		return true
	}
	defer func() { <-s.general }()
	if r.URL.Path == "/api/v2/configuration-drafts/validate" && r.Method == http.MethodPost {
		var input agentserviceclient.ConfigurationInput
		if !decodeLimit(w, r, &input, 3<<20) {
			return true
		}
		value, err := s.configuration.ValidateConfiguration(r.Context(), current.ownerID, input)
		if err != nil {
			configurationFault(w, err)
			return true
		}
		reply(w, http.StatusOK, value)
		return true
	}
	nodeID := strings.TrimPrefix(r.URL.Path, "/api/v2/configuration-drafts/")
	if !inventoryNodeID.MatchString(nodeID) || strings.Contains(nodeID, "/") {
		fail(w, http.StatusBadRequest, "invalid_request")
		return true
	}
	if r.Method == http.MethodGet {
		value, err := s.configuration.ConfigurationDraft(r.Context(), current.ownerID, nodeID)
		if err != nil {
			configurationFault(w, err)
			return true
		}
		reply(w, http.StatusOK, value)
		return true
	}
	if r.Method == http.MethodPost {
		var input agentserviceclient.ConfigurationSave
		if !decodeLimit(w, r, &input, 3<<20) {
			return true
		}
		value, currentDraft, err := s.configuration.SaveConfigurationDraft(r.Context(), current.ownerID, nodeID, input)
		if err != nil {
			var fault *agentserviceclient.Fault
			if errors.As(err, &fault) && fault.Status == http.StatusConflict && currentDraft != nil {
				reply(w, http.StatusConflict, map[string]any{"schemaId": "agent-configuration-conflict-v2", "error": fault.Code, "current": currentDraft})
				return true
			}
			configurationFault(w, err)
			return true
		}
		reply(w, http.StatusOK, value)
		return true
	}
	fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
	return true
}
func configurationFault(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "configuration_unavailable"
	var fault *agentserviceclient.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	fail(w, status, code)
}
