package harnessclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type Stream struct {
	body        io.ReadCloser
	cancel      context.CancelFunc
	scanner     *bufio.Scanner
	nodeID      string
	epoch, last int64
}

func (c *Client) OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*Stream, error) {
	if after < 0 || after > hp.MaximumSafeInteger {
		return nil, invalid()
	}
	e, err := c.node(nodeID, owner)
	if err != nil {
		return nil, err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	id, _, err := c.handshake(checkCtx, e, owner)
	cancel()
	if err != nil {
		return nil, err
	}
	// A stream has no overall client timeout. Its caller owns cancellation;
	// setup and non-200 bodies still have a bounded deadline.
	requestCtx, cancelRequest := context.WithCancel(ctx)
	timer := time.AfterFunc(2*time.Second, cancelRequest)
	retained := false
	defer func() {
		timer.Stop()
		if !retained {
			cancelRequest()
		}
	}()
	resp, err := e.requestAccept(requestCtx, owner, http.MethodGet, "/v1/nodes/"+nodeID+"/events?after="+strconv.FormatInt(after, 10), nil, "text/event-stream")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, err := jsonBody(resp)
		if err != nil {
			return nil, err
		}
		if err := validateErrorStatus(resp.StatusCode, body); err != nil {
			return nil, err
		}
		var failure hp.Error
		_ = json.Unmarshal(body, &failure)
		return nil, &Fault{Status: resp.StatusCode, Code: failure.Code}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" || resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close()
		return nil, mismatch()
	}
	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 16<<10), hp.MaximumWireBytes+1024)
	if !timer.Stop() || requestCtx.Err() != nil {
		resp.Body.Close()
		return nil, unavailable()
	}
	retained = true
	return &Stream{body: resp.Body, cancel: cancelRequest, scanner: s, nodeID: nodeID, epoch: id.IdentityEpoch, last: after}, nil
}

func (s *Stream) Close() error { s.cancel(); return s.body.Close() }

// Next accepts only the agreed single-line JSON framing and never forwards raw
// upstream diagnostics. Duplicate delivery is harmless; gaps require resnapshot.
func (s *Stream) Next() ([]byte, error) {
	var data []byte
	var id string
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if len(line) == 0 {
			if data == nil {
				id = ""
				continue
			}
			if hp.Validate("event", data) != nil {
				return nil, mismatch()
			}
			var event hp.EventEnvelope
			if json.Unmarshal(data, &event) != nil || event.NodeID != s.nodeID || event.Epoch != s.epoch || id != strconv.FormatInt(event.Seq, 10) {
				return nil, mismatch()
			}
			if event.Seq <= s.last {
				data = nil
				id = ""
				continue
			}
			if event.Seq != s.last+1 {
				return nil, &Fault{Status: 409, Code: "stale"}
			}
			s.last = event.Seq
			return data, nil
		}
		if line[0] == ':' {
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			return nil, mismatch()
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "id":
			if id != "" {
				return nil, mismatch()
			}
			id = string(value)
			if _, ok := safeNumber(id); !ok {
				return nil, mismatch()
			}
		case "data":
			if data != nil || len(value) > hp.MaximumWireBytes {
				return nil, mismatch()
			}
			data = bytes.Clone(value)
		case "event":
			if strings.TrimSpace(string(value)) != "message" {
				return nil, mismatch()
			}
		default:
			return nil, mismatch()
		}
	}
	if s.scanner.Err() != nil {
		return nil, unavailable()
	}
	if data != nil || id != "" {
		return nil, mismatch()
	}
	return nil, io.EOF
}
