// Package server exposes the private mTLS Harness HTTP boundary.
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

const DefaultActorHeader = "X-Harness-Actor-ID"

type Config struct {
	NodeID                   string
	GatewayCertificateSHA256 string
	ActorHeader              string
}

func New(config Config, authority *node.Node) (http.Handler, error) {
	if authority == nil || config.NodeID == "" || authority.NodeID() != config.NodeID {
		return nil, errors.New("server config is incomplete")
	}
	pin, err := hex.DecodeString(config.GatewayCertificateSHA256)
	if err != nil || len(pin) != sha256.Size {
		return nil, errors.New("gateway certificate SHA-256 pin is invalid")
	}
	if config.ActorHeader == "" {
		config.ActorHeader = DefaultActorHeader
	}
	if strings.ContainsAny(config.ActorHeader, "\r\n") {
		return nil, errors.New("actor header is invalid")
	}
	server := &Server{config: config, node: authority, gatewayPin: pin}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", server.live)
	mux.HandleFunc("GET /health/ready", server.ready)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/identity", server.identity)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/snapshot", server.snapshot)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/dialogs", server.dialogs)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/dialogs/{dialogId}/messages", server.history)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/requests", server.requests)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/requests/{requestId}/attempts", server.attempts)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/attempts/{attemptId}", server.attempt)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/attempts/{attemptId}/events", server.attemptEvents)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/artifacts/{artifactId}/metadata", server.artifactMetadata)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/artifacts/{artifactId}", server.artifact)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/events", server.events)
	mux.HandleFunc("POST /v1/nodes/{nodeId}/commands", server.command)
	mux.HandleFunc("GET /v1/nodes/{nodeId}/commands/{commandId}", server.commandStatus)
	server.handler = mux
	return server, nil
}

func queryLimit(request *http.Request) (int, bool) {
	value := request.URL.Query().Get("limit")
	if value == "" {
		return 0, true
	}
	limit, err := strconv.Atoi(value)
	return limit, err == nil && strconv.Itoa(limit) == value && limit >= 1 && limit <= harnessprotocol.MaximumPageSize
}

func validQuery(request *http.Request, allowed ...string) bool {
	permitted := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		permitted[key] = true
	}
	for key, values := range request.URL.Query() {
		if !permitted[key] || len(values) != 1 || values[0] == "" {
			return false
		}
	}
	return true
}

func (server *Server) dialogs(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "cursor", "limit") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	limit, ok := queryLimit(request)
	if !ok {
		writeResult(writer, server.node.Invalid("limit is invalid"))
		return
	}
	writeResult(writer, server.node.Dialogs(request.Context(), trust, request.URL.Query().Get("cursor"), limit))
}

func (server *Server) history(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "cursor", "limit") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	limit, ok := queryLimit(request)
	if !ok {
		writeResult(writer, server.node.Invalid("limit is invalid"))
		return
	}
	writeResult(writer, server.node.History(request.Context(), trust, request.PathValue("dialogId"), request.URL.Query().Get("cursor"), limit))
}

func (server *Server) requests(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "cursor", "limit", "state") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	limit, ok := queryLimit(request)
	if !ok {
		writeResult(writer, server.node.Invalid("limit is invalid"))
		return
	}
	writeResult(writer, server.node.Requests(request.Context(), trust, request.URL.Query().Get("state"), request.URL.Query().Get("cursor"), limit))
}

func (server *Server) attempts(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "cursor", "limit") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	limit, ok := queryLimit(request)
	if !ok {
		writeResult(writer, server.node.Invalid("limit is invalid"))
		return
	}
	writeResult(writer, server.node.Attempts(request.Context(), trust, request.PathValue("requestId"), request.URL.Query().Get("cursor"), limit))
}

func (server *Server) attempt(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.Attempt(request.Context(), trust, request.PathValue("attemptId")))
}

func (server *Server) attemptEvents(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "after", "limit") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	limit, ok := queryLimit(request)
	if !ok {
		writeResult(writer, server.node.Invalid("limit is invalid"))
		return
	}
	after := int64(0)
	if value := request.URL.Query().Get("after"); value != "" {
		var valid bool
		after, valid = parseSafeInteger(value)
		if !valid {
			writeResult(writer, server.node.Invalid("event cursor is invalid"))
			return
		}
	}
	writeResult(writer, server.node.AttemptEvents(request.Context(), trust, request.PathValue("attemptId"), after, limit))
}

func (server *Server) artifactMetadata(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.ArtifactMetadata(request.Context(), trust, request.PathValue("artifactId")))
}

func (server *Server) artifact(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	blob, failure, ok := server.node.Artifact(request.Context(), trust, request.PathValue("artifactId"))
	if !ok {
		writeResult(writer, failure)
		return
	}
	start, end, partial, valid := byteRange(request.Header.Get("Range"), int64(len(blob.Bytes)))
	if !valid {
		writer.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(int64(len(blob.Bytes)), 10))
		writer.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	writer.Header().Set("Content-Type", blob.Metadata.MediaType)
	writer.Header().Set("Content-Disposition", mime.FormatMediaType(blob.Metadata.Disposition, map[string]string{"filename": blob.Metadata.Name}))
	writer.Header().Set("Accept-Ranges", "bytes")
	writer.Header().Set("Cache-Control", "no-store")
	body := blob.Bytes
	if partial {
		writer.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(body)))
		body = body[start : end+1]
		writer.WriteHeader(http.StatusPartialContent)
	} else {
		writer.WriteHeader(http.StatusOK)
	}
	_, _ = writer.Write(body)
}

func byteRange(value string, size int64) (int64, int64, bool, bool) {
	if value == "" {
		return 0, size - 1, false, true
	}
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return 0, 0, false, false
	}
	left, right, found := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !found {
		return 0, 0, false, false
	}
	if left == "" {
		n, ok := parseSafeInteger(right)
		if !ok || n == 0 {
			return 0, 0, false, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, true
	}
	start, ok := parseSafeInteger(left)
	if !ok || start >= size {
		return 0, 0, false, false
	}
	end := size - 1
	if right != "" {
		end, ok = parseSafeInteger(right)
		if !ok || end < start {
			return 0, 0, false, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true, true
}

func parseSafeInteger(value string) (int64, bool) {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n <= harnessprotocol.MaximumSafeInteger
}

func (server *Server) events(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request, "after") {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	after, ok := parseSafeInteger(request.URL.Query().Get("after"))
	if !ok {
		writeResult(writer, server.node.Invalid("event cursor is invalid"))
		return
	}
	initial, failure, ok := server.node.ReplayEvents(request.Context(), trust, after, 100)
	if !ok {
		writeResult(writer, failure)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeResult(writer, server.node.Invalid("streaming is unavailable"))
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	flusher.Flush()
	last := after
	batch := initial
	heartbeat := time.NewTicker(15 * time.Second)
	poll := time.NewTicker(20 * time.Millisecond)
	defer heartbeat.Stop()
	defer poll.Stop()
	for {
		for _, raw := range batch.Events {
			var envelope struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(raw, &envelope) != nil || envelope.Seq != last+1 {
				return
			}
			_, _ = writer.Write([]byte("id: " + strconv.FormatInt(envelope.Seq, 10) + "\ndata: "))
			_, _ = writer.Write(raw)
			_, _ = writer.Write([]byte("\n\n"))
			last = envelope.Seq
		}
		if len(batch.Events) > 0 {
			flusher.Flush()
		}
		select {
		case <-request.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = writer.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case <-poll.C:
		}
		var good bool
		batch, failure, good = server.node.ReplayEvents(request.Context(), trust, last, 100)
		if !good {
			return
		}
	}
}

type Server struct {
	config     Config
	node       *node.Node
	gatewayPin []byte
	handler    http.Handler
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	server.handler.ServeHTTP(writer, request)
}

func (server *Server) trust(request *http.Request) (node.TrustContext, bool) {
	if request.TLS == nil || !request.TLS.HandshakeComplete || len(request.TLS.VerifiedChains) == 0 || len(request.TLS.PeerCertificates) == 0 {
		return node.TrustContext{}, false
	}
	digest := sha256.Sum256(request.TLS.PeerCertificates[0].Raw)
	if subtle.ConstantTimeCompare(digest[:], server.gatewayPin) != 1 {
		return node.TrustContext{}, false
	}
	actor := request.Header.Get(server.config.ActorHeader)
	if actor == "" {
		return node.TrustContext{}, false
	}
	transportNodeID := request.PathValue("nodeId")
	if transportNodeID == "" {
		transportNodeID = server.config.NodeID
	}
	return node.TrustContext{ActorID: actor, TransportNodeID: transportNodeID, PeerVerified: true}, true
}

func (server *Server) authenticate(writer http.ResponseWriter, request *http.Request) (node.TrustContext, bool) {
	trust, ok := server.trust(request)
	if ok {
		return trust, true
	}
	writeResult(writer, server.node.Forbidden())
	return node.TrustContext{}, false
}

func (server *Server) live(writer http.ResponseWriter, request *http.Request) {
	if _, ok := server.authenticate(writer, request); !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.HealthLive())
}

func (server *Server) ready(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.HealthReady(request.Context(), trust))
}

func (server *Server) identity(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.Identity(request.Context(), trust))
}

func (server *Server) snapshot(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.Snapshot(request.Context(), trust))
}

func (server *Server) commandStatus(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	writeResult(writer, server.node.CommandStatus(request.Context(), trust, request.PathValue("commandId")))
}

func (server *Server) command(writer http.ResponseWriter, request *http.Request) {
	trust, ok := server.authenticate(writer, request)
	if !ok {
		return
	}
	if !validQuery(request) {
		writeResult(writer, server.node.Invalid("query is invalid"))
		return
	}
	reader := http.MaxBytesReader(writer, request.Body, harnessprotocol.MaximumWireBytes)
	body, err := io.ReadAll(reader)
	if err != nil {
		writeResult(writer, server.node.Invalid("command exceeds the wire limit"))
		return
	}
	writeResult(writer, server.node.SubmitCommand(request.Context(), trust, body))
}

func writeResult(writer http.ResponseWriter, result node.Result) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(result.HTTPStatus)
	_, _ = writer.Write(result.Body)
}
