package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

var (
	ErrHistoryReplicaGap      = errors.New("history replica gap")
	ErrHistoryReplicaConflict = errors.New("history replica conflict")
	ErrHistoryReplicaScope    = errors.New("history replica scope mismatch")
)

const (
	HistoryReplicaFreshnessWindow = 5 * time.Second
	historyMaximumReadStreams     = 24
	historyMaximumCursorBytes     = 8192
	historyReadDataBudget         = model.HistoryMaximumPageBytes - historyMaximumCursorBytes
)

type HistoryReadHorizon struct {
	StreamID   string `json:"streamId"`
	ThroughSeq int64  `json:"throughSeq"`
	ChainHash  string `json:"chainHash"`
	Complete   bool   `json:"complete"`
	ObservedAt string `json:"observedAt"`
}

type HistoryReadPosition struct {
	Horizons          []HistoryReadHorizon    `json:"horizons"`
	Checkpoint        model.HistoryCheckpoint `json:"checkpoint"`
	ObservedAt        string                  `json:"observedAt"`
	SourceCapturedAt  string                  `json:"sourceCapturedAt"`
	Complete          bool                    `json:"complete"`
	EntryCreatedAt    string                  `json:"entryCreatedAt,omitempty"`
	EntryID           string                  `json:"entryId,omitempty"`
	FactOccurredAt    string                  `json:"factOccurredAt,omitempty"`
	FactID            string                  `json:"factId,omitempty"`
	ReceiptAcceptedAt string                  `json:"receiptAcceptedAt,omitempty"`
	ReceiptRevisionID string                  `json:"receiptRevisionId,omitempty"`
}

type HistoryReadResult struct {
	Page     model.HistoryReadPage
	HasMore  bool
	Position HistoryReadPosition
}

func (s *Store) ApplyHistoryReplicaPage(ctx context.Context, owner string, page model.HistoryExportPage) (model.HistoryImportResult, error) {
	if owner == "" || owner != page.Identity.OwnerID || model.ValidateHistoryExportPage(page) != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	capturedAt, err := time.Parse(time.RFC3339Nano, page.Checkpoint.CapturedAt)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	sourceCheckpointJSON, err := json.Marshal(page.Checkpoint)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	initialImported := emptyHistoryCheckpoint(page.StreamID, page.Checkpoint)
	initialImportedJSON, err := json.Marshal(initialImported)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.HistoryImportResult{}, errors.New("history replica transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Logical-dialog serialization is ordered before the per-stream lock so
	// delete, transfer, retirement, and late replica import cannot deadlock or
	// cross the tombstone CAS.
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "logical-dialog:"+owner+":"+page.Identity.LogicalDialogID); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica dialog lock unavailable")
	}
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "history-replica:"+owner+":"+page.StreamID); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica lock unavailable")
	}
	var bindingMarker int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM agent_service.dialog_bindings b
		JOIN agent_service.logical_dialogs d ON d.owner_id=b.owner_id AND d.logical_dialog_id=b.logical_dialog_id
		WHERE b.owner_id=$1 AND b.logical_dialog_id=$2 AND b.node_id=$3 AND b.node_dialog_id=$4
			AND b.binding_version=$5 AND b.state='active' AND d.deleted_at IS NULL
		FOR UPDATE OF b,d`, owner, page.Identity.LogicalDialogID, page.Identity.NodeID, page.Identity.NodeDialogID,
		page.Identity.BindingGeneration).Scan(&bindingMarker); err != nil || bindingMarker != 1 {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_replica_streams(
		owner_id,stream_id,logical_dialog_id,node_id,node_dialog_id,binding_generation,
		imported_through,imported_chain_hash,imported_checkpoint,source_through,source_chain_hash,source_checkpoint,
		source_captured_at,observed_at,complete)
		VALUES($1,$2,$3,$4,$5,$6,0,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT(owner_id,stream_id) DO NOTHING`,
		owner, page.StreamID, page.Identity.LogicalDialogID, page.Identity.NodeID, page.Identity.NodeDialogID,
		page.Identity.BindingGeneration, model.HistoryChainGenesis(page.StreamID), initialImportedJSON, page.Checkpoint.ThroughSeq,
		page.Checkpoint.ChainHash, sourceCheckpointJSON, capturedAt, s.now().UTC(), false); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica stream unavailable")
	}
	var stored model.HistoryStreamIdentity
	var importedThrough, sourceThrough int64
	var importedChain, sourceChain string
	var storedComplete bool
	var importedCheckpointRaw, storedSourceCheckpointRaw []byte
	if err := tx.QueryRow(ctx, `SELECT owner_id,logical_dialog_id,node_id,node_dialog_id,binding_generation,
		imported_through,imported_chain_hash,imported_checkpoint,source_through,source_chain_hash,source_checkpoint,complete
		FROM agent_service.history_replica_streams WHERE owner_id=$1 AND stream_id=$2 FOR UPDATE`,
		owner, page.StreamID).Scan(&stored.OwnerID, &stored.LogicalDialogID, &stored.NodeID, &stored.NodeDialogID,
		&stored.BindingGeneration, &importedThrough, &importedChain, &importedCheckpointRaw,
		&sourceThrough, &sourceChain, &storedSourceCheckpointRaw, &storedComplete); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica stream unavailable")
	}
	if stored != page.Identity {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	var importedCheckpoint, sourceCheckpoint model.HistoryCheckpoint
	if json.Unmarshal(importedCheckpointRaw, &importedCheckpoint) != nil ||
		json.Unmarshal(storedSourceCheckpointRaw, &sourceCheckpoint) != nil ||
		importedCheckpoint.ThroughSeq != importedThrough || importedCheckpoint.ChainHash != importedChain ||
		sourceCheckpoint.ThroughSeq != sourceThrough || sourceCheckpoint.ChainHash != sourceChain {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if page.Checkpoint.ThroughSeq == sourceThrough &&
		(page.Checkpoint.ChainHash != sourceChain || !sameHistorySourceBoundary(page.Checkpoint, sourceCheckpoint)) {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if page.Checkpoint.ThroughSeq < sourceThrough {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if page.Checkpoint.ThroughSeq < importedThrough || page.AfterSeq > importedThrough {
		return model.HistoryImportResult{}, ErrHistoryReplicaGap
	}
	duplicate := true
	for _, record := range page.Records {
		if record.StreamSeq <= importedThrough {
			var recordHash, chainHash string
			if err := tx.QueryRow(ctx, `SELECT record_hash,chain_hash FROM agent_service.history_replica_records
				WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=$3`, owner, page.StreamID, record.StreamSeq).Scan(&recordHash, &chainHash); err != nil {
				return model.HistoryImportResult{}, ErrHistoryReplicaConflict
			}
			if recordHash != record.RecordHash || chainHash != record.ChainHash {
				return model.HistoryImportResult{}, ErrHistoryReplicaConflict
			}
			continue
		}
		if record.StreamSeq != importedThrough+1 || record.PrevHash != importedChain {
			return model.HistoryImportResult{}, ErrHistoryReplicaGap
		}
		recordJSON, err := json.Marshal(record)
		if err != nil {
			return model.HistoryImportResult{}, ErrHistoryReplicaScope
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_replica_records(
			owner_id,stream_id,stream_seq,record_id,record_type,entity_id,revision,record_hash,prev_hash,chain_hash,record_json)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`, owner, page.StreamID, record.StreamSeq,
			record.RecordID, record.Type, record.EntityID, record.Revision, record.RecordHash, record.PrevHash,
			record.ChainHash, recordJSON); err != nil {
			return model.HistoryImportResult{}, errors.New("history replica record unavailable")
		}
		if err := applyHistoryEntity(ctx, tx, owner, page.StreamID, page.Identity, record); err != nil {
			return model.HistoryImportResult{}, err
		}
		foldHistoryCheckpoint(&importedCheckpoint, record)
		importedThrough, importedChain, duplicate = record.StreamSeq, record.ChainHash, false
	}
	if importedCheckpoint.ThroughSeq != importedThrough || importedCheckpoint.ChainHash != importedChain {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if importedThrough == page.Checkpoint.ThroughSeq &&
		(importedChain != page.Checkpoint.ChainHash || !sameHistoryCoverage(importedCheckpoint, page.Checkpoint)) {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if page.Checkpoint.ThroughSeq > sourceThrough {
		sourceCheckpoint = page.Checkpoint
		sourceThrough, sourceChain = sourceCheckpoint.ThroughSeq, sourceCheckpoint.ChainHash
	} else if page.Checkpoint.ThroughSeq == sourceThrough {
		sourceCheckpoint = page.Checkpoint
	}
	importedCheckpoint.DialogVersion = page.Checkpoint.DialogVersion
	importedCheckpoint.QueueRevision = page.Checkpoint.QueueRevision
	importedCheckpoint.CapturedAt = page.Checkpoint.CapturedAt
	if importedThrough == sourceThrough {
		if importedChain != sourceChain || !sameHistoryCoverage(importedCheckpoint, sourceCheckpoint) {
			return model.HistoryImportResult{}, ErrHistoryReplicaConflict
		}
		importedCheckpoint.Ready = sourceCheckpoint.Ready
		importedCheckpoint.IncompleteReason = sourceCheckpoint.IncompleteReason
		alreadyVerified := duplicate && storedComplete && importedThrough == page.Checkpoint.ThroughSeq
		if importedCheckpoint.Ready && !alreadyVerified {
			if err := verifyHistoryStreamCompleteness(ctx, tx, owner, page.StreamID); err != nil {
				return model.HistoryImportResult{}, err
			}
		}
	} else {
		importedCheckpoint.Ready = false
		importedCheckpoint.IncompleteReason = "replica_lag"
	}
	complete := importedCheckpoint.Ready && importedThrough == sourceThrough
	observedAt := s.now().UTC()
	importedCheckpointJSON, err := json.Marshal(importedCheckpoint)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	sourceCheckpointJSON, err = json.Marshal(sourceCheckpoint)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaScope
	}
	sourceCapturedAt, err := time.Parse(time.RFC3339Nano, sourceCheckpoint.CapturedAt)
	if err != nil {
		return model.HistoryImportResult{}, ErrHistoryReplicaConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_replica_streams SET
		imported_through=$3,imported_chain_hash=$4,imported_checkpoint=$5,
		source_through=$6,source_chain_hash=$7,source_checkpoint=$8,source_captured_at=$9,
		observed_at=$10,complete=$11 WHERE owner_id=$1 AND stream_id=$2`,
		owner, page.StreamID, importedThrough, importedChain, importedCheckpointJSON, sourceThrough,
		sourceChain, sourceCheckpointJSON, sourceCapturedAt, observedAt, complete); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica checkpoint unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return model.HistoryImportResult{}, errors.New("history replica commit unavailable")
	}
	return model.HistoryImportResult{
		SchemaID: model.HistoryExportSchemaID, StreamID: page.StreamID, ImportedThrough: importedThrough,
		SourceThrough: sourceThrough, Duplicate: duplicate, Complete: complete, ObservedAt: observedAt.Format(time.RFC3339Nano),
	}, nil
}

func emptyHistoryCheckpoint(streamID string, source model.HistoryCheckpoint) model.HistoryCheckpoint {
	result := model.HistoryCheckpoint{
		ChainHash: model.HistoryChainGenesis(streamID), DialogVersion: source.DialogVersion, QueueRevision: source.QueueRevision,
		EntriesHash: model.HistoryGenesisHash, FactsHash: model.HistoryGenesisHash,
		ReceiptsHash: model.HistoryGenesisHash, TextHash: model.HistoryGenesisHash,
		AssetManifestHash: model.HistoryGenesisHash, CapturedAt: source.CapturedAt,
	}
	if source.ThroughSeq == 0 && !source.Ready {
		result.IncompleteReason = source.IncompleteReason
	} else {
		result.IncompleteReason = "replica_lag"
	}
	return result
}

func sameHistoryCoverage(left, right model.HistoryCheckpoint) bool {
	return left.EntriesHash == right.EntriesHash && left.FactsHash == right.FactsHash &&
		left.ReceiptsHash == right.ReceiptsHash && left.TextHash == right.TextHash &&
		left.AssetManifestHash == right.AssetManifestHash && left.EffectsSummary == right.EffectsSummary
}

func sameHistorySourceBoundary(left, right model.HistoryCheckpoint) bool {
	return sameHistoryCoverage(left, right) && left.Ready == right.Ready &&
		left.IncompleteReason == right.IncompleteReason
}

func foldHistoryCheckpoint(checkpoint *model.HistoryCheckpoint, record model.HistoryRecord) {
	checkpoint.ThroughSeq = record.StreamSeq
	checkpoint.ChainHash = record.ChainHash
	switch record.Type {
	case model.HistoryRecordEntry:
		checkpoint.EntriesHash = model.HistoryFoldCoverageHash(checkpoint.EntriesHash, record.RecordHash)
		checkpoint.EffectsSummary.EntryCount++
	case model.HistoryRecordFact:
		checkpoint.FactsHash = model.HistoryFoldCoverageHash(checkpoint.FactsHash, record.RecordHash)
		checkpoint.EffectsSummary.FactCount++
	case model.HistoryRecordReceipt:
		checkpoint.ReceiptsHash = model.HistoryFoldCoverageHash(checkpoint.ReceiptsHash, record.RecordHash)
		checkpoint.EffectsSummary.ReceiptRevisionCount++
	case model.HistoryRecordTextManifest:
		checkpoint.TextHash = model.HistoryFoldCoverageHash(checkpoint.TextHash, record.RecordHash)
		checkpoint.EffectsSummary.TextManifestCount++
	case model.HistoryRecordTextChunk:
		checkpoint.TextHash = model.HistoryFoldCoverageHash(checkpoint.TextHash, record.RecordHash)
		checkpoint.EffectsSummary.TextChunkCount++
	case model.HistoryRecordAssetManifest:
		checkpoint.AssetManifestHash = model.HistoryFoldCoverageHash(checkpoint.AssetManifestHash, record.RecordHash)
		checkpoint.EffectsSummary.AssetManifestCount++
	}
}

func applyHistoryEntity(ctx context.Context, tx pgx.Tx, owner, streamID string, identity model.HistoryStreamIdentity, record model.HistoryRecord) error {
	payload, err := model.DecodeHistoryPayload(record)
	if err != nil {
		return ErrHistoryReplicaScope
	}
	sameHash := func(query string, args ...any) error {
		var stored string
		if err := tx.QueryRow(ctx, query, args...).Scan(&stored); err != nil || stored != record.RecordHash {
			return ErrHistoryReplicaConflict
		}
		return nil
	}
	switch value := payload.(type) {
	case *model.HistoryTranscriptEntry:
		if value.LogicalDialogID != identity.LogicalDialogID || value.EntryID != record.EntityID {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_entries(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,entry_id,entry_hash,origin_node_id,origin_node_dialog_id,message_id,
			request_id,attempt_id,role,created_at,execution_ordinal,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,NULLIF($10,'')::uuid,NULLIF($11,'')::uuid,$12,$13,$14,$15)
			ON CONFLICT(owner_id,logical_dialog_id,entry_id) DO NOTHING`, owner, identity.LogicalDialogID,
			streamID, record.StreamSeq, value.EntryID, record.RecordHash, value.Origin.NodeID, value.Origin.NodeDialogID, value.MessageID,
			value.RequestID, value.AttemptID, value.Role, value.CreatedAt, value.ExecutionOrdinal, record.Payload)
		if err != nil {
			return errors.New("history entry unavailable")
		}
		if tag.RowsAffected() == 0 {
			return sameHash(`SELECT entry_hash FROM agent_service.history_entries
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND entry_id=$3`, owner, identity.LogicalDialogID, value.EntryID)
		}
	case *model.HistoryExecutionFact:
		if value.LogicalDialogID != identity.LogicalDialogID || value.FactID != record.EntityID {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_execution_facts(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,fact_id,fact_hash,origin_node_id,origin_node_dialog_id,request_id,attempt_id,
			event_seq,kind,occurred_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')::uuid,NULLIF($10,'')::uuid,$11,$12,$13,$14)
			ON CONFLICT(owner_id,logical_dialog_id,fact_id) DO NOTHING`, owner, identity.LogicalDialogID,
			streamID, record.StreamSeq, value.FactID, record.RecordHash, value.Origin.NodeID, value.Origin.NodeDialogID, value.RequestID,
			value.AttemptID, value.EventSeq, value.Kind, value.OccurredAt, record.Payload)
		if err != nil {
			return errors.New("history execution fact unavailable")
		}
		if tag.RowsAffected() == 0 {
			return sameHash(`SELECT fact_hash FROM agent_service.history_execution_facts
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND fact_id=$3`, owner, identity.LogicalDialogID, value.FactID)
		}
	case *model.HistoryReceiptRevision:
		if value.LogicalDialogID != identity.LogicalDialogID || value.ReceiptRevisionID != record.EntityID ||
			value.Revision != record.Revision {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_receipt_revisions(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,receipt_revision_id,receipt_hash,origin_node_id,origin_node_dialog_id,
			command_id,command_kind,fingerprint,request_id,attempt_id,revision,predecessor_hash,accepted_at,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::uuid,NULLIF($13,'')::uuid,$14,$15,$16,$17)
			ON CONFLICT(owner_id,logical_dialog_id,receipt_revision_id) DO NOTHING`, owner, identity.LogicalDialogID,
			streamID, record.StreamSeq, value.ReceiptRevisionID, record.RecordHash, value.Origin.NodeID, value.Origin.NodeDialogID,
			value.CommandID, value.CommandKind, value.Fingerprint, value.RequestID, value.AttemptID,
			value.Revision, value.PredecessorHash, value.AcceptedAt, record.Payload)
		if err != nil {
			return errors.New("history receipt unavailable")
		}
		if tag.RowsAffected() == 0 {
			return sameHash(`SELECT receipt_hash FROM agent_service.history_receipt_revisions
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND receipt_revision_id=$3`, owner, identity.LogicalDialogID, value.ReceiptRevisionID)
		}
	case *model.HistoryTextManifest:
		if value.LogicalDialogID != identity.LogicalDialogID || value.TextID != record.EntityID {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_text_manifests(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,text_id,manifest_hash,attempt_id,complete,size_bytes,text_sha256,chunk_count,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
			ON CONFLICT(owner_id,logical_dialog_id,text_id) DO NOTHING`, owner, identity.LogicalDialogID,
			streamID, record.StreamSeq, value.TextID, record.RecordHash, value.AttemptID, value.Complete, value.SizeBytes, value.SHA256,
			value.ChunkCount, record.Payload)
		if err != nil {
			return errors.New("history text manifest unavailable")
		}
		if tag.RowsAffected() == 0 {
			return sameHash(`SELECT manifest_hash FROM agent_service.history_text_manifests
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, identity.LogicalDialogID, value.TextID)
		}
	case *model.HistoryTextChunk:
		if value.LogicalDialogID != identity.LogicalDialogID || record.EntityID != model.HistoryStableUUID(
			"history-text-chunk-v1", value.TextID+"\x00"+fmt.Sprintf("%d", value.OffsetBytes)) {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_text_chunks(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,text_id,chunk_index,offset_bytes,size_bytes,chunk_sha256,content)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
			ON CONFLICT(owner_id,logical_dialog_id,text_id,chunk_index) DO NOTHING`, owner, identity.LogicalDialogID,
			streamID, record.StreamSeq, value.TextID, value.ChunkIndex, value.OffsetBytes, value.SizeBytes, value.SHA256, value.Bytes)
		if err != nil {
			return errors.New("history text chunk unavailable")
		}
		if tag.RowsAffected() == 0 {
			var offset, size int64
			var hash string
			var content []byte
			if err := tx.QueryRow(ctx, `SELECT offset_bytes,size_bytes,chunk_sha256,content FROM agent_service.history_text_chunks
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3 AND chunk_index=$4`,
				owner, identity.LogicalDialogID, value.TextID, value.ChunkIndex).Scan(&offset, &size, &hash, &content); err != nil ||
				offset != value.OffsetBytes || size != value.SizeBytes || hash != value.SHA256 || string(content) != string(value.Bytes) {
				return ErrHistoryReplicaConflict
			}
		}
	case *model.HistoryAssetManifest:
		if value.LogicalDialogID != identity.LogicalDialogID {
			return ErrHistoryReplicaScope
		}
		tag, err := tx.Exec(ctx, `INSERT INTO agent_service.history_asset_manifests(
			owner_id,logical_dialog_id,source_stream_id,source_stream_seq,manifest_entity_id,revision,manifest_hash,payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT(owner_id,logical_dialog_id,manifest_entity_id,revision) DO NOTHING`, owner,
			identity.LogicalDialogID, streamID, record.StreamSeq, record.EntityID, record.Revision, record.RecordHash, record.Payload)
		if err != nil {
			return errors.New("history asset manifest unavailable")
		}
		if tag.RowsAffected() == 0 {
			return sameHash(`SELECT manifest_hash FROM agent_service.history_asset_manifests
				WHERE owner_id=$1 AND logical_dialog_id=$2 AND manifest_entity_id=$3 AND revision=$4`,
				owner, identity.LogicalDialogID, record.EntityID, record.Revision)
		}
	default:
		return ErrHistoryReplicaScope
	}
	return nil
}

type historyTextProof struct {
	manifest *model.HistoryTextManifest
	chunks   []model.HistoryTextChunk
}

type historyReceiptProof struct {
	revision model.HistoryReceiptRevision
	hash     string
}

// verifyHistoryStreamCompleteness is the final Ready gate. The source
// checkpoint proves record coverage; this second pass proves the relational
// invariants that cannot be expressed by a single record hash.
func verifyHistoryStreamCompleteness(ctx context.Context, tx pgx.Tx, owner, streamID string) error {
	rows, err := tx.Query(ctx, `SELECT record_json FROM agent_service.history_replica_records
		WHERE owner_id=$1 AND stream_id=$2 ORDER BY stream_seq`, owner, streamID)
	if err != nil {
		return errors.New("history replica completeness unavailable")
	}
	defer rows.Close()
	texts := map[string]*historyTextProof{}
	receipts := map[string][]historyReceiptProof{}
	assetManifests := 0
	for rows.Next() {
		var raw []byte
		var record model.HistoryRecord
		if rows.Scan(&raw) != nil || json.Unmarshal(raw, &record) != nil {
			return ErrHistoryReplicaConflict
		}
		payload, err := model.DecodeHistoryPayload(record)
		if err != nil {
			return ErrHistoryReplicaConflict
		}
		switch value := payload.(type) {
		case *model.HistoryTextManifest:
			proof := texts[value.TextID]
			if proof == nil {
				proof = &historyTextProof{}
				texts[value.TextID] = proof
			}
			if proof.manifest != nil {
				return ErrHistoryReplicaConflict
			}
			copyValue := *value
			proof.manifest = &copyValue
		case *model.HistoryTextChunk:
			proof := texts[value.TextID]
			if proof == nil {
				proof = &historyTextProof{}
				texts[value.TextID] = proof
			}
			proof.chunks = append(proof.chunks, *value)
		case *model.HistoryReceiptRevision:
			key := value.Origin.NodeID + "\x00" + value.CommandID
			receipts[key] = append(receipts[key], historyReceiptProof{revision: *value, hash: record.RecordHash})
		case *model.HistoryAssetManifest:
			assetManifests++
		}
	}
	if rows.Err() != nil || assetManifests == 0 {
		return ErrHistoryReplicaConflict
	}
	for _, proof := range texts {
		if proof.manifest == nil {
			return ErrHistoryReplicaConflict
		}
		manifest := proof.manifest
		if !manifest.Complete {
			return ErrHistoryReplicaConflict
		}
		if int64(len(proof.chunks)) != manifest.ChunkCount {
			return ErrHistoryReplicaConflict
		}
		sort.Slice(proof.chunks, func(i, j int) bool { return proof.chunks[i].ChunkIndex < proof.chunks[j].ChunkIndex })
		hasher := sha256.New()
		var offset int64
		for index, chunk := range proof.chunks {
			if chunk.ChunkIndex != int64(index) || chunk.OffsetBytes != offset || chunk.SizeBytes != int64(len(chunk.Bytes)) ||
				model.HistoryHashBytes(chunk.Bytes) != chunk.SHA256 {
				return ErrHistoryReplicaConflict
			}
			_, _ = hasher.Write(chunk.Bytes)
			offset += chunk.SizeBytes
		}
		if offset != manifest.SizeBytes || hex.EncodeToString(hasher.Sum(nil)) != manifest.SHA256 {
			return ErrHistoryReplicaConflict
		}
	}
	for _, chain := range receipts {
		sort.Slice(chain, func(i, j int) bool { return chain[i].revision.Revision < chain[j].revision.Revision })
		previous := model.HistoryGenesisHash
		for index, item := range chain {
			if item.revision.Revision != int64(index+1) || item.revision.PredecessorHash != previous {
				return ErrHistoryReplicaConflict
			}
			previous = item.hash
		}
	}
	return nil
}

func (s *Store) ReadHistoryReplica(ctx context.Context, owner, logicalDialogID string, position HistoryReadPosition, limit int) (HistoryReadResult, error) {
	if owner == "" || !model.ValidUUID(logicalDialogID) || limit < 1 || limit > model.HistoryMaximumPageSize {
		return HistoryReadResult{}, ErrHistoryReplicaScope
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return HistoryReadResult{}, errors.New("history read transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var visible int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM agent_service.logical_dialogs
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND deleted_at IS NULL`, owner, logicalDialogID).Scan(&visible); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HistoryReadResult{}, pgx.ErrNoRows
		}
		return HistoryReadResult{}, errors.New("history dialog unavailable")
	}
	if len(position.Horizons) == 0 {
		position, err = s.captureHistoryReadPosition(ctx, tx, owner, logicalDialogID)
	} else {
		err = validateHistoryReadPosition(ctx, tx, owner, logicalDialogID, position)
	}
	if err != nil {
		return HistoryReadResult{}, err
	}
	pageStart := position
	horizons, err := json.Marshal(position.Horizons)
	if err != nil {
		return HistoryReadResult{}, ErrHistoryReplicaScope
	}
	entries, entryMore, entryKeysAt, entryKeysID, err := readHistoryEntries(ctx, tx, owner, logicalDialogID, horizons,
		position.EntryCreatedAt, position.EntryID, limit)
	if err != nil {
		return HistoryReadResult{}, err
	}
	facts, factMore, factKeysAt, factKeysID, err := readHistoryFacts(ctx, tx, owner, logicalDialogID, horizons,
		position.FactOccurredAt, position.FactID, limit)
	if err != nil {
		return HistoryReadResult{}, err
	}
	receipts, receiptMore, receiptKeysAt, receiptKeysID, err := readHistoryReceipts(ctx, tx, owner, logicalDialogID, horizons,
		position.ReceiptAcceptedAt, position.ReceiptRevisionID, limit)
	if err != nil {
		return HistoryReadResult{}, err
	}
	sourceTime, _ := time.Parse(time.RFC3339Nano, position.SourceCapturedAt)
	lag := s.now().UTC().Sub(sourceTime).Milliseconds()
	if lag < 0 {
		lag = 0
	}
	page := model.HistoryReadPage{
		SchemaID: model.HistoryReadSchemaID, LogicalDialogID: logicalDialogID, Entries: entries, Facts: facts,
		Receipts: receipts, SyncedThrough: position.Checkpoint, StreamCoverage: historyStreamCoverage(position.Horizons), ObservedAt: position.ObservedAt,
		LagMillis: lag, Incomplete: !position.Complete,
	}
	trimmed, err := trimHistoryReadPage(&page)
	if err != nil {
		return HistoryReadResult{}, err
	}
	position.EntryCreatedAt, position.EntryID = pageStart.EntryCreatedAt, pageStart.EntryID
	position.FactOccurredAt, position.FactID = pageStart.FactOccurredAt, pageStart.FactID
	position.ReceiptAcceptedAt, position.ReceiptRevisionID = pageStart.ReceiptAcceptedAt, pageStart.ReceiptRevisionID
	if len(page.Entries) > 0 {
		last := len(page.Entries) - 1
		position.EntryCreatedAt, position.EntryID = entryKeysAt[last], entryKeysID[last]
	}
	if len(page.Facts) > 0 {
		last := len(page.Facts) - 1
		position.FactOccurredAt, position.FactID = factKeysAt[last], factKeysID[last]
	}
	if len(page.Receipts) > 0 {
		last := len(page.Receipts) - 1
		position.ReceiptAcceptedAt, position.ReceiptRevisionID = receiptKeysAt[last], receiptKeysID[last]
	}
	result := HistoryReadResult{Page: page, HasMore: entryMore || factMore || receiptMore || trimmed, Position: position}
	if err := tx.Commit(ctx); err != nil {
		return HistoryReadResult{}, errors.New("history read commit unavailable")
	}
	return result, nil
}

func trimHistoryReadPage(page *model.HistoryReadPage) (bool, error) {
	raw, err := json.Marshal(page)
	if err != nil {
		return false, errors.New("history read page is invalid")
	}
	size := len(raw)
	trimmed := false
	for size > historyReadDataBudget {
		entryCost, factCost, receiptCost := -1, -1, -1
		if count := len(page.Entries); count > 0 {
			encoded, marshalErr := json.Marshal(page.Entries[count-1])
			if marshalErr != nil {
				return false, errors.New("history read page is invalid")
			}
			entryCost = len(encoded)
			if count > 1 {
				entryCost++
			}
		}
		if count := len(page.Facts); count > 0 {
			encoded, marshalErr := json.Marshal(page.Facts[count-1])
			if marshalErr != nil {
				return false, errors.New("history read page is invalid")
			}
			factCost = len(encoded)
			if count > 1 {
				factCost++
			}
		}
		if count := len(page.Receipts); count > 0 {
			encoded, marshalErr := json.Marshal(page.Receipts[count-1])
			if marshalErr != nil {
				return false, errors.New("history read page is invalid")
			}
			receiptCost = len(encoded)
			if count > 1 {
				receiptCost++
			}
		}
		switch {
		case entryCost >= factCost && entryCost >= receiptCost && entryCost >= 0:
			page.Entries = page.Entries[:len(page.Entries)-1]
			size -= entryCost
		case factCost >= receiptCost && factCost >= 0:
			page.Facts = page.Facts[:len(page.Facts)-1]
			size -= factCost
		case receiptCost >= 0:
			page.Receipts = page.Receipts[:len(page.Receipts)-1]
			size -= receiptCost
		default:
			return false, errors.New("history read metadata exceeds byte bound")
		}
		trimmed = true
	}
	raw, err = json.Marshal(page)
	if err != nil || len(raw) > historyReadDataBudget {
		return false, errors.New("history read page exceeds byte bound")
	}
	return trimmed, nil
}

func (s *Store) captureHistoryReadPosition(ctx context.Context, tx pgx.Tx, owner, logicalDialogID string) (HistoryReadPosition, error) {
	rows, err := tx.Query(ctx, `SELECT stream_id::text,imported_through,imported_chain_hash,imported_checkpoint,
		observed_at,source_captured_at,complete FROM agent_service.history_replica_streams
		WHERE owner_id=$1 AND logical_dialog_id=$2
		ORDER BY binding_generation DESC,observed_at DESC,stream_id DESC`, owner, logicalDialogID)
	if err != nil {
		return HistoryReadPosition{}, errors.New("history checkpoint unavailable")
	}
	defer rows.Close()
	result := HistoryReadPosition{Complete: true}
	first := true
	for rows.Next() {
		var horizon HistoryReadHorizon
		var checkpointRaw []byte
		var observedAt, sourceAt time.Time
		var complete bool
		if rows.Scan(&horizon.StreamID, &horizon.ThroughSeq, &horizon.ChainHash, &checkpointRaw,
			&observedAt, &sourceAt, &complete) != nil {
			return HistoryReadPosition{}, errors.New("history checkpoint unavailable")
		}
		result.Horizons = append(result.Horizons, horizon)
		result.Horizons[len(result.Horizons)-1].Complete = complete
		result.Horizons[len(result.Horizons)-1].ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
		result.Complete = result.Complete && complete
		if first {
			if json.Unmarshal(checkpointRaw, &result.Checkpoint) != nil {
				return HistoryReadPosition{}, errors.New("history checkpoint unavailable")
			}
			result.ObservedAt = observedAt.UTC().Format(time.RFC3339Nano)
			result.SourceCapturedAt = sourceAt.UTC().Format(time.RFC3339Nano)
			if age := s.now().UTC().Sub(observedAt.UTC()); age > HistoryReplicaFreshnessWindow {
				result.Complete = false
				result.Horizons[len(result.Horizons)-1].Complete = false
				result.Checkpoint.Ready = false
				result.Checkpoint.IncompleteReason = "source_unreachable"
			}
			first = false
		}
	}
	if len(result.Horizons) > historyMaximumReadStreams {
		return HistoryReadPosition{}, errors.New("history stream coverage exceeds cursor bound")
	}
	if rows.Err() != nil {
		return HistoryReadPosition{}, errors.New("history checkpoint unavailable")
	}
	if first {
		return HistoryReadPosition{}, pgx.ErrNoRows
	}
	if !result.Complete {
		result.Checkpoint.Ready = false
		if result.Checkpoint.IncompleteReason == "" {
			result.Checkpoint.IncompleteReason = "replica_lag"
		}
	}
	return result, nil
}

func validateHistoryReadPosition(ctx context.Context, tx pgx.Tx, owner, logicalDialogID string, position HistoryReadPosition) error {
	if len(position.Horizons) == 0 || len(position.Horizons) > historyMaximumReadStreams || !validHistoryReadKey(position.EntryCreatedAt, position.EntryID) ||
		!validHistoryReadKey(position.FactOccurredAt, position.FactID) ||
		!validHistoryReadKey(position.ReceiptAcceptedAt, position.ReceiptRevisionID) {
		return ErrHistoryReplicaScope
	}
	if _, err := time.Parse(time.RFC3339Nano, position.ObservedAt); err != nil {
		return ErrHistoryReplicaScope
	}
	if _, err := time.Parse(time.RFC3339Nano, position.SourceCapturedAt); err != nil {
		return ErrHistoryReplicaScope
	}
	seen := map[string]bool{}
	checkpointBound := false
	for _, horizon := range position.Horizons {
		if !model.ValidUUID(horizon.StreamID) || horizon.ThroughSeq < 0 || horizon.ThroughSeq > model.HistoryMaximumSafeInt ||
			!model.ValidSHA256(horizon.ChainHash) || seen[horizon.StreamID] {
			return ErrHistoryReplicaScope
		}
		if _, err := time.Parse(time.RFC3339Nano, horizon.ObservedAt); err != nil {
			return ErrHistoryReplicaScope
		}
		seen[horizon.StreamID] = true
		var current int64
		if err := tx.QueryRow(ctx, `SELECT imported_through FROM agent_service.history_replica_streams
			WHERE owner_id=$1 AND logical_dialog_id=$2 AND stream_id=$3`, owner, logicalDialogID, horizon.StreamID).Scan(&current); err != nil || current < horizon.ThroughSeq {
			return ErrHistoryReplicaConflict
		}
		chain := model.HistoryChainGenesis(horizon.StreamID)
		if horizon.ThroughSeq > 0 {
			if err := tx.QueryRow(ctx, `SELECT chain_hash FROM agent_service.history_replica_records
				WHERE owner_id=$1 AND stream_id=$2 AND stream_seq=$3`, owner, horizon.StreamID, horizon.ThroughSeq).Scan(&chain); err != nil {
				return ErrHistoryReplicaConflict
			}
		}
		if chain != horizon.ChainHash {
			return ErrHistoryReplicaConflict
		}
		if horizon.ThroughSeq == position.Checkpoint.ThroughSeq && horizon.ChainHash == position.Checkpoint.ChainHash {
			checkpointBound = true
		}
	}
	if !checkpointBound {
		return ErrHistoryReplicaScope
	}
	return nil
}

func historyStreamCoverage(horizons []HistoryReadHorizon) []model.HistoryStreamCoverage {
	result := make([]model.HistoryStreamCoverage, 0, len(horizons))
	for _, horizon := range horizons {
		result = append(result, model.HistoryStreamCoverage{
			StreamID: horizon.StreamID, ThroughSeq: horizon.ThroughSeq, ChainHash: horizon.ChainHash,
			Complete: horizon.Complete, ObservedAt: horizon.ObservedAt,
		})
	}
	return result
}

func validHistoryReadKey(at, id string) bool {
	if at == "" || id == "" {
		return at == "" && id == ""
	}
	_, err := time.Parse(time.RFC3339Nano, at)
	return err == nil && model.ValidUUID(id)
}

func readHistoryEntries(ctx context.Context, tx pgx.Tx, owner, logicalDialogID string, horizons []byte,
	afterAt, afterID string, limit int) ([]model.HistoryTranscriptEntry, bool, []string, []string, error) {
	rows, err := tx.Query(ctx, `SELECT payload,created_at,entry_id::text FROM agent_service.history_entries e
		WHERE owner_id=$1 AND logical_dialog_id=$2
		AND EXISTS (SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS h("streamId" uuid,"throughSeq" bigint,"chainHash" text)
			WHERE h."streamId"=e.source_stream_id AND e.source_stream_seq<=h."throughSeq")
		AND (NULLIF($4,'') IS NULL OR (created_at,entry_id)>(NULLIF($4,'')::timestamptz,NULLIF($5,'')::uuid))
		ORDER BY created_at,entry_id LIMIT $6`, owner, logicalDialogID, horizons, afterAt, afterID, limit+1)
	if err != nil {
		return nil, false, nil, nil, errors.New("history entries unavailable")
	}
	defer rows.Close()
	result := make([]model.HistoryTranscriptEntry, 0, limit+1)
	keysAt, keysID := make([]string, 0, limit+1), make([]string, 0, limit+1)
	for rows.Next() {
		var raw []byte
		var item model.HistoryTranscriptEntry
		var at time.Time
		var id string
		if rows.Scan(&raw, &at, &id) != nil || json.Unmarshal(raw, &item) != nil {
			return nil, false, nil, nil, errors.New("history entry is invalid")
		}
		result = append(result, item)
		keysAt, keysID = append(keysAt, at.UTC().Format(time.RFC3339Nano)), append(keysID, id)
	}
	if rows.Err() != nil {
		return nil, false, nil, nil, errors.New("history entries unavailable")
	}
	return trimHistoryPage(result, keysAt, keysID, limit)
}

func readHistoryFacts(ctx context.Context, tx pgx.Tx, owner, logicalDialogID string, horizons []byte,
	afterAt, afterID string, limit int) ([]model.HistoryExecutionFact, bool, []string, []string, error) {
	rows, err := tx.Query(ctx, `SELECT payload,occurred_at,fact_id::text FROM agent_service.history_execution_facts f
		WHERE owner_id=$1 AND logical_dialog_id=$2
		AND EXISTS (SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS h("streamId" uuid,"throughSeq" bigint,"chainHash" text)
			WHERE h."streamId"=f.source_stream_id AND f.source_stream_seq<=h."throughSeq")
		AND (NULLIF($4,'') IS NULL OR (occurred_at,fact_id)>(NULLIF($4,'')::timestamptz,NULLIF($5,'')::uuid))
		ORDER BY occurred_at,fact_id LIMIT $6`, owner, logicalDialogID, horizons, afterAt, afterID, limit+1)
	if err != nil {
		return nil, false, nil, nil, errors.New("history facts unavailable")
	}
	defer rows.Close()
	result := make([]model.HistoryExecutionFact, 0, limit+1)
	keysAt, keysID := make([]string, 0, limit+1), make([]string, 0, limit+1)
	for rows.Next() {
		var raw []byte
		var item model.HistoryExecutionFact
		var at time.Time
		var id string
		if rows.Scan(&raw, &at, &id) != nil || json.Unmarshal(raw, &item) != nil {
			return nil, false, nil, nil, errors.New("history fact is invalid")
		}
		result = append(result, item)
		keysAt, keysID = append(keysAt, at.UTC().Format(time.RFC3339Nano)), append(keysID, id)
	}
	if rows.Err() != nil {
		return nil, false, nil, nil, errors.New("history facts unavailable")
	}
	return trimHistoryPage(result, keysAt, keysID, limit)
}

func readHistoryReceipts(ctx context.Context, tx pgx.Tx, owner, logicalDialogID string, horizons []byte,
	afterAt, afterID string, limit int) ([]model.HistoryReceiptRevision, bool, []string, []string, error) {
	rows, err := tx.Query(ctx, `SELECT payload,accepted_at,receipt_revision_id::text FROM agent_service.history_receipt_revisions r
		WHERE owner_id=$1 AND logical_dialog_id=$2
		AND EXISTS (SELECT 1 FROM jsonb_to_recordset($3::jsonb) AS h("streamId" uuid,"throughSeq" bigint,"chainHash" text)
			WHERE h."streamId"=r.source_stream_id AND r.source_stream_seq<=h."throughSeq")
		AND (NULLIF($4,'') IS NULL OR (accepted_at,receipt_revision_id)>(NULLIF($4,'')::timestamptz,NULLIF($5,'')::uuid))
		ORDER BY accepted_at,receipt_revision_id LIMIT $6`, owner, logicalDialogID, horizons, afterAt, afterID, limit+1)
	if err != nil {
		return nil, false, nil, nil, errors.New("history receipts unavailable")
	}
	defer rows.Close()
	result := make([]model.HistoryReceiptRevision, 0, limit+1)
	keysAt, keysID := make([]string, 0, limit+1), make([]string, 0, limit+1)
	for rows.Next() {
		var raw []byte
		var item model.HistoryReceiptRevision
		var at time.Time
		var id string
		if rows.Scan(&raw, &at, &id) != nil || json.Unmarshal(raw, &item) != nil {
			return nil, false, nil, nil, errors.New("history receipt is invalid")
		}
		result = append(result, item)
		keysAt, keysID = append(keysAt, at.UTC().Format(time.RFC3339Nano)), append(keysID, id)
	}
	if rows.Err() != nil {
		return nil, false, nil, nil, errors.New("history receipts unavailable")
	}
	return trimHistoryPage(result, keysAt, keysID, limit)
}

func trimHistoryPage[T any](items []T, keysAt, keysID []string, limit int) ([]T, bool, []string, []string, error) {
	more := len(items) > limit
	if more {
		items, keysAt, keysID = items[:limit], keysAt[:limit], keysID[:limit]
	}
	return items, more, keysAt, keysID, nil
}

func (s *Store) HistoryReceiptByOrigin(ctx context.Context, owner, nodeID, commandID string) (model.HistoryReceiptLookup, error) {
	if owner == "" || !model.ValidUUID(nodeID) || !model.ValidUUID(commandID) {
		return model.HistoryReceiptLookup{}, ErrHistoryReplicaScope
	}
	var receiptRaw, checkpointRaw []byte
	err := s.pool.QueryRow(ctx, `WITH receipt AS (
		SELECT r.source_stream_id,r.payload FROM agent_service.history_receipt_revisions r
		JOIN agent_service.logical_dialogs d ON d.owner_id=r.owner_id AND d.logical_dialog_id=r.logical_dialog_id
		WHERE r.owner_id=$1 AND r.origin_node_id=$2 AND r.command_id=$3 AND d.deleted_at IS NULL
		ORDER BY r.revision DESC LIMIT 1
	), checkpoint AS (
		SELECT imported_checkpoint FROM agent_service.history_replica_streams s
		JOIN receipt r ON r.source_stream_id=s.stream_id
		WHERE s.owner_id=$1 ORDER BY s.observed_at DESC,s.stream_id DESC LIMIT 1
	)
	SELECT receipt.payload,checkpoint.imported_checkpoint FROM receipt CROSS JOIN checkpoint`,
		owner, nodeID, commandID).Scan(&receiptRaw, &checkpointRaw)
	if err != nil {
		return model.HistoryReceiptLookup{}, err
	}
	result := model.HistoryReceiptLookup{SchemaID: model.HistoryReceiptSchemaID}
	if json.Unmarshal(receiptRaw, &result.Receipt) != nil || json.Unmarshal(checkpointRaw, &result.Checkpoint) != nil {
		return model.HistoryReceiptLookup{}, errors.New("history receipt unavailable")
	}
	return result, nil
}

func (s *Store) HistoryTextManifest(ctx context.Context, owner, logicalDialogID, textID string) (model.HistoryTextManifest, error) {
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT m.payload FROM agent_service.history_text_manifests m
		JOIN agent_service.logical_dialogs d ON d.owner_id=m.owner_id AND d.logical_dialog_id=m.logical_dialog_id
		WHERE m.owner_id=$1 AND m.logical_dialog_id=$2 AND m.text_id=$3 AND d.deleted_at IS NULL`,
		owner, logicalDialogID, textID).Scan(&raw); err != nil {
		return model.HistoryTextManifest{}, err
	}
	var result model.HistoryTextManifest
	if json.Unmarshal(raw, &result) != nil {
		return model.HistoryTextManifest{}, errors.New("history text manifest unavailable")
	}
	return result, nil
}

func (s *Store) HistoryTextChunk(ctx context.Context, owner, logicalDialogID, textID string, index int64) (model.HistoryTextChunk, error) {
	var result model.HistoryTextChunk
	result.LogicalDialogID, result.TextID, result.ChunkIndex = logicalDialogID, textID, index
	if err := s.pool.QueryRow(ctx, `SELECT c.offset_bytes,c.size_bytes,c.chunk_sha256,c.content
		FROM agent_service.history_text_chunks c
		JOIN agent_service.logical_dialogs d ON d.owner_id=c.owner_id AND d.logical_dialog_id=c.logical_dialog_id
		WHERE c.owner_id=$1 AND c.logical_dialog_id=$2 AND c.text_id=$3 AND c.chunk_index=$4 AND d.deleted_at IS NULL`,
		owner, logicalDialogID, textID, index).Scan(&result.OffsetBytes, &result.SizeBytes, &result.SHA256, &result.Bytes); err != nil {
		return model.HistoryTextChunk{}, err
	}
	return result, nil
}
