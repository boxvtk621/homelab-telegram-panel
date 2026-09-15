package harnessclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

var transcriptHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

type TranscriptChunkRequest struct {
	DialogID   string
	AttemptID  string
	TextID     string
	Source     transcriptview.Source
	ChunkIndex int64
	ArtifactID string
	SizeBytes  int64
	SHA256     string
}

type TranscriptChunkResponse struct {
	Request TranscriptChunkRequest
	Body    []byte
}

// ParseTranscriptChunk closes the browser-to-Router binary route over the
// exact source and manifest values. Arbitrary query keys never reach a node.
func ParseTranscriptChunk(path, rawQuery string) (TranscriptChunkRequest, error) {
	parts := strings.Split(path, "/")
	if len(parts) != 4 || parts[0] != "texts" || !uuid.MatchString(parts[1]) || parts[2] != "chunks" {
		return TranscriptChunkRequest{}, invalid()
	}
	chunkIndex, ok := safeNumber(parts[3])
	if !ok {
		return TranscriptChunkRequest{}, invalid()
	}
	query, err := url.ParseQuery(rawQuery)
	if err != nil {
		return TranscriptChunkRequest{}, invalid()
	}
	keys := []string{"dialogId", "attemptId", "sourceKind", "sourceId", "sourceIndex", "sourceStream", "artifactId", "sizeBytes", "sha256"}
	if len(query) != len(keys) {
		return TranscriptChunkRequest{}, invalid()
	}
	for _, key := range keys {
		if values := query[key]; len(values) != 1 || values[0] == "" {
			return TranscriptChunkRequest{}, invalid()
		}
	}
	sourceIndex, sourceOK := safeNumber(query.Get("sourceIndex"))
	sizeBytes, sizeOK := safeNumber(query.Get("sizeBytes"))
	source := transcriptview.Source{
		Kind: query.Get("sourceKind"), ID: query.Get("sourceId"), Index: sourceIndex, Stream: query.Get("sourceStream"),
	}
	request := TranscriptChunkRequest{
		DialogID: query.Get("dialogId"), AttemptID: query.Get("attemptId"), TextID: parts[1], Source: source,
		ChunkIndex: chunkIndex, ArtifactID: query.Get("artifactId"), SizeBytes: sizeBytes, SHA256: query.Get("sha256"),
	}
	if !sourceOK || !sizeOK || !uuid.MatchString(request.DialogID) || !uuid.MatchString(request.AttemptID) ||
		!uuid.MatchString(request.ArtifactID) || request.SizeBytes < 1 || request.SizeBytes > transcriptview.MaximumChunkBytes ||
		!transcriptHash.MatchString(request.SHA256) || transcriptview.ValidateSource(request.Source) != nil {
		return TranscriptChunkRequest{}, invalid()
	}
	return request, nil
}

func transcriptChunkQuery(request TranscriptChunkRequest) string {
	query := url.Values{
		"dialogId": {request.DialogID}, "attemptId": {request.AttemptID},
		"sourceKind": {request.Source.Kind}, "sourceId": {request.Source.ID},
		"sourceIndex": {strconv.FormatInt(request.Source.Index, 10)}, "sourceStream": {request.Source.Stream},
		"artifactId": {request.ArtifactID}, "sizeBytes": {strconv.FormatInt(request.SizeBytes, 10)}, "sha256": {request.SHA256},
	}
	return query.Encode()
}

func exactResponseHeader(response *http.Response, name, expected string) bool {
	values := response.Header.Values(name)
	return len(values) == 1 && values[0] == expected
}

// TranscriptChunk fetches at most one 16 MiB chunk. The private node proves
// the exact source binding and the client verifies identity, headers, size and
// hash before any bytes can cross the public Panel boundary.
func (c *Client) TranscriptChunk(ctx context.Context, nodeID, owner string, request TranscriptChunkRequest) (TranscriptChunkResponse, error) {
	path := "texts/" + request.TextID + "/chunks/" + strconv.FormatInt(request.ChunkIndex, 10)
	parsed, err := ParseTranscriptChunk(path, transcriptChunkQuery(request))
	if err != nil || parsed != request {
		return TranscriptChunkResponse{}, invalid()
	}
	entry, err := c.node(nodeID, owner)
	if err != nil {
		return TranscriptChunkResponse{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	identity, _, err := c.handshake(ctx, entry, owner)
	if err != nil {
		return TranscriptChunkResponse{}, err
	}
	upstream := "/v1/nodes/" + nodeID + "/" + path + "?" + transcriptChunkQuery(request)
	response, err := entry.requestAcceptExpected(ctx, owner, http.MethodGet, upstream, nil, "application/octet-stream", &identity)
	if err != nil {
		return TranscriptChunkResponse{}, err
	}
	if response.StatusCode != http.StatusOK {
		body, bodyErr := jsonBody(response)
		if bodyErr != nil {
			return TranscriptChunkResponse{}, bodyErr
		}
		if err := validateErrorStatus(response.StatusCode, body); err != nil {
			return TranscriptChunkResponse{}, err
		}
		var failure hp.Error
		_ = json.Unmarshal(body, &failure)
		return TranscriptChunkResponse{}, &Fault{Status: response.StatusCode, Code: failure.Code}
	}
	defer response.Body.Close()
	mediaType, params, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	disposition, dispositionParams, dispositionErr := mime.ParseMediaType(response.Header.Get("Content-Disposition"))
	if mediaErr != nil || mediaType != "application/octet-stream" || len(params) != 0 || dispositionErr != nil ||
		disposition != "attachment" || dispositionParams["filename"] != "safe-text-"+strconv.FormatInt(request.ChunkIndex, 10)+".txt" ||
		response.Header.Get("Content-Encoding") != "" || response.Header.Get("Content-Range") != "" ||
		(response.ContentLength >= 0 && response.ContentLength != request.SizeBytes) ||
		!exactResponseHeader(response, transcriptview.TextIDHeader, request.TextID) ||
		!exactResponseHeader(response, transcriptview.ChunkIndexHeader, strconv.FormatInt(request.ChunkIndex, 10)) ||
		!exactResponseHeader(response, transcriptview.ArtifactIDHeader, request.ArtifactID) ||
		!exactResponseHeader(response, transcriptview.ChunkSHA256Header, request.SHA256) {
		return TranscriptChunkResponse{}, notDurable()
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, request.SizeBytes+1))
	if err != nil || int64(len(body)) != request.SizeBytes {
		return TranscriptChunkResponse{}, notDurable()
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != request.SHA256 {
		return TranscriptChunkResponse{}, notDurable()
	}
	return TranscriptChunkResponse{Request: request, Body: body}, nil
}
