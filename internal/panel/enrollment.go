package panel

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	"github.com/boxvtk621/homelab-telegram-panel/internal/agentserviceclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnesstunnel"
)

const (
	externalEnrollmentHostsSchema  = "external-harness-enrollment-host-page-v1"
	externalEnrollmentResultSchema = "external-harness-enrollment-result-v1"
)

type enrollmentBackend interface {
	Hosts(context.Context, string, int, string) (agentserviceclient.DockerHostPage, error)
	PrepareExternalEnrollment(context.Context, string, agentserviceclient.ExternalEnrollmentRequest) (agentserviceclient.ExternalEnrollmentPlan, error)
	RegistryOperationSent(context.Context, string, agentserviceclient.RegistryOperationStatus) (agentserviceclient.RegistryOperationStatus, error)
	RegistryOperationUnknown(context.Context, string, agentserviceclient.RegistryOperationStatus) (agentserviceclient.RegistryOperationStatus, error)
	FinishRegistryOperation(context.Context, string, agentserviceclient.RegistryOperationStatus, string) (agentserviceclient.RegistryOperationStatus, error)
	FailRegistryOperation(context.Context, string, agentserviceclient.RegistryOperationStatus, string) (agentserviceclient.RegistryOperationStatus, error)
}

type enrollmentRouter interface {
	InstallEnrollmentRegistry(harnessrouter.EnrollmentRegistryInput) (harnessrouter.State, error)
	ActivateEnrollment(context.Context, string, string) (harnessrouter.NodeState, error)
}

type enrollmentHost struct {
	HostID       string `json:"hostId"`
	HostVersion  int64  `json:"hostVersion"`
	DisplayName  string `json:"displayName"`
	Transport    string `json:"transport"`
	Availability string `json:"availability"`
}

type enrollmentHostPage struct {
	SchemaID   string           `json:"schemaId"`
	Items      []enrollmentHost `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

type enrollmentResult struct {
	SchemaID             string `json:"schemaId"`
	OperationID          string `json:"operationId"`
	NodeID               string `json:"nodeId"`
	Status               string `json:"status"`
	RegistrationRevision int64  `json:"registrationRevision"`
	IdentityEpoch        int64  `json:"identityEpoch"`
}

func (s *Server) enrollmentHTTP(w http.ResponseWriter, r *http.Request, current session) bool {
	if r.URL.Path != "/api/v2/external-enrollment-hosts" && r.URL.Path != "/api/v2/external-enrollments" {
		return false
	}
	if s.enrollment == nil || s.enrollmentControl == nil {
		fail(w, http.StatusNotFound, "enrollment_not_configured")
		return true
	}
	if !s.harnessPermit(w, s.general) {
		return true
	}
	defer func() { <-s.general }()
	if r.URL.Path == "/api/v2/external-enrollment-hosts" {
		if r.Method != http.MethodGet {
			fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return true
		}
		s.enrollmentHostsHTTP(w, r, current)
		return true
	}
	if r.Method != http.MethodPost {
		fail(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return true
	}
	var input agentserviceclient.ExternalEnrollmentRequest
	if !decode(w, r, &input) {
		return true
	}
	plan, err := s.enrollment.PrepareExternalEnrollment(r.Context(), current.ownerID, input)
	if err != nil {
		enrollmentClientFault(w, err)
		return true
	}
	if plan.Status.Phase == "failed" {
		fail(w, http.StatusConflict, "enrollment_conflict")
		return true
	}
	bindingStaged := false
	var stagedBinding harnesstunnel.EndpointBinding
	if plan.Binding != nil {
		if plan.Binding.Transport == "ssh" && s.cfg.Harness.TunnelBindings == "" {
			fail(w, http.StatusUnprocessableEntity, "tunnel_not_configured")
			return true
		}
		if s.cfg.Harness.TunnelBindings != "" {
			binding := externalTunnelBinding(*plan.Binding)
			if err := harnesstunnel.StageExternalBinding(
				s.cfg.Harness.TunnelBindings,
				current.ownerID,
				plan.Status.Receipt.ExpectedRegistrySHA256,
				plan.Status.Receipt.CandidateRegistrySHA256,
				binding,
			); err != nil {
				fail(w, http.StatusConflict, "tunnel_binding_conflict")
				return true
			}
			bindingStaged, stagedBinding = true, binding
		}
	}

	status := plan.Status
	if status.Phase == "accepted" {
		status, err = s.enrollment.RegistryOperationSent(r.Context(), current.ownerID, status)
		if err != nil {
			enrollmentClientFault(w, err)
			return true
		}
	}
	if status.Phase != "applying" && status.Phase != "reconciling" && status.Phase != "succeeded" {
		fail(w, http.StatusServiceUnavailable, "enrollment_reconciliation_required")
		return true
	}

	_, err = s.enrollmentControl.InstallEnrollmentRegistry(harnessrouter.EnrollmentRegistryInput{
		OperationID:             plan.OperationID,
		NodeID:                  plan.NodeID,
		ExpectedRegistryVersion: status.Receipt.ExpectedRegistryVersion,
		ExpectedRegistrySHA256:  status.Receipt.ExpectedRegistrySHA256,
		Registry:                plan.Registry,
	})
	if err != nil {
		var fault *harnessrouter.EnrollmentFault
		if errors.As(err, &fault) && fault.Status < http.StatusInternalServerError {
			if status.Phase != "succeeded" {
				if bindingStaged && harnesstunnel.UnstageExternalBinding(
					s.cfg.Harness.TunnelBindings,
					current.ownerID,
					status.Receipt.ExpectedRegistrySHA256,
					status.Receipt.CandidateRegistrySHA256,
					stagedBinding,
				) != nil {
					fail(w, http.StatusServiceUnavailable, "enrollment_reconciliation_required")
					return true
				}
				if _, failErr := s.enrollment.FailRegistryOperation(r.Context(), current.ownerID, status, fault.Code); failErr != nil {
					fail(w, http.StatusServiceUnavailable, "enrollment_reconciliation_required")
					return true
				}
			}
			fail(w, fault.Status, fault.Code)
			return true
		}
		if status.Phase != "succeeded" {
			_, _ = s.enrollment.RegistryOperationUnknown(r.Context(), current.ownerID, status)
		}
		fail(w, http.StatusServiceUnavailable, "enrollment_reconciliation_required")
		return true
	}
	node, err := s.enrollmentControl.ActivateEnrollment(r.Context(), plan.OperationID, plan.NodeID)
	if err != nil {
		var fault *harnessrouter.EnrollmentFault
		if errors.As(err, &fault) {
			fail(w, fault.Status, fault.Code)
			return true
		}
		fail(w, http.StatusServiceUnavailable, "enrollment_unavailable")
		return true
	}
	if status.Phase != "succeeded" {
		effectState := "acknowledged"
		if status.Phase == "reconciling" {
			effectState = "reconciled"
		}
		status, err = s.enrollment.FinishRegistryOperation(r.Context(), current.ownerID, status, effectState)
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "enrollment_reconciliation_required")
			return true
		}
	}
	reply(w, http.StatusOK, enrollmentResult{
		SchemaID: externalEnrollmentResultSchema, OperationID: plan.OperationID, NodeID: plan.NodeID,
		Status: "ready", RegistrationRevision: node.RegistrationRevision, IdentityEpoch: node.IdentityEpoch,
	})
	return true
}

func (s *Server) enrollmentHostsHTTP(w http.ResponseWriter, r *http.Request, current session) {
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
	page, err := s.enrollment.Hosts(r.Context(), current.ownerID, limit, cursor)
	if err != nil {
		enrollmentClientFault(w, err)
		return
	}
	result := enrollmentHostPage{SchemaID: externalEnrollmentHostsSchema, Items: make([]enrollmentHost, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, host := range page.Items {
		if host.Transport != "local" && host.Transport != "ssh" {
			continue
		}
		result.Items = append(result.Items, enrollmentHost{
			HostID: host.HostID, HostVersion: host.HostVersion, DisplayName: host.DisplayName,
			Transport: host.Transport, Availability: host.Availability,
		})
	}
	reply(w, http.StatusOK, result)
}

func externalTunnelBinding(value agentserviceclient.ExternalEndpointBinding) harnesstunnel.EndpointBinding {
	return harnesstunnel.EndpointBinding{
		Kind: value.Kind, NodeID: value.NodeID, RegistrationRevision: value.RegistrationRevision,
		RegistrationEpoch: value.RegistrationEpoch, EndpointRevision: value.EndpointRevision,
		HostID: value.HostID, HostVersion: value.HostVersion, Transport: value.Transport,
		TargetRef: value.TargetRef, CredentialRef: value.CredentialRef, DockerContextRef: value.DockerContextRef,
		ExpectedHostKey: value.ExpectedHostKey, ExpectedHostIdentitySHA256: value.ExpectedHostIdentitySHA256,
		HostPlatform: value.HostPlatform, HostArchitecture: value.HostArchitecture, ContainerID: value.ContainerID,
		RuntimeGeneration: value.RuntimeGeneration, Address: value.Address,
	}
}

func enrollmentClientFault(w http.ResponseWriter, err error) {
	status, code := http.StatusServiceUnavailable, "enrollment_unavailable"
	var fault *agentserviceclient.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	fail(w, status, code)
}
