package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func canonicalHistoryPage(t *testing.T) HistoryExportPage {
	t.Helper()
	raw, err := os.ReadFile("../../../api/history-replica-v1.fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ValidPage HistoryExportPage `json:"validPage"`
	}
	if json.Unmarshal(raw, &fixture) != nil {
		t.Fatal("canonical fixture cannot be decoded")
	}
	return fixture.ValidPage
}

func TestCanonicalFixtureMatchesConsumerContract(t *testing.T) {
	schema, err := os.ReadFile("../../../api/history-replica-v1.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(schema)
	if hex.EncodeToString(digest[:]) != HistorySchemaSHA256 {
		t.Fatal("compiled history schema pin is stale")
	}
	page := canonicalHistoryPage(t)
	if err := ValidateHistoryExportPage(page); err != nil {
		t.Fatal(err)
	}
	page.Records[0].Payload = json.RawMessage(`{"changed":true}`)
	if err := ValidateHistoryExportPage(page); err == nil {
		t.Fatal("changed payload retained trusted hash")
	}
}

func TestHistoryRecordHashInputMatchesSemanticJSONAfterRoundTrip(t *testing.T) {
	record := canonicalHistoryPage(t).Records[0]
	hashInput, err := HistoryRecordHashInput(record)
	if err != nil || !HistoryRecordHashInputMatches(record, hashInput) {
		t.Fatalf("canonical hash input rejected: %v", err)
	}
	var formatted bytes.Buffer
	if json.Indent(&formatted, record.Payload, "", "  ") != nil {
		t.Fatal("canonical payload could not be reformatted")
	}
	record.Payload = formatted.Bytes()
	if !HistoryRecordHashInputMatches(record, hashInput) {
		t.Fatal("semantically equal JSONB-style payload was rejected")
	}
	record.Payload = json.RawMessage(`{"changed":true}`)
	if HistoryRecordHashInputMatches(record, hashInput) {
		t.Fatal("semantic payload change retained trusted hash input")
	}

	numeric := HistoryRecord{
		Type: HistoryRecordReceipt, RecordID: "10000000-0000-4000-8000-000000000030",
		EntityID: "10000000-0000-4000-8000-000000000031", Revision: 1,
		Payload: json.RawMessage(`{"opaque":9007199254740992,"normalized":1e2}`),
	}
	numericInput, err := HistoryRecordHashInput(numeric)
	if err != nil {
		t.Fatal(err)
	}
	numeric.RecordHash = HistoryHashBytes(numericInput)
	numeric.Payload = json.RawMessage(`{"normalized":100.00,"opaque":9007199254740992}`)
	if !HistoryRecordHashInputMatches(numeric, numericInput) {
		t.Fatal("exact numeric value after JSONB normalization was rejected")
	}
	numeric.Payload = json.RawMessage(`{"normalized":100,"opaque":9007199254740993}`)
	if HistoryRecordHashInputMatches(numeric, numericInput) {
		t.Fatal("distinct integer above IEEE-754 precision retained trusted hash input")
	}
}

func TestHistoryPageRejectsGapAndIdentityAlias(t *testing.T) {
	page := canonicalHistoryPage(t)
	page.AfterSeq = 1
	page.Records = nil
	page.Checkpoint.ThroughSeq = 2
	if err := ValidateHistoryExportPage(page); err == nil {
		t.Fatal("missing record gap was accepted")
	}
	page = canonicalHistoryPage(t)
	page.Identity.BindingGeneration++
	if err := ValidateHistoryExportPage(page); err == nil {
		t.Fatal("stream id was reusable across binding generations")
	}
}

func TestHistoryTextPreviewMatchesTranscriptContract(t *testing.T) {
	manifest := HistoryTextManifest{
		TextID: "10000000-0000-4000-8000-000000000020",
		Origin: HistoryOrigin{
			NodeID: "10000000-0000-4000-8000-000000000002", NodeDialogID: "10000000-0000-4000-8000-000000000003",
			AuthorEventID: "text:10000000-0000-4000-8000-000000000020",
		},
		LogicalDialogID: "10000000-0000-4000-8000-000000000001",
		AttemptID:       "10000000-0000-4000-8000-000000000021",
		Source:          json.RawMessage(`{"kind":"assistant_message"}`),
		Preview:         strings.Repeat("x", 65_536), Redaction: "none", Complete: true,
		SHA256: HistoryHashBytes(nil),
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	record := HistoryRecord{Type: HistoryRecordTextManifest, Payload: raw}
	if _, err := DecodeHistoryPayload(record); err != nil {
		t.Fatalf("64 KiB preview rejected: %v", err)
	}
	manifest.Preview += "x"
	raw, _ = json.Marshal(manifest)
	record.Payload = raw
	if _, err := DecodeHistoryPayload(record); err == nil {
		t.Fatal("preview above 64 KiB was accepted")
	}
}
