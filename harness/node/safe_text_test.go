package node_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/harness/fixture"
	"github.com/boxvtk621/homelab-telegram-panel/harness/node"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

func safeTextContent(value string, incomplete bool) harnessprotocol.SafeContent {
	preview := value
	if len(preview) > transcriptview.MaximumPreview {
		preview = preview[:transcriptview.MaximumPreview]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
	}
	return harnessprotocol.SafeContent{
		Kind: "inline", Content: preview, Redaction: "none",
		Truncated: incomplete || len(preview) != len(value),
	}
}

func readSafeTextManifest(t *testing.T, opened *node.Node, reference harnessadapter.AttemptRef, source transcriptview.Source) transcriptview.Manifest {
	t.Helper()
	result := opened.SafeTextManifest(context.Background(), nodeTrust(), reference.DialogID, reference.AttemptID, source)
	if result.HTTPStatus != 200 {
		t.Fatalf("manifest status=%d body=%s", result.HTTPStatus, result.Body)
	}
	manifest, err := transcriptview.Decode(result.Body)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func readSafeText(t *testing.T, opened *node.Node, manifest transcriptview.Manifest) []byte {
	t.Helper()
	value := make([]byte, manifest.SizeBytes)
	for _, chunk := range manifest.Chunks {
		metadataResult := opened.ArtifactMetadata(context.Background(), nodeTrust(), chunk.ArtifactID)
		if metadataResult.HTTPStatus != 200 || harnessprotocol.Validate("artifactMetadata", metadataResult.Body) != nil {
			t.Fatalf("chunk metadata status=%d body=%s", metadataResult.HTTPStatus, metadataResult.Body)
		}
		var metadata harnessprotocol.ArtifactMetadata
		if err := json.Unmarshal(metadataResult.Body, &metadata); err != nil {
			t.Fatal(err)
		}
		expectedCallID := ""
		if manifest.Source.Kind != "assistant_message" {
			expectedCallID = manifest.Source.ID
		}
		if metadata.DialogID != manifest.DialogID || metadata.AttemptID != manifest.AttemptID ||
			metadata.CallID != expectedCallID || metadata.ArtifactID != chunk.ArtifactID ||
			metadata.SizeBytes != chunk.SizeBytes || metadata.SHA256 != chunk.SHA256 ||
			metadata.Name != "safe-text-"+leftPadTwo(chunk.Index)+".txt" ||
			metadata.MediaType != "text/plain; charset=utf-8" || metadata.Truncated ||
			metadata.Redaction != manifest.Redaction || metadata.Disposition != "attachment" {
			t.Fatalf("chunk metadata is not exactly bound: %+v manifest=%+v", metadata, manifest)
		}
		blob, failure, ok := opened.Artifact(context.Background(), nodeTrust(), chunk.ArtifactID)
		if !ok || failure.HTTPStatus != 0 || blob.Metadata.ArtifactID != chunk.ArtifactID || int64(len(blob.Bytes)) != chunk.SizeBytes {
			t.Fatalf("chunk read failed ok=%v failure=%+v blob=%+v", ok, failure, blob.Metadata)
		}
		copy(value[chunk.OffsetBytes:chunk.OffsetBytes+chunk.SizeBytes], blob.Bytes)
	}
	return value
}

func leftPadTwo(value int64) string {
	if value < 10 {
		return "0" + string(rune('0'+value))
	}
	return string(rune('0'+value/10)) + string(rune('0'+value%10))
}

func TestSafeTextDurabilityChunkingScopeAndReplay(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	opened, reference := runningAttempt(t, path)

	messageID := "71000000-0000-4000-8000-000000000001"
	answer := strings.Repeat("a", transcriptview.MaximumPreview+257)
	event := harnessadapter.AssistantMessageEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, MessageID: messageID,
		Content: safeTextContent(answer, false), FinishReason: "complete", FullText: &answer,
	}
	if err := opened.ObserveAdapterEvent(ctx, reference, event); err != nil {
		t.Fatal(err)
	}
	source := transcriptview.Source{Kind: "assistant_message", ID: messageID, Stream: "none"}
	manifest := readSafeTextManifest(t, opened, reference, source)
	if !manifest.Complete || !manifest.PreviewTruncated || manifest.SizeBytes != int64(len(answer)) || len(manifest.Chunks) != 1 ||
		!bytes.Equal(readSafeText(t, opened, manifest), []byte(answer)) {
		t.Fatalf("complete answer was not retained: %+v", manifest)
	}
	chunkSize := manifest.Chunks[0].SizeBytes
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.AssistantMessageEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, MessageID: "71000000-0000-4000-8000-000000000010",
		Content: harnessprotocol.SafeContent{
			Kind: "artifact", ArtifactID: manifest.Chunks[0].ArtifactID, SizeBytes: &chunkSize,
			SHA256: manifest.Chunks[0].SHA256, Redaction: manifest.Redaction,
		}, FinishReason: "complete",
	}); err == nil || !strings.Contains(err.Error(), "artifact safe content binding mismatch") {
		t.Fatalf("internal transcript chunk was accepted as provider artifact: %v", err)
	}

	if err := opened.ObserveAdapterEvent(ctx, reference, event); err != nil {
		t.Fatalf("exact replay was not idempotent: %v", err)
	}
	conflict := answer[:len(answer)-1] + "b"
	conflictingEvent := event
	conflictingEvent.FullText = &conflict
	if err := opened.ObserveAdapterEvent(ctx, reference, conflictingEvent); err == nil || !strings.Contains(err.Error(), "conflicting safe text source") {
		t.Fatalf("same source with another full-text hash was accepted: %v", err)
	}

	multibyteID := "71000000-0000-4000-8000-000000000002"
	multibyte := strings.Repeat("€", transcriptview.MaximumChunkBytes/3+3)
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.AssistantMessageEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, MessageID: multibyteID,
		Content: safeTextContent(multibyte, false), FinishReason: "complete", FullText: &multibyte,
	}); err != nil {
		t.Fatal(err)
	}
	multibyteManifest := readSafeTextManifest(t, opened, reference, transcriptview.Source{Kind: "assistant_message", ID: multibyteID, Stream: "none"})
	if len(multibyteManifest.Chunks) != 2 || multibyteManifest.Chunks[0].SizeBytes > transcriptview.MaximumChunkBytes ||
		multibyteManifest.Chunks[1].OffsetBytes != multibyteManifest.Chunks[0].SizeBytes ||
		!bytes.Equal(readSafeText(t, opened, multibyteManifest), []byte(multibyte)) {
		t.Fatalf("UTF-8 text was not split into exact bounded chunks: %+v", multibyteManifest.Chunks)
	}

	oldMessageID := "71000000-0000-4000-8000-000000000003"
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.AssistantMessageEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, MessageID: oldMessageID,
		Content: harnessprotocol.SafeContent{Kind: "inline", Content: "old preview", Redaction: "none", Truncated: true}, FinishReason: "length",
	}); err != nil {
		t.Fatal(err)
	}
	missing := opened.SafeTextManifest(ctx, nodeTrust(), reference.DialogID, reference.AttemptID, transcriptview.Source{Kind: "assistant_message", ID: oldMessageID, Stream: "none"})
	if missing.HTTPStatus != 404 {
		t.Fatalf("old truncated result was presented as complete: %d %s", missing.HTTPStatus, missing.Body)
	}
	foreign := opened.SafeTextManifest(ctx, node.TrustContext{ActorID: "1-2", TransportNodeID: testNodeID, PeerVerified: true}, reference.DialogID, reference.AttemptID, source)
	if foreign.HTTPStatus != 403 {
		t.Fatalf("foreign owner read safe text: %d %s", foreign.HTTPStatus, foreign.Body)
	}
	wrongSource := source
	wrongSource.ID = "71000000-0000-4000-8000-000000000009"
	if result := opened.SafeTextManifest(ctx, nodeTrust(), reference.DialogID, reference.AttemptID, wrongSource); result.HTTPStatus != 404 {
		t.Fatalf("wrong source resolved: %d %s", result.HTTPStatus, result.Body)
	}

	var schemaVersion, transcriptTables, artifactRows, internalRows, manifestRows, publicArtifactEvents int
	db, err := sql.Open("sqlite", "file:"+filepath.Join(path, "harness.db")+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&schemaVersion); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name IN ('safe_texts','safe_text_chunks')").Scan(&transcriptTables); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts").Scan(&artifactRows); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE disposition='transcript_internal'").Scan(&internalRows); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM artifacts WHERE media_type='application/vnd.homelab.transcript-view-v1+json'").Scan(&manifestRows); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM events WHERE json_extract(CAST(event_json AS TEXT),'$.type')='artifact.available'`).Scan(&publicArtifactEvents); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	if schemaVersion != 2 || transcriptTables != 0 || artifactRows != 5 || internalRows != 5 || manifestRows != 2 || publicArtifactEvents != 0 {
		t.Fatalf("private transcript storage is not additive: schema=%d tables=%d artifacts=%d internal=%d manifests=%d events=%d", schemaVersion, transcriptTables, artifactRows, internalRows, manifestRows, publicArtifactEvents)
	}

	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	reopenConfig := testConfig(path)
	reopenConfig.Policies = fixture.NewPolicySource()
	opened, err = node.Open(ctx, reopenConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	restarted := readSafeTextManifest(t, opened, reference, source)
	if !bytes.Equal(readSafeText(t, opened, restarted), []byte(answer)) {
		t.Fatal("safe text changed after restart")
	}
}

func TestSafeToolTextLimitAndChunkIntegrity(t *testing.T) {
	ctx := context.Background()
	path := t.TempDir()
	opened, reference := runningAttempt(t, path)
	defer opened.Close()

	callID := "72000000-0000-4000-8000-000000000001"
	startTool(t, opened, reference, callID, "a")
	toolResult := strings.Repeat("t", 1<<20)
	content := safeTextContent(toolResult, false)
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.ToolOutputEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: callID, ChunkIndex: 0, Stream: "result",
		Output: content, FullText: &toolResult,
	}); err != nil {
		t.Fatal(err)
	}
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.ToolCompletedEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: callID, Status: "succeeded",
		Result: content, EffectStatus: "none", FullText: &toolResult,
	}); err != nil {
		t.Fatal(err)
	}
	for _, source := range []transcriptview.Source{
		{Kind: "tool_output", ID: callID, Index: 0, Stream: "result"},
		{Kind: "tool_result", ID: callID, Stream: "none"},
	} {
		manifest := readSafeTextManifest(t, opened, reference, source)
		if !manifest.Complete || manifest.SizeBytes != 1<<20 || !bytes.Equal(readSafeText(t, opened, manifest), []byte(toolResult)) {
			t.Fatalf("1 MiB tool result was not retained: %+v", manifest)
		}
	}

	incompleteCallID := "72000000-0000-4000-8000-000000000002"
	startTool(t, opened, reference, incompleteCallID, "b")
	retainedPrefix := strings.Repeat("p", 1<<20)
	incompleteContent := safeTextContent(retainedPrefix, true)
	if err := opened.ObserveAdapterEvent(ctx, reference, harnessadapter.ToolOutputEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: incompleteCallID, ChunkIndex: 0, Stream: "result",
		Output: incompleteContent, FullText: &retainedPrefix, FullTextIncomplete: true,
	}); err != nil {
		t.Fatal(err)
	}
	incomplete := readSafeTextManifest(t, opened, reference, transcriptview.Source{Kind: "tool_output", ID: incompleteCallID, Index: 0, Stream: "result"})
	if incomplete.Complete || incomplete.Reason != "output_limit_exceeded" || len(incomplete.Chunks) != 0 || !incomplete.PreviewTruncated {
		t.Fatalf("producer truncation was not explicit: %+v", incomplete)
	}

	complete := readSafeTextManifest(t, opened, reference, transcriptview.Source{Kind: "tool_result", ID: callID, Stream: "none"})
	chunk := complete.Chunks[0]
	if err := os.WriteFile(filepath.Join(path, "artifacts", chunk.ArtifactID), []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if result := opened.ArtifactMetadata(ctx, nodeTrust(), chunk.ArtifactID); result.HTTPStatus != 503 {
		t.Fatalf("corrupt safe text chunk remained readable: %d %s", result.HTTPStatus, result.Body)
	}
}

func startTool(t *testing.T, opened *node.Node, reference harnessadapter.AttemptRef, callID, hashDigit string) {
	t.Helper()
	if err := opened.ObserveAdapterEvent(context.Background(), reference, harnessadapter.ToolStartedEvent{
		EventBase: harnessadapter.EventBase{Attempt: reference}, CallID: callID,
		ToolName: "cursor.command", ActionHash: strings.Repeat(hashDigit, 64),
		Input: harnessprotocol.SafeContent{Kind: "inline", Content: "safe input", Redaction: "none"},
	}); err != nil {
		t.Fatal(err)
	}
}
