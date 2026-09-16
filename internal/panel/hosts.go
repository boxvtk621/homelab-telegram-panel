package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
)

type hostBackend interface {
	Hosts(context.Context, string, int, string) (agentserviceclient.DockerHostPage, error)
	Host(context.Context, string, string) (agentserviceclient.DockerHost, error)
	UpsertHost(context.Context, string, agentserviceclient.HostUpsert) (agentserviceclient.DockerHost, int, error)
	ProvisionHostSecret(context.Context, string, agentserviceclient.HostSecretInput) (agentserviceclient.HostSecretProvision, error)
	HostSecretProvision(context.Context, string, string) (agentserviceclient.HostSecretProvision, error)
	ProbeHost(context.Context, string, string, agentserviceclient.HostProbeRequest) (agentserviceclient.DockerHost, error)
}

func (s *Server) hostHTTP(w http.ResponseWriter, r *http.Request, current session) bool {
	if r.URL.Path != "/api/v2/hosts" && r.URL.Path != "/api/v2/host-secrets" &&
		!strings.HasPrefix(r.URL.Path, "/api/v2/host-secrets/") && !strings.HasPrefix(r.URL.Path, "/api/v2/hosts/") {
		return false
	}
	if s.hosts == nil {
		fail(w, http.StatusNotFound, "inventory_not_configured")
		return true
	}
	if !s.harnessPermit(w, s.general) {
		return true
	}
	defer func() { <-s.general }()
	switch {
	case r.URL.Path == "/api/v2/hosts" && r.Method == http.MethodGet:
		s.listHostsHTTP(w, r, current)
	case r.URL.Path == "/api/v2/hosts" && r.Method == http.MethodPost:
		var input agentserviceclient.HostUpsert
		if !decode(w, r, &input) {
			return true
		}
		host, status, err := s.hosts.UpsertHost(r.Context(), current.ownerID, input)
		if err != nil {
			hostFault(w, err)
			return true
		}
		reply(w, status, host)
	case r.URL.Path == "/api/v2/host-secrets" && r.Method == http.MethodPost:
		var input agentserviceclient.HostSecretInput
		if !decode(w, r, &input) {
			return true
		}
		defer zeroPanelSecret(input)
		provision, err := s.hosts.ProvisionHostSecret(r.Context(), current.ownerID, input)
		if err != nil {
			hostFault(w, err)
			return true
		}
		status := http.StatusOK
		if provision.Created {
			status = http.StatusCreated
		}
		reply(w, status, provision)
	case strings.HasPrefix(r.URL.Path, "/api/v2/host-secrets/") && r.Method == http.MethodGet:
		operationID := strings.TrimPrefix(r.URL.Path, "/api/v2/host-secrets/")
		if !inventoryNodeID.MatchString(operationID) || strings.Contains(operationID, "/") || r.URL.RawQuery != "" ||
			r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			fail(w, http.StatusBadRequest, "invalid_request")
			return true
		}
		provision, err := s.hosts.HostSecretProvision(r.Context(), current.ownerID, operationID)
		if err != nil {
			hostFault(w, err)
			return true
		}
		reply(w, http.StatusOK, provision)
	case strings.HasPrefix(r.URL.Path, "/api/v2/hosts/"):
		s.singleHostHTTP(w, r, current)
	default:
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
	}
	return true
}

func (s *Server) listHostsHTTP(w http.ResponseWriter, r *http.Request, current session) {
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
		limit, err = strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 100 || strconv.Itoa(limit) != values[0] {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
	}
	cursor := query.Get("cursor")
	if values, present := query["cursor"]; present && (values[0] == "" || len(cursor) > 512) {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	page, err := s.hosts.Hosts(r.Context(), current.ownerID, limit, cursor)
	if err != nil {
		hostFault(w, err)
		return
	}
	reply(w, http.StatusOK, page)
}

func (s *Server) singleHostHTTP(w http.ResponseWriter, r *http.Request, current session) {
	suffix := strings.TrimPrefix(r.URL.Path, "/api/v2/hosts/")
	if strings.HasSuffix(suffix, "/probe") {
		hostID := strings.TrimSuffix(suffix, "/probe")
		if r.Method != http.MethodPost || !inventoryNodeID.MatchString(hostID) || r.URL.RawQuery != "" {
			fail(w, http.StatusBadRequest, "invalid_request")
			return
		}
		var input agentserviceclient.HostProbeRequest
		if !decode(w, r, &input) {
			return
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(60 * time.Second))
		host, err := s.hosts.ProbeHost(r.Context(), current.ownerID, hostID, input)
		if err != nil {
			hostFault(w, err)
			return
		}
		reply(w, http.StatusOK, host)
		return
	}
	if r.Method != http.MethodGet || !inventoryNodeID.MatchString(suffix) || r.URL.RawQuery != "" {
		fail(w, http.StatusBadRequest, "invalid_request")
		return
	}
	host, err := s.hosts.Host(r.Context(), current.ownerID, suffix)
	if err != nil {
		hostFault(w, err)
		return
	}
	reply(w, http.StatusOK, host)
}

func hostFault(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "hosts_unavailable"
	var fault *agentserviceclient.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	fail(w, status, code)
}

func zeroPanelSecret(input agentserviceclient.HostSecretInput) {
	for index := range input.PrivateKey {
		input.PrivateKey[index] = 0
	}
	for index := range input.Passphrase {
		input.Passphrase[index] = 0
	}
	for index := range input.Payload {
		input.Payload[index] = 0
	}
}
