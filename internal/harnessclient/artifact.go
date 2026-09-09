package harnessclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type BinaryResponse struct {
	Metadata     hp.ArtifactMetadata
	Body         []byte
	Status       int
	ContentRange string
}

type RangeError struct{ Size int64 }

func (e *RangeError) Error() string { return "range_not_satisfiable" }
func notDurable() error             { return &Fault{Status: 503, Code: "not_durable"} }

// Artifact authenticates metadata and buffers at most 16 MiB. No byte reaches
// the browser until the entire immutable artifact's size/hash is verified.
// Ranges are sliced locally after verification, never trusted as hash evidence.
func (c *Client) Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (BinaryResponse, error) {
	if !uuid.MatchString(artifactID) {
		return BinaryResponse{}, invalid()
	}
	metadata, err := c.Read(ctx, nodeID, owner, "artifacts/"+artifactID+"/metadata", "")
	if err != nil {
		return BinaryResponse{}, err
	}
	if metadata.Status != 200 {
		var e hp.Error
		_ = json.Unmarshal(metadata.Body, &e)
		return BinaryResponse{}, &Fault{Status: metadata.Status, Code: e.Code}
	}
	var m hp.ArtifactMetadata
	if json.Unmarshal(metadata.Body, &m) != nil {
		return BinaryResponse{}, unavailable()
	}
	start, end, status, err := artifactRange(byteRange, m.SizeBytes)
	if err != nil {
		return BinaryResponse{}, err
	}
	e, err := c.node(nodeID, owner)
	if err != nil {
		return BinaryResponse{}, err
	}
	// The metadata read is bounded to 2s; a full 16 MiB artifact has its own
	// 15s transfer budget, within Panel's 20s response deadline.
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resp, err := e.requestAccept(ctx, owner, http.MethodGet, "/v1/nodes/"+nodeID+"/artifacts/"+artifactID, nil, "application/octet-stream")
	if err != nil {
		return BinaryResponse{}, err
	}
	if resp.StatusCode != 200 {
		body, err := jsonBody(resp)
		if err != nil {
			return BinaryResponse{}, err
		}
		if err := validateErrorStatus(resp.StatusCode, body); err != nil {
			return BinaryResponse{}, err
		}
		var failure hp.Error
		_ = json.Unmarshal(body, &failure)
		return BinaryResponse{}, &Fault{Status: resp.StatusCode, Code: failure.Code}
	}
	defer resp.Body.Close()
	media, params, mediaErr := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	wantMedia, wantParams, wantErr := mime.ParseMediaType(m.MediaType)
	disposition, dispParams, dispErr := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
	if mediaErr != nil || wantErr != nil || media != wantMedia || !equalParams(params, wantParams) || dispErr != nil || disposition != m.Disposition || dispParams["filename"] != m.Name || resp.Header.Get("Content-Encoding") != "" || resp.Header.Get("Content-Range") != "" || (resp.ContentLength >= 0 && resp.ContentLength != m.SizeBytes) {
		return BinaryResponse{}, notDurable()
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, m.SizeBytes+1))
	if err != nil || int64(len(body)) != m.SizeBytes {
		return BinaryResponse{}, notDurable()
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != m.SHA256 {
		return BinaryResponse{}, notDurable()
	}
	result := BinaryResponse{Metadata: m, Body: body, Status: status}
	if status == 206 {
		result.Body = body[start : end+1]
		result.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, m.SizeBytes)
	}
	return result, nil
}

func equalParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func artifactRange(value string, size int64) (int64, int64, int, error) {
	if value == "" {
		return 0, size - 1, 200, nil
	}
	bad := func() (int64, int64, int, error) { return 0, 0, 0, &RangeError{Size: size} }
	if !strings.HasPrefix(value, "bytes=") || strings.Contains(value, ",") || size == 0 {
		return bad()
	}
	left, right, ok := strings.Cut(strings.TrimPrefix(value, "bytes="), "-")
	if !ok {
		return bad()
	}
	if left == "" {
		n, ok := safeNumber(right)
		if !ok || n == 0 {
			return bad()
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, 206, nil
	}
	start, ok := safeNumber(left)
	if !ok || start >= size {
		return bad()
	}
	end := size - 1
	if right != "" {
		end, ok = safeNumber(right)
		if !ok || end < start {
			return bad()
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, 206, nil
}
