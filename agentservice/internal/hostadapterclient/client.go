// Package hostadapterclient is the only Agent Service path to the Docker
// adapter host API. It exchanges safe descriptors and observations over a
// private Unix socket; secret input is forwarded once and never returned.
package hostadapterclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
)

const (
	ownerHeader = "X-Docker-Adapter-Owner"
	tokenHeader = "X-Docker-Adapter-Token"
	maxBody     = 128 << 10
)

var (
	ErrUnavailable    = errors.New("docker host adapter unavailable")
	ErrSecretConflict = errors.New("secret provisioning operation conflict")
	ErrSecretNotFound = errors.New("secret provisioning operation not found")
)

type Client struct {
	http      *http.Client
	probeHTTP *http.Client
	token     string
}

type hostDescriptor struct {
	SchemaID               string `json:"schemaId"`
	HostID                 string `json:"hostId"`
	HostVersion            int64  `json:"hostVersion"`
	DisplayName            string `json:"displayName"`
	Transport              string `json:"transport"`
	TargetRef              string `json:"targetRef"`
	CredentialRef          string `json:"credentialRef"`
	RegistryCredentialRef  string `json:"registryCredentialRef"`
	ExpectedHostKey        string `json:"expectedHostKey"`
	DockerContextRef       string `json:"dockerContextRef"`
	ExpectedIdentitySHA256 string `json:"expectedIdentitySHA256"`
	HostPlatform           string `json:"hostPlatform"`
	HostArchitecture       string `json:"hostArchitecture"`
}

func New(socket, token string) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 || strings.ContainsAny(socket, "\x00\r\n") ||
		len(token) < 32 || len(token) > 128 || strings.TrimSpace(token) != token || strings.ContainsAny(token, "\x00\r\n") {
		return nil, errors.New("invalid docker host adapter configuration")
	}
	dialer := &net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
		DisableCompression: true, MaxConnsPerHost: 4, MaxIdleConns: 4,
		IdleConnTimeout: 30 * time.Second, ResponseHeaderTimeout: 18 * time.Second,
	}
	probeTransport := transport.Clone()
	probeTransport.ResponseHeaderTimeout = 52 * time.Second
	checkRedirect := func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect,
	}, probeHTTP: &http.Client{Transport: probeTransport, CheckRedirect: checkRedirect}, token: token}, nil
}

func (c *Client) Close() {
	if transport, ok := c.http.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
	if c.probeHTTP != nil {
		if transport, ok := c.probeHTTP.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	}
}

func (c *Client) Provision(ctx context.Context, owner string, input model.HostSecretInput) (model.HostSecretProvision, error) {
	if !model.ValidActor(owner) || model.ValidateHostSecretInput(input) != nil {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	body, status, err := c.exchange(ctx, c.http, http.MethodPost, owner, "/internal/v1/secrets", input)
	if err != nil {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	defer zero(body)
	if status == http.StatusConflict {
		return model.HostSecretProvision{}, ErrSecretConflict
	}
	if status != http.StatusCreated && status != http.StatusOK {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	var result model.HostSecretProvision
	if !decode(body, &result) || model.ValidateHostSecretProvision(result, input.OperationID) != nil || result.Kind != input.Kind {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	result.Created = status == http.StatusCreated
	return result, nil
}

func (c *Client) ProvisionStatus(ctx context.Context, owner, operationID string) (model.HostSecretProvision, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(operationID) {
		return model.HostSecretProvision{}, ErrSecretNotFound
	}
	body, status, err := c.exchange(ctx, c.http, http.MethodGet, owner, "/internal/v1/secrets/"+operationID, nil)
	if err != nil {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	defer zero(body)
	if status == http.StatusNotFound {
		return model.HostSecretProvision{}, ErrSecretNotFound
	}
	if status != http.StatusOK {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	var result model.HostSecretProvision
	if !decode(body, &result) || model.ValidateHostSecretProvision(result, operationID) != nil {
		return model.HostSecretProvision{}, ErrUnavailable
	}
	return result, nil
}

func (c *Client) Probe(ctx context.Context, owner string, host model.HostRecord) (model.HostObservation, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(host.HostID) || host.HostVersion < 1 {
		return model.HostObservation{}, ErrUnavailable
	}
	descriptor := hostDescriptor{
		SchemaID: "docker-host-descriptor-v1", HostID: host.HostID, HostVersion: host.HostVersion,
		DisplayName: host.DisplayName, Transport: host.Transport, TargetRef: host.TargetRef,
		CredentialRef: host.CredentialRef, RegistryCredentialRef: host.RegistryCredentialRef,
		ExpectedHostKey: host.ExpectedHostKey, DockerContextRef: host.DockerContextRef,
		ExpectedIdentitySHA256: host.ExpectedIdentitySHA256, HostPlatform: host.HostPlatform,
		HostArchitecture: host.HostArchitecture,
	}
	var observation model.HostObservation
	client := c.probeHTTP
	if client == nil {
		client = c.http
	}
	if err := c.doPostWithClient(ctx, client, owner, "/internal/v1/probes", descriptor, http.StatusOK, &observation); err != nil ||
		model.ValidateHostObservation(observation) != nil || observation.HostID != host.HostID ||
		observation.HostVersion != host.HostVersion || observation.DockerContextRef != host.DockerContextRef {
		return model.HostObservation{}, ErrUnavailable
	}
	return observation, nil
}

func (c *Client) doPost(ctx context.Context, owner, path string, input any, expectedStatus int, output any) error {
	return c.doPostWithClient(ctx, c.http, owner, path, input, expectedStatus, output)
}

func (c *Client) doPostWithClient(ctx context.Context, client *http.Client, owner, path string, input any, expectedStatus int, output any) error {
	body, status, err := c.exchange(ctx, client, http.MethodPost, owner, path, input)
	if err != nil {
		return err
	}
	defer zero(body)
	if status != expectedStatus || !decode(body, output) {
		return ErrUnavailable
	}
	return nil
}

func (c *Client) exchange(ctx context.Context, client *http.Client, method, owner, path string, input any) ([]byte, int, error) {
	var reader io.Reader
	var raw []byte
	var err error
	if input != nil {
		raw, err = json.Marshal(input)
		if err != nil || len(raw) == 0 || len(raw) > maxBody {
			zero(raw)
			return nil, 0, ErrUnavailable
		}
		defer zero(raw)
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, "http://docker-adapter"+path, reader)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set(ownerHeader, owner)
	request.Header.Set(tokenHeader, c.token)
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, ErrUnavailable
	}
	defer response.Body.Close()
	mediaType, _, contentTypeErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if contentTypeErr != nil || mediaType != "application/json" || readErr != nil || len(body) == 0 || len(body) > maxBody || !registry.UniqueJSON(body) {
		zero(body)
		return nil, 0, ErrUnavailable
	}
	return body, response.StatusCode, nil
}

func decode(body []byte, output any) bool {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(output) != nil || decoder.Decode(new(any)) != io.EOF {
		return false
	}
	return true
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
