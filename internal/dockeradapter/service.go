package dockeradapter

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

const (
	AdapterOwnerHeader    = "X-Docker-Adapter-Owner"
	AdapterTokenHeader    = "X-Docker-Adapter-Token"
	SecretInputSchema     = "docker-secret-input-v1"
	SecretProvisionSchema = "docker-secret-provision-v1"
)

type secretProvisioner interface {
	ProvisionSSH(context.Context, string, string, []byte, []byte) (SecretProvision, error)
	ProvisionRegistry(context.Context, string, string, []byte) (SecretProvision, error)
	ProvisionStatus(context.Context, string, string) (SecretProvision, error)
}

type hostProber interface {
	Probe(context.Context, string, HostDescriptor) HostObservation
}

// HostService is the adapter-only private API. It is deliberately small: the
// caller can provision an opaque credential reference or request a read-only
// host probe, but it can never retrieve secret bytes or issue Docker effects.
type HostService struct {
	token   string
	secrets secretProvisioner
	prober  hostProber
}

type secretInput struct {
	SchemaID    string `json:"schemaId"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	PrivateKey  []byte `json:"privateKey"`
	Passphrase  []byte `json:"passphrase"`
	Payload     []byte `json:"payload"`
}

func NewHostService(token string, secrets secretProvisioner, prober hostProber) (*HostService, error) {
	if len(token) < 32 || len(token) > 128 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\x00\r\n") || secrets == nil || prober == nil {
		return nil, errors.New("invalid host service configuration")
	}
	return &HostService{token: token, secrets: secrets, prober: prober}, nil
}

func (s *HostService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.RawPath != "" || r.URL.RawQuery != "" {
		adapterFailure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	if len(r.Header.Values(AdapterTokenHeader)) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get(AdapterTokenHeader)), []byte(s.token)) != 1 {
		adapterFailure(w, http.StatusForbidden, "adapter_scope_required")
		return
	}
	owner := r.Header.Get(AdapterOwnerHeader)
	if len(r.Header.Values(AdapterOwnerHeader)) != 1 || !refPattern.MatchString(owner) {
		adapterFailure(w, http.StatusForbidden, "owner_scope_required")
		return
	}
	switch {
	case r.URL.Path == "/internal/v1/secrets" && r.Method == http.MethodPost:
		s.provisionSecret(w, r, owner)
	case strings.HasPrefix(r.URL.Path, "/internal/v1/secrets/") && r.Method == http.MethodGet:
		s.secretStatus(w, r, owner, strings.TrimPrefix(r.URL.Path, "/internal/v1/secrets/"))
	case r.URL.Path == "/internal/v1/probes" && r.Method == http.MethodPost:
		s.probeHost(w, r, owner)
	case r.URL.Path == "/internal/v1/secrets" || r.URL.Path == "/internal/v1/probes" || strings.HasPrefix(r.URL.Path, "/internal/v1/secrets/"):
		adapterFailure(w, http.StatusMethodNotAllowed, "method_not_allowed")
	default:
		adapterFailure(w, http.StatusNotFound, "not_found")
	}
}

func (s *HostService) provisionSecret(w http.ResponseWriter, r *http.Request, owner string) {
	var input secretInput
	if !decodeAdapterBody(r, &input, 96<<10, "schemaId", "operationId", "kind", "privateKey", "passphrase", "payload") ||
		input.SchemaID != SecretInputSchema || !validSecretInput(input) {
		zeroBytes(input.PrivateKey)
		zeroBytes(input.Passphrase)
		zeroBytes(input.Payload)
		adapterFailure(w, http.StatusBadRequest, "invalid_secret_input")
		return
	}
	defer zeroBytes(input.PrivateKey)
	defer zeroBytes(input.Passphrase)
	defer zeroBytes(input.Payload)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var provision SecretProvision
	var err error
	if input.Kind == "ssh" {
		provision, err = s.secrets.ProvisionSSH(ctx, owner, input.OperationID, input.PrivateKey, input.Passphrase)
	} else {
		provision, err = s.secrets.ProvisionRegistry(ctx, owner, input.OperationID, input.Payload)
	}
	if errors.Is(err, ErrSecretConflict) {
		adapterFailure(w, http.StatusConflict, "secret_operation_conflict")
		return
	}
	if err != nil || !validSecretProvision(provision, input.OperationID) || provision.Kind != input.Kind {
		adapterFailure(w, http.StatusServiceUnavailable, "secret_store_unavailable")
		return
	}
	status := http.StatusOK
	if provision.Created {
		status = http.StatusCreated
	}
	adapterReply(w, status, provision)
}

func (s *HostService) secretStatus(w http.ResponseWriter, r *http.Request, owner, operationID string) {
	if !hostUUIDPattern.MatchString(operationID) || strings.Contains(operationID, "/") || r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
		adapterFailure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	provision, err := s.secrets.ProvisionStatus(ctx, owner, operationID)
	if errors.Is(err, ErrSecretNotFound) {
		adapterFailure(w, http.StatusNotFound, "secret_provision_not_found")
		return
	}
	if err != nil || !validSecretProvision(provision, operationID) {
		adapterFailure(w, http.StatusServiceUnavailable, "secret_store_unavailable")
		return
	}
	adapterReply(w, http.StatusOK, provision)
}

func (s *HostService) probeHost(w http.ResponseWriter, r *http.Request, owner string) {
	var descriptor HostDescriptor
	if !decodeAdapterBody(r, &descriptor, 16<<10,
		"schemaId", "hostId", "hostVersion", "displayName", "transport", "targetRef", "credentialRef",
		"registryCredentialRef", "expectedHostKey", "dockerContextRef", "expectedIdentitySHA256", "hostPlatform", "hostArchitecture") ||
		ValidateHostDescriptor(descriptor) != nil {
		adapterFailure(w, http.StatusBadRequest, "invalid_host_descriptor")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(50 * time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	adapterReply(w, http.StatusOK, s.prober.Probe(ctx, owner, descriptor))
}

func validSecretInput(input secretInput) bool {
	if !hostUUIDPattern.MatchString(input.OperationID) {
		return false
	}
	switch input.Kind {
	case "ssh":
		return len(input.PrivateKey) > 0 && len(input.PrivateKey) <= maximumSecretBytes && len(input.Passphrase) <= 4<<10 && len(input.Payload) == 0
	case "registry":
		return len(input.Payload) > 0 && len(input.Payload) <= maximumSecretBytes && len(input.PrivateKey) == 0 && len(input.Passphrase) == 0
	default:
		return false
	}
}

func validSecretProvision(value SecretProvision, operationID string) bool {
	return value.SchemaID == SecretProvisionSchema && value.OperationID == operationID &&
		(value.Kind == "ssh" || value.Kind == "registry") && value.Status == "provisioned" && refPattern.MatchString(value.CredentialRef)
}

func decodeAdapterBody(r *http.Request, target any, maximum int64, exactKeys ...string) bool {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || r.ContentLength > maximum {
		return false
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maximum+1))
	if err != nil || len(raw) == 0 || int64(len(raw)) > maximum || !strictjson.Valid(raw) {
		zeroBytes(raw)
		return false
	}
	defer zeroBytes(raw)
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) != len(exactKeys) {
		return false
	}
	for _, key := range exactKeys {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target) == nil && decoder.Decode(new(any)) == io.EOF
}

func adapterReply(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func adapterFailure(w http.ResponseWriter, status int, code string) {
	adapterReply(w, status, map[string]map[string]string{"error": {"code": code}})
}
