package panel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

func harnessFailure(w http.ResponseWriter, err error) {
	status, code := 503, "node_unavailable"
	var fault *harnessclient.Fault
	if errors.As(err, &fault) {
		status, code = fault.Status, fault.Code
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		reply(w, 503, map[string]string{"error": "service_unavailable"})
		return
	}
	id[6] = (id[6] & 15) | 64
	id[8] = (id[8] & 63) | 128
	h := hex.EncodeToString(id[:])
	correlation := h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
	message := "Связь с нодой недоступна. Подтверждение не получено."
	switch code {
	case "invalid":
		message = "Некорректная команда или адрес."
	case "not_found":
		message = "Объект не найден."
	case "forbidden":
		message = "Изменения запрещены."
	case "stale":
		message = "Состояние изменилось. Обновите данные."
	case "schema_mismatch", "protocol_mismatch":
		message = "Нода использует несовместимый контракт."
	case "too_large":
		message = "Превышен размер сообщения."
	}
	reply(w, status, hp.Error{ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, Code: code, SafeMessage: message, Retryable: code == "queue_full" || code == "node_unavailable" || code == "not_durable", CorrelationID: correlation})
}

func harnessWire(w http.ResponseWriter, r harnessclient.Response) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(r.Status)
	_, _ = w.Write(r.Body)
}

func (s *Server) harnessPermit(w http.ResponseWriter, gate chan struct{}) bool {
	select {
	case gate <- struct{}{}:
		return true
	default:
		harnessFailure(w, &harnessclient.Fault{Status: 503, Code: "node_unavailable"})
		return false
	}
}

func (s *Server) harnessCommandBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	// Bounded before allocation/validation, independently of running commands
	// and streams. HTTP server ReadTimeout also bounds slow request bodies.
	if !s.harnessPermit(w, s.commandBodies) {
		return nil, false
	}
	defer func() { <-s.commandBodies }()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hp.MaximumWireBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			harnessFailure(w, &harnessclient.Fault{Status: 413, Code: "too_large"})
		} else {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
		}
		return nil, false
	}
	if hp.Validate("command", body) != nil {
		harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
		return nil, false
	}
	return body, true
}

func (s *Server) harnessHTTP(w http.ResponseWriter, r *http.Request, sessionID string, v session) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/harness/")
	if path == "nodes" && r.Method == http.MethodGet && r.URL.RawQuery == "" {
		registry, ok := s.router.Public(v.user.ID)
		if !ok {
			harnessFailure(w, &harnessclient.Fault{Status: 403, Code: "forbidden"})
			return
		}
		reply(w, 200, registry)
		return
	}
	parts := strings.SplitN(path, "/", 3)
	if len(parts) != 3 || parts[0] != "nodes" {
		harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
		return
	}
	nodeID, route := parts[1], parts[2]
	if r.Method == http.MethodPost {
		if route != "commands" {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
			return
		}
		if !s.cfg.Writes {
			harnessFailure(w, &harnessclient.Fault{Status: 403, Code: "forbidden"})
			return
		}
		ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || ct != "application/json" {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
			return
		}
		body, ok := s.harnessCommandBody(w, r)
		if !ok {
			return
		}
		var command hp.CommandEnvelope
		_ = json.Unmarshal(body, &command)
		gate := s.general
		switch command.Kind {
		case hp.CommandMessageSteer, hp.CommandAttemptStop, hp.CommandQueueResume, hp.CommandRequestCancel, hp.CommandApprovalRespond, hp.CommandInputRespond:
			gate = s.control
		}
		if !s.harnessPermit(w, gate) {
			return
		}
		defer func() { <-gate }()
		response, err := s.router.Command(r.Context(), nodeID, v.user.ID, body)
		if err != nil {
			harnessFailure(w, err)
			return
		}
		harnessWire(w, response)
		return
	}
	if route == "events" {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil || len(q) != 1 || len(q["after"]) != 1 {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
			return
		}
		after, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if err != nil || after < 0 || after > hp.MaximumSafeInteger || strconv.FormatInt(after, 10) != q.Get("after") {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
			return
		}
		if !s.harnessPermit(w, s.streams) {
			return
		}
		defer func() { <-s.streams }()
		s.harnessEvents(w, r, sessionID, v.user.ID, nodeID, after)
		return
	}
	if !s.harnessPermit(w, s.general) {
		return
	}
	defer func() { <-s.general }()
	if item := strings.Split(route, "/"); len(item) == 2 && item[0] == "artifacts" {
		if r.URL.RawQuery != "" {
			harnessFailure(w, &harnessclient.Fault{Status: 400, Code: "invalid"})
			return
		}
		artifact, err := s.router.Artifact(r.Context(), nodeID, v.user.ID, item[1], strings.Join(r.Header.Values("Range"), ","))
		if err != nil {
			var invalidRange *harnessclient.RangeError
			if errors.As(err, &invalidRange) {
				w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(invalidRange.Size, 10))
				w.WriteHeader(416)
				return
			}
			harnessFailure(w, err)
			return
		}
		if _, ok := s.sessions.peek(sessionID); !ok {
			reply(w, 401, map[string]string{"error": "authentication_required"})
			return
		}
		// Downloads never render arbitrary node-supplied content in Panel origin.
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": artifact.Metadata.Name}))
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", strconv.Itoa(len(artifact.Body)))
		if artifact.ContentRange != "" {
			w.Header().Set("Content-Range", artifact.ContentRange)
		}
		w.WriteHeader(artifact.Status)
		_, _ = w.Write(artifact.Body)
		return
	}
	response, err := s.router.Read(r.Context(), nodeID, v.user.ID, route, r.URL.RawQuery)
	if err != nil {
		harnessFailure(w, err)
		return
	}
	harnessWire(w, response)
}

func (s *Server) harnessEvents(w http.ResponseWriter, r *http.Request, sessionID, owner, nodeID string, after int64) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	stream, err := s.router.OpenEvents(ctx, nodeID, owner, after)
	if err != nil {
		harnessFailure(w, err)
		return
	}
	defer stream.Close()
	if _, ok := s.sessions.peek(sessionID); !ok {
		reply(w, 401, map[string]string{"error": "authentication_required"})
		return
	}
	control := http.NewResponseController(w)
	write := func(body []byte) bool {
		if _, ok := s.sessions.peek(sessionID); !ok {
			return false
		}
		if control.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
			return false
		}
		if _, err := w.Write(body); err != nil {
			return false
		}
		return control.Flush() == nil
	}
	if control.SetWriteDeadline(time.Now().Add(10*time.Second)) != nil {
		harnessFailure(w, &harnessclient.Fault{Status: 503, Code: "node_unavailable"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	if !write([]byte(": connected\n\n")) {
		return
	}
	type observation struct {
		body []byte
		err  error
	}
	next := make(chan observation, 1)
	go func() {
		for {
			body, err := stream.Next()
			select {
			case next <- observation{body: body, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastHeartbeat := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case observed := <-next:
			if observed.err != nil {
				return
			}
			var event hp.EventEnvelope
			_ = json.Unmarshal(observed.body, &event)
			frame := []byte("id: " + strconv.FormatInt(event.Seq, 10) + "\ndata: ")
			frame = append(frame, observed.body...)
			frame = append(frame, '\n', '\n')
			if !write(frame) {
				return
			}
		case now := <-tick.C:
			if _, ok := s.sessions.peek(sessionID); !ok {
				return
			}
			if now.Sub(lastHeartbeat) >= 5*time.Second {
				if !write([]byte(": keep-alive\n\n")) {
					return
				}
				lastHeartbeat = now
			}
		}
	}
}
