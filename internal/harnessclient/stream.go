package harnessclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type Stream struct {
	mu           sync.Mutex
	client       *Client
	entry        *entry
	owner        string
	ctx          context.Context
	cancel       context.CancelFunc
	bodyCancel   context.CancelFunc
	body         io.ReadCloser
	scanner      *bufio.Scanner
	identity     hp.NodeIdentity
	nodeID       string
	last         int64
	closed       bool
	reconnects   int
	incomplete   bool
	waitForRetry func(context.Context, time.Duration) error
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
	streamCtx, cancelStream := context.WithCancel(ctx)
	stream := &Stream{
		client: c, entry: e, owner: owner, ctx: streamCtx, cancel: cancelStream,
		identity: id, nodeID: nodeID, last: after, waitForRetry: waitForStreamRetry,
	}
	if err := stream.open(); err != nil {
		cancelStream()
		return nil, err
	}
	return stream, nil
}

func (s *Stream) open() error {
	requestCtx, cancelRequest := context.WithCancel(s.ctx)
	timer := time.AfterFunc(2*time.Second, cancelRequest)
	retained := false
	defer func() {
		timer.Stop()
		if !retained {
			cancelRequest()
		}
	}()
	resp, err := s.entry.requestAccept(requestCtx, s.owner, http.MethodGet,
		"/v1/nodes/"+s.nodeID+"/events?after="+strconv.FormatInt(s.last, 10), nil, "text/event-stream")
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		body, err := jsonBody(resp)
		if err != nil {
			return err
		}
		if err := validateErrorStatus(resp.StatusCode, body); err != nil {
			return err
		}
		var failure hp.Error
		_ = json.Unmarshal(body, &failure)
		return &Fault{Status: resp.StatusCode, Code: failure.Code}
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || media != "text/event-stream" || resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close()
		return mismatch()
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 16<<10), hp.MaximumWireBytes+1024)
	s.incomplete = false
	scanner.Split(boundedSSELines(&s.incomplete))
	if !timer.Stop() || requestCtx.Err() != nil {
		resp.Body.Close()
		return unavailable()
	}
	s.body = resp.Body
	s.bodyCancel = cancelRequest
	s.scanner = scanner
	retained = true
	return nil
}

func (s *Stream) Close() error {
	// Cancellation must happen before waiting for Next's serialization lock;
	// otherwise a blocked scanner and Close would deadlock each other.
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.bodyCancel != nil {
		s.bodyCancel()
	}
	if s.body != nil {
		return s.body.Close()
	}
	return nil
}

// Next preserves the exact last accepted cursor. A clean or broken transport
// is reopened from that cursor; duplicates remain harmless and a gap/epoch
// change is still stale rather than silently skipped.
func (s *Stream) Next() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		data, ended, err := s.nextFrame()
		if !ended {
			return data, err
		}
		if err := s.reopen(); err != nil {
			return nil, err
		}
	}
}

func (s *Stream) nextFrame() ([]byte, bool, error) {
	var data []byte
	var id string
	for s.scanner.Scan() {
		line := s.scanner.Bytes()
		if s.incomplete {
			// Scanner yields a final unterminated line before exposing the
			// underlying read error. Probe once more before interpreting that
			// line so a torn field name/value is discarded on transport loss.
			line = bytes.Clone(line)
			if s.scanner.Scan() {
				return nil, false, mismatch()
			}
			s.incomplete = false
			if s.scanner.Err() != nil {
				return nil, true, nil
			}
		}
		if len(line) > hp.MaximumWireBytes+8 {
			return nil, false, mismatch()
		}
		if len(line) == 0 {
			if data == nil {
				id = ""
				continue
			}
			if hp.Validate("event", data) != nil {
				return nil, false, mismatch()
			}
			var event hp.EventEnvelope
			if json.Unmarshal(data, &event) != nil || event.NodeID != s.nodeID || event.Epoch != s.identity.IdentityEpoch || id != strconv.FormatInt(event.Seq, 10) {
				return nil, false, mismatch()
			}
			if event.Seq <= s.last {
				data = nil
				id = ""
				continue
			}
			if event.Seq != s.last+1 {
				return nil, false, &Fault{Status: 409, Code: "stale"}
			}
			s.last = event.Seq
			s.reconnects = 0
			return data, false, nil
		}
		if line[0] == ':' {
			continue
		}
		name, value, found := bytes.Cut(line, []byte(":"))
		if !found {
			return nil, false, mismatch()
		}
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(name) {
		case "id":
			if id != "" {
				return nil, false, mismatch()
			}
			id = string(value)
			if _, ok := safeNumber(id); !ok {
				return nil, false, mismatch()
			}
		case "data":
			if data != nil || len(value) > hp.MaximumWireBytes {
				return nil, false, mismatch()
			}
			data = bytes.Clone(value)
		case "event":
			if strings.TrimSpace(string(value)) != "message" {
				return nil, false, mismatch()
			}
		default:
			return nil, false, mismatch()
		}
	}
	if s.closed || s.ctx.Err() != nil {
		return nil, false, io.EOF
	}
	if s.scanner.Err() != nil {
		// A transport failure may leave an id/data fragment in the scanner.
		// Discard it and reopen from the last fully accepted cursor. Clean EOF
		// with an incomplete frame remains a protocol mismatch below.
		return nil, true, nil
	}
	if data != nil || id != "" {
		return nil, false, mismatch()
	}
	return nil, false, io.EOF
}

func boundedSSELines(incomplete *bool) bufio.SplitFunc {
	return func(data []byte, atEOF bool) (advance int, token []byte, err error) {
		*incomplete = false
		// Keep Scanner's I/O errors distinguishable from an oversized protocol
		// line: surface an over-limit line as a token so nextFrame fails closed
		// instead of retrying it as a broken transport.
		if len(data) > hp.MaximumWireBytes+8 {
			return len(data), data, nil
		}
		if atEOF && len(data) > 0 && !bytes.Contains(data, []byte{'\n'}) {
			*incomplete = true
		}
		return bufio.ScanLines(data, atEOF)
	}
}

func retryableStreamError(err error) bool {
	var fault *Fault
	return errors.As(err, &fault) && fault.Status == http.StatusServiceUnavailable && fault.Code == "node_unavailable"
}

func (s *Stream) reopen() error {
	if s.bodyCancel != nil {
		s.bodyCancel()
	}
	if s.body != nil {
		_ = s.body.Close()
	}
	for {
		checkCtx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
		identity, _, err := s.client.handshake(checkCtx, s.entry, s.owner)
		cancel()
		if err == nil {
			if identity != s.identity {
				return stale()
			}
			if err = s.open(); err == nil {
				return nil
			}
		}
		if !retryableStreamError(err) {
			return err
		}
		s.reconnects++
		if err := s.waitForRetry(s.ctx, streamRetryDelay(s.nodeID, s.reconnects)); err != nil {
			return io.EOF
		}
	}
}

func streamRetryDelay(nodeID string, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	maximum := min(1<<min(attempt-1, 5), 30)
	digest := sha256.Sum256([]byte(nodeID + "\x00" + strconv.Itoa(attempt)))
	return time.Duration(1+int(digest[0])%maximum) * time.Second
}

func waitForStreamRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
