package harnessclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

func TestTranscriptChunkUsesExactScopeAndVerifiesBeforeDelivery(t *testing.T) {
	identity := fixture(t, "read.identity")
	body := []byte("safe transcript bytes")
	digest := sha256.Sum256(body)
	request := TranscriptChunkRequest{
		DialogID: "30000000-0000-4000-8000-000000000001", AttemptID: "60000000-0000-4000-8000-000000000001",
		TextID:     "50000000-0000-4000-8000-000000000001",
		Source:     transcriptview.Source{Kind: "assistant_message", ID: "40000000-0000-4000-8000-000000000001", Stream: "none"},
		ChunkIndex: 0, ArtifactID: "70000000-0000-4000-8000-000000000001", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	var calls atomic.Int32
	rig := newRig(t, func(writer http.ResponseWriter, incoming *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(incoming.URL.Path, "/identity") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(identity)
			return
		}
		if incoming.URL.Path != "/v1/nodes/"+testNode+"/texts/"+request.TextID+"/chunks/0" || incoming.URL.RawQuery != transcriptChunkQuery(request) {
			t.Errorf("unexpected transcript request %s?%s", incoming.URL.Path, incoming.URL.RawQuery)
		}
		for _, header := range []string{hp.ExpectedNodeIDHeader, hp.ExpectedRegistryHeader, hp.ExpectedEpochHeader, hp.ExpectedAdapterKindHeader, hp.ExpectedAdapterVersionHeader} {
			if incoming.Header.Get(header) == "" {
				t.Errorf("missing identity fence %s", header)
			}
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition", "attachment; filename=safe-text-0.txt")
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.Header().Set(transcriptview.TextIDHeader, request.TextID)
		writer.Header().Set(transcriptview.ChunkIndexHeader, "0")
		writer.Header().Set(transcriptview.ArtifactIDHeader, request.ArtifactID)
		writer.Header().Set(transcriptview.ChunkSHA256Header, request.SHA256)
		_, _ = writer.Write(body)
	})
	response, err := rig.client.TranscriptChunk(context.Background(), testNode, testOwner, request)
	if err != nil || response.Request != request || !bytes.Equal(response.Body, body) {
		t.Fatalf("exact transcript chunk failed: response=%+v err=%v", response, err)
	}
	before := calls.Load()
	invalid := request
	invalid.SHA256 = strings.ToUpper(invalid.SHA256)
	if _, err := rig.client.TranscriptChunk(context.Background(), testNode, testOwner, invalid); err == nil {
		t.Fatal("invalid transcript hash reached node")
	}
	if calls.Load() != before {
		t.Fatal("invalid transcript request reached node")
	}
}

func TestTranscriptChunkRejectsSwappedResponseHeader(t *testing.T) {
	identity := fixture(t, "read.identity")
	body := []byte("safe")
	digest := sha256.Sum256(body)
	request := TranscriptChunkRequest{
		DialogID: "30000000-0000-4000-8000-000000000001", AttemptID: "60000000-0000-4000-8000-000000000001",
		TextID:     "50000000-0000-4000-8000-000000000001",
		Source:     transcriptview.Source{Kind: "assistant_message", ID: "40000000-0000-4000-8000-000000000001", Stream: "none"},
		ChunkIndex: 0, ArtifactID: "70000000-0000-4000-8000-000000000001", SizeBytes: int64(len(body)), SHA256: hex.EncodeToString(digest[:]),
	}
	rig := newRig(t, func(writer http.ResponseWriter, incoming *http.Request) {
		if strings.HasSuffix(incoming.URL.Path, "/identity") {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(identity)
			return
		}
		writer.Header().Set("Content-Type", "application/octet-stream")
		writer.Header().Set("Content-Disposition", "attachment; filename=safe-text-0.txt")
		writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
		writer.Header().Set(transcriptview.TextIDHeader, request.TextID)
		writer.Header().Set(transcriptview.ChunkIndexHeader, "0")
		writer.Header().Set(transcriptview.ArtifactIDHeader, "70000000-0000-4000-8000-000000000009")
		writer.Header().Set(transcriptview.ChunkSHA256Header, request.SHA256)
		_, _ = writer.Write(body)
	})
	response, err := rig.client.TranscriptChunk(context.Background(), testNode, testOwner, request)
	if len(response.Body) != 0 {
		t.Fatal("bytes escaped before response binding validation")
	}
	requireFault(t, err, http.StatusServiceUnavailable, "not_durable")
}
