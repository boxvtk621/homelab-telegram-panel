package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	HistoryExportSchemaID    = "harness-history-export-v1"
	HistorySchemaSHA256      = "0bf89d20d3e8a5235248257fca1d64ce3af00c33eab3dbfc83a1c680f15ec221"
	HistoryReadSchemaID      = "history-replica-read-v1"
	HistoryReceiptSchemaID   = "history-origin-receipt-v1"
	HistoryMaximumPageSize   = 200
	HistoryMaximumPageBytes  = 8 * 1024 * 1024
	HistoryMaximumChunkBytes = 1024 * 1024
	HistoryMaximumSafeInt    = int64(1<<53 - 1)
	HistoryGenesisHash       = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	HistoryRecordEntry         = "entry"
	HistoryRecordFact          = "execution_fact"
	HistoryRecordReceipt       = "receipt_revision"
	HistoryRecordTextManifest  = "text_manifest"
	HistoryRecordTextChunk     = "text_chunk"
	HistoryRecordAssetManifest = "asset_manifest"
)

var historySHA256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type HistoryStreamIdentity struct {
	OwnerID           string `json:"ownerId"`
	LogicalDialogID   string `json:"logicalDialogId"`
	NodeID            string `json:"nodeId"`
	NodeDialogID      string `json:"nodeDialogId"`
	BindingGeneration int64  `json:"bindingGeneration"`
}

type HistoryOrigin struct {
	NodeID        string `json:"nodeId"`
	NodeDialogID  string `json:"nodeDialogId"`
	AuthorEventID string `json:"authorEventId"`
}

type HistoryRecord struct {
	StreamSeq  int64           `json:"streamSeq"`
	Type       string          `json:"type"`
	RecordID   string          `json:"recordId"`
	EntityID   string          `json:"entityId"`
	Revision   int64           `json:"revision"`
	Payload    json.RawMessage `json:"payload"`
	RecordHash string          `json:"recordHash"`
	PrevHash   string          `json:"prevHash"`
	ChainHash  string          `json:"chainHash"`
}

type HistoryEffectsSummary struct {
	EntryCount           int64 `json:"entryCount"`
	FactCount            int64 `json:"factCount"`
	ReceiptRevisionCount int64 `json:"receiptRevisionCount"`
	TextManifestCount    int64 `json:"textManifestCount"`
	TextChunkCount       int64 `json:"textChunkCount"`
	AssetManifestCount   int64 `json:"assetManifestCount"`
}

type HistoryCheckpoint struct {
	ThroughSeq        int64                 `json:"throughSeq"`
	ChainHash         string                `json:"chainHash"`
	DialogVersion     int64                 `json:"dialogVersion"`
	QueueRevision     int64                 `json:"queueRevision"`
	EntriesHash       string                `json:"entriesHash"`
	FactsHash         string                `json:"factsHash"`
	ReceiptsHash      string                `json:"receiptsHash"`
	TextHash          string                `json:"textHash"`
	AssetManifestHash string                `json:"assetManifestHash"`
	EffectsSummary    HistoryEffectsSummary `json:"effectsSummary"`
	Ready             bool                  `json:"ready"`
	IncompleteReason  string                `json:"incompleteReason,omitempty"`
	CapturedAt        string                `json:"capturedAt"`
}

type HistoryExportPage struct {
	SchemaID     string                `json:"schemaId"`
	SchemaSHA256 string                `json:"schemaSHA256"`
	StreamID     string                `json:"streamId"`
	Identity     HistoryStreamIdentity `json:"identity"`
	AfterSeq     int64                 `json:"afterSeq"`
	Records      []HistoryRecord       `json:"records"`
	NextAfter    *int64                `json:"nextAfterSeq"`
	Checkpoint   HistoryCheckpoint     `json:"checkpoint"`
}

type HistoryTranscriptEntry struct {
	EntryID          string          `json:"entryId"`
	Origin           HistoryOrigin   `json:"origin"`
	LogicalDialogID  string          `json:"logicalDialogId"`
	MessageID        string          `json:"messageId"`
	RequestID        string          `json:"requestId,omitempty"`
	AttemptID        string          `json:"attemptId,omitempty"`
	Kind             string          `json:"kind"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	CreatedAt        string          `json:"createdAt"`
	ProfileHash      string          `json:"profileHash,omitempty"`
	SourceGeneration int64           `json:"sourceGeneration"`
	ExecutionOrdinal int64           `json:"executionOrdinal"`
	ExecutionStatus  string          `json:"executionStatus"`
}

type HistoryExecutionFact struct {
	FactID          string          `json:"factId"`
	Origin          HistoryOrigin   `json:"origin"`
	LogicalDialogID string          `json:"logicalDialogId"`
	RequestID       string          `json:"requestId,omitempty"`
	AttemptID       string          `json:"attemptId,omitempty"`
	Kind            string          `json:"kind"`
	Status          string          `json:"status"`
	EventSeq        int64           `json:"eventSeq"`
	OccurredAt      string          `json:"occurredAt"`
	Event           json.RawMessage `json:"event"`
}

type HistoryReceiptRevision struct {
	ReceiptRevisionID string          `json:"receiptRevisionId"`
	Origin            HistoryOrigin   `json:"origin"`
	LogicalDialogID   string          `json:"logicalDialogId"`
	CommandID         string          `json:"commandId"`
	CommandKind       string          `json:"commandKind"`
	Fingerprint       string          `json:"fingerprint"`
	RequestID         string          `json:"requestId,omitempty"`
	AttemptID         string          `json:"attemptId,omitempty"`
	Revision          int64           `json:"revision"`
	PredecessorHash   string          `json:"predecessorHash"`
	Admission         string          `json:"admission"`
	Outcome           string          `json:"outcome"`
	Receipt           json.RawMessage `json:"receipt"`
	AcceptedAt        string          `json:"acceptedAt"`
}

type HistoryTextManifest struct {
	TextID          string          `json:"textId"`
	Origin          HistoryOrigin   `json:"origin"`
	LogicalDialogID string          `json:"logicalDialogId"`
	AttemptID       string          `json:"attemptId"`
	Source          json.RawMessage `json:"source"`
	Preview         string          `json:"preview"`
	Redaction       string          `json:"redaction"`
	Complete        bool            `json:"complete"`
	Reason          string          `json:"reason,omitempty"`
	SizeBytes       int64           `json:"sizeBytes"`
	SHA256          string          `json:"sha256"`
	ChunkCount      int64           `json:"chunkCount"`
}

type HistoryTextChunk struct {
	TextID          string `json:"textId"`
	LogicalDialogID string `json:"logicalDialogId"`
	ChunkIndex      int64  `json:"chunkIndex"`
	OffsetBytes     int64  `json:"offsetBytes"`
	SizeBytes       int64  `json:"sizeBytes"`
	SHA256          string `json:"sha256"`
	Bytes           []byte `json:"bytes"`
}

type HistoryAsset struct {
	ArtifactID string `json:"artifactId"`
	AttemptID  string `json:"attemptId"`
	CallID     string `json:"callId,omitempty"`
	Name       string `json:"name"`
	MediaType  string `json:"mediaType"`
	SizeBytes  int64  `json:"sizeBytes"`
	SHA256     string `json:"sha256"`
	Redaction  string `json:"redaction"`
	Truncated  bool   `json:"truncated"`
}

type HistoryAssetManifest struct {
	LogicalDialogID string         `json:"logicalDialogId"`
	Assets          []HistoryAsset `json:"assets"`
}

type HistoryReadPage struct {
	SchemaID        string                   `json:"schemaId"`
	LogicalDialogID string                   `json:"logicalDialogId"`
	Entries         []HistoryTranscriptEntry `json:"entries"`
	Facts           []HistoryExecutionFact   `json:"facts"`
	Receipts        []HistoryReceiptRevision `json:"receipts"`
	NextCursor      *string                  `json:"nextCursor"`
	SyncedThrough   HistoryCheckpoint        `json:"syncedThrough"`
	StreamCoverage  []HistoryStreamCoverage  `json:"streamCoverage"`
	ObservedAt      string                   `json:"observedAt"`
	LagMillis       int64                    `json:"lagMillis"`
	Incomplete      bool                     `json:"incomplete"`
}

type HistoryStreamCoverage struct {
	StreamID   string `json:"streamId"`
	ThroughSeq int64  `json:"throughSeq"`
	ChainHash  string `json:"chainHash"`
	Complete   bool   `json:"complete"`
	ObservedAt string `json:"observedAt"`
}

type HistoryReceiptLookup struct {
	SchemaID   string                 `json:"schemaId"`
	Receipt    HistoryReceiptRevision `json:"receipt"`
	Checkpoint HistoryCheckpoint      `json:"checkpoint"`
}

type HistoryImportResult struct {
	SchemaID        string `json:"schemaId"`
	StreamID        string `json:"streamId"`
	ImportedThrough int64  `json:"importedThrough"`
	SourceThrough   int64  `json:"sourceThrough"`
	Duplicate       bool   `json:"duplicate"`
	Complete        bool   `json:"complete"`
	ObservedAt      string `json:"observedAt"`
}

type historyRecordCore struct {
	Type     string          `json:"type"`
	RecordID string          `json:"recordId"`
	EntityID string          `json:"entityId"`
	Revision int64           `json:"revision"`
	Payload  json.RawMessage `json:"payload"`
}

func ValidateHistoryExportPage(page HistoryExportPage) error {
	if page.SchemaID != HistoryExportSchemaID || page.SchemaSHA256 != HistorySchemaSHA256 || !validHistoryIdentity(page.Identity) ||
		page.StreamID != HistoryStreamID(page.Identity) || page.AfterSeq < 0 || page.AfterSeq > HistoryMaximumSafeInt ||
		page.AfterSeq > page.Checkpoint.ThroughSeq ||
		len(page.Records) > HistoryMaximumPageSize || !validHistoryCheckpoint(page.Checkpoint) {
		return errors.New("history export page is invalid")
	}
	previous := HistoryChainGenesis(page.StreamID)
	if page.AfterSeq > 0 {
		if len(page.Records) == 0 {
			previous = page.Checkpoint.ChainHash
		} else {
			previous = page.Records[0].PrevHash
		}
	}
	for index, record := range page.Records {
		if record.StreamSeq != page.AfterSeq+int64(index)+1 || record.PrevHash != previous || validateHistoryRecord(page.StreamID, record) != nil {
			return errors.New("history export chain is invalid")
		}
		previous = record.ChainHash
	}
	if page.NextAfter != nil {
		if len(page.Records) == 0 || *page.NextAfter != page.Records[len(page.Records)-1].StreamSeq ||
			*page.NextAfter >= page.Checkpoint.ThroughSeq {
			return errors.New("history export cursor is invalid")
		}
	} else if len(page.Records) > 0 && page.Records[len(page.Records)-1].StreamSeq != page.Checkpoint.ThroughSeq {
		return errors.New("history export boundary is invalid")
	} else if len(page.Records) == 0 && page.AfterSeq != page.Checkpoint.ThroughSeq {
		return errors.New("history export boundary is missing records")
	} else if page.Checkpoint.ThroughSeq == 0 && page.Checkpoint.ChainHash != HistoryChainGenesis(page.StreamID) {
		return errors.New("history export empty chain is invalid")
	}
	raw, err := json.Marshal(page)
	if err != nil || len(raw) > HistoryMaximumPageBytes {
		return errors.New("history export page is too large")
	}
	return nil
}

func DecodeHistoryPayload(record HistoryRecord) (any, error) {
	var target any
	switch record.Type {
	case HistoryRecordEntry:
		target = &HistoryTranscriptEntry{}
	case HistoryRecordFact:
		target = &HistoryExecutionFact{}
	case HistoryRecordReceipt:
		target = &HistoryReceiptRevision{}
	case HistoryRecordTextManifest:
		target = &HistoryTextManifest{}
	case HistoryRecordTextChunk:
		target = &HistoryTextChunk{}
	case HistoryRecordAssetManifest:
		target = &HistoryAssetManifest{}
	default:
		return nil, errors.New("history record type is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(record.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) == nil {
		return nil, errors.New("history record payload is invalid")
	}
	if chunk, ok := target.(*HistoryTextChunk); ok {
		if chunk.SizeBytes != int64(len(chunk.Bytes)) || chunk.SizeBytes < 0 || chunk.SizeBytes > HistoryMaximumChunkBytes ||
			HistoryHashBytes(chunk.Bytes) != chunk.SHA256 {
			return nil, errors.New("history text chunk is invalid")
		}
	}
	if !validHistoryPayload(target) {
		return nil, errors.New("history record payload is invalid")
	}
	return target, nil
}

func validHistoryPayload(value any) bool {
	switch item := value.(type) {
	case *HistoryTranscriptEntry:
		return ValidUUID(item.EntryID) && validHistoryOrigin(item.Origin) && ValidUUID(item.LogicalDialogID) &&
			ValidUUID(item.MessageID) && validOptionalHistoryUUID(item.RequestID) && validOptionalHistoryUUID(item.AttemptID) &&
			boundedHistoryText(item.Kind, 120) && (item.Role == "user" || item.Role == "assistant") &&
			validHistoryObject(item.Content) && validHistoryTime(item.CreatedAt) &&
			(item.ProfileHash == "" || ValidSHA256(item.ProfileHash)) && item.SourceGeneration >= 0 &&
			item.SourceGeneration <= HistoryMaximumSafeInt && item.ExecutionOrdinal >= 1 &&
			item.ExecutionOrdinal <= HistoryMaximumSafeInt && boundedHistoryText(item.ExecutionStatus, 120)
	case *HistoryExecutionFact:
		return ValidUUID(item.FactID) && validHistoryOrigin(item.Origin) && ValidUUID(item.LogicalDialogID) &&
			validOptionalHistoryUUID(item.RequestID) && validOptionalHistoryUUID(item.AttemptID) &&
			boundedHistoryText(item.Kind, 120) && boundedHistoryText(item.Status, 120) &&
			item.EventSeq >= 1 && item.EventSeq <= HistoryMaximumSafeInt && validHistoryTime(item.OccurredAt) &&
			validHistoryObject(item.Event)
	case *HistoryReceiptRevision:
		return ValidUUID(item.ReceiptRevisionID) && validHistoryOrigin(item.Origin) && ValidUUID(item.LogicalDialogID) &&
			ValidUUID(item.CommandID) && validHistoryCommandKind(item.CommandKind) && ValidSHA256(item.Fingerprint) &&
			validOptionalHistoryUUID(item.RequestID) && validOptionalHistoryUUID(item.AttemptID) &&
			item.Revision >= 1 && item.Revision <= HistoryMaximumSafeInt && ValidSHA256(item.PredecessorHash) &&
			item.Admission == "accepted" && boundedHistoryText(item.Outcome, 120) && validHistoryObject(item.Receipt) &&
			validHistoryTime(item.AcceptedAt)
	case *HistoryTextManifest:
		return ValidUUID(item.TextID) && validHistoryOrigin(item.Origin) && ValidUUID(item.LogicalDialogID) &&
			ValidUUID(item.AttemptID) && validHistoryObject(item.Source) && utf8.ValidString(item.Preview) &&
			len([]byte(item.Preview)) <= 65_536 && boundedHistoryText(item.Redaction, 120) &&
			((item.Complete && item.Reason == "") || (!item.Complete && boundedHistoryText(item.Reason, 120))) &&
			item.SizeBytes >= 0 && item.SizeBytes <= HistoryMaximumSafeInt && ValidSHA256(item.SHA256) &&
			item.ChunkCount >= 0 && item.ChunkCount <= HistoryMaximumSafeInt && (item.Complete || item.ChunkCount == 0)
	case *HistoryTextChunk:
		return ValidUUID(item.TextID) && ValidUUID(item.LogicalDialogID) && item.ChunkIndex >= 0 &&
			item.ChunkIndex <= HistoryMaximumSafeInt && item.OffsetBytes >= 0 && item.OffsetBytes <= HistoryMaximumSafeInt &&
			item.SizeBytes == int64(len(item.Bytes)) && item.SizeBytes <= HistoryMaximumChunkBytes && ValidSHA256(item.SHA256)
	case *HistoryAssetManifest:
		if !ValidUUID(item.LogicalDialogID) || item.Assets == nil || len(item.Assets) > HistoryMaximumPageSize*HistoryMaximumPageSize {
			return false
		}
		for _, asset := range item.Assets {
			if !ValidUUID(asset.ArtifactID) || !ValidUUID(asset.AttemptID) || !validOptionalHistoryUUID(asset.CallID) ||
				!boundedHistoryText(asset.Name, 512) || !boundedHistoryText(asset.MediaType, 200) ||
				asset.SizeBytes < 0 || asset.SizeBytes > HistoryMaximumSafeInt || !ValidSHA256(asset.SHA256) ||
				!boundedHistoryText(asset.Redaction, 120) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func validHistoryOrigin(value HistoryOrigin) bool {
	return ValidUUID(value.NodeID) && ValidUUID(value.NodeDialogID) && boundedHistoryText(value.AuthorEventID, 256)
}

func validOptionalHistoryUUID(value string) bool { return value == "" || ValidUUID(value) }

func boundedHistoryText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && len([]byte(value)) <= maximum && !strings.ContainsRune(value, '\x00')
}

func validHistoryObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	return len(raw) > 0 && json.Unmarshal(raw, &value) == nil && value != nil
}

func validHistoryTime(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validHistoryCommandKind(value string) bool {
	switch value {
	case "dialog.create", "dialog.delete", "message.enqueue", "message.steer", "request.cancel",
		"attempt.stop", "queue.resume", "attempt.retry", "approval.respond", "input.respond":
		return true
	default:
		return false
	}
}

func HistoryHashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func HistoryFoldCoverageHash(previous, value string) string {
	return HistoryHashBytes([]byte("history-coverage-v1\x00" + previous + "\x00" + value))
}

func HistoryChainGenesis(streamID string) string {
	return HistoryHashBytes([]byte("history-chain-genesis-v1\x00" + streamID))
}

func HistoryStreamID(identity HistoryStreamIdentity) string {
	return HistoryStableUUID("history-stream-v1", strings.Join([]string{
		identity.OwnerID, identity.LogicalDialogID, identity.NodeID, identity.NodeDialogID,
		strconv.FormatInt(identity.BindingGeneration, 10),
	}, "\x00"))
}

func HistoryStableUUID(namespace, value string) string {
	digest := sha256.Sum256([]byte(namespace + "\x00" + value))
	raw := append([]byte(nil), digest[:16]...)
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func validHistoryIdentity(identity HistoryStreamIdentity) bool {
	return identity.OwnerID != "" && utf8.ValidString(identity.OwnerID) && len([]byte(identity.OwnerID)) <= 200 &&
		ValidUUID(identity.LogicalDialogID) && ValidUUID(identity.NodeID) && ValidUUID(identity.NodeDialogID) &&
		identity.BindingGeneration >= 1 && identity.BindingGeneration <= HistoryMaximumSafeInt
}

func validateHistoryRecord(streamID string, record HistoryRecord) error {
	validType := record.Type == HistoryRecordEntry || record.Type == HistoryRecordFact || record.Type == HistoryRecordReceipt ||
		record.Type == HistoryRecordTextManifest || record.Type == HistoryRecordTextChunk || record.Type == HistoryRecordAssetManifest
	if !ValidUUID(streamID) || !validType || !ValidUUID(record.RecordID) || !ValidUUID(record.EntityID) || record.Revision < 1 ||
		record.Revision > HistoryMaximumSafeInt || record.StreamSeq < 1 || record.StreamSeq > HistoryMaximumSafeInt ||
		!historySHA256Pattern.MatchString(record.RecordHash) || !historySHA256Pattern.MatchString(record.PrevHash) ||
		!historySHA256Pattern.MatchString(record.ChainHash) || len(record.Payload) == 0 || !json.Valid(record.Payload) {
		return errors.New("history record is invalid")
	}
	core := historyRecordCore{Type: record.Type, RecordID: record.RecordID, EntityID: record.EntityID, Revision: record.Revision, Payload: record.Payload}
	raw, err := json.Marshal(core)
	if err != nil || HistoryHashBytes(raw) != record.RecordHash {
		return errors.New("history record hash mismatch")
	}
	want := HistoryHashBytes([]byte("history-chain-v1\x00" + streamID + "\x00" + strconv.FormatInt(record.StreamSeq, 10) + "\x00" + record.PrevHash + "\x00" + record.RecordHash))
	if want != record.ChainHash {
		return errors.New("history record chain hash mismatch")
	}
	_, err = DecodeHistoryPayload(record)
	return err
}

func validHistoryCheckpoint(checkpoint HistoryCheckpoint) bool {
	if checkpoint.ThroughSeq < 0 || checkpoint.ThroughSeq > HistoryMaximumSafeInt || checkpoint.DialogVersion < 1 ||
		checkpoint.DialogVersion > HistoryMaximumSafeInt || checkpoint.QueueRevision < 1 || checkpoint.QueueRevision > HistoryMaximumSafeInt ||
		checkpoint.CapturedAt == "" || !historySHA256Pattern.MatchString(checkpoint.ChainHash) ||
		!historySHA256Pattern.MatchString(checkpoint.EntriesHash) || !historySHA256Pattern.MatchString(checkpoint.FactsHash) ||
		!historySHA256Pattern.MatchString(checkpoint.ReceiptsHash) || !historySHA256Pattern.MatchString(checkpoint.TextHash) ||
		!historySHA256Pattern.MatchString(checkpoint.AssetManifestHash) {
		return false
	}
	counts := checkpoint.EffectsSummary
	if counts.EntryCount < 0 || counts.FactCount < 0 || counts.ReceiptRevisionCount < 0 ||
		counts.TextManifestCount < 0 || counts.TextChunkCount < 0 || counts.AssetManifestCount < 0 ||
		counts.EntryCount+counts.FactCount+counts.ReceiptRevisionCount+counts.TextManifestCount+
			counts.TextChunkCount+counts.AssetManifestCount != checkpoint.ThroughSeq {
		return false
	}
	return (checkpoint.Ready && checkpoint.IncompleteReason == "") || (!checkpoint.Ready && checkpoint.IncompleteReason != "")
}
