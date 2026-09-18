package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

var (
	ErrHistorySearchScope   = errors.New("history search scope mismatch")
	ErrHistorySearchExpired = errors.New("history search snapshot expired")
	searchSpacePattern      = regexp.MustCompile(`\s+`)
)

type HistorySearchSpec struct {
	Query    model.HistorySearchQuery
	Words    string
	RankText string
	Terms    []string
	Phrases  []string
}

type HistorySearchPosition struct {
	SnapshotID string `json:"snapshotId"`
	Offset     int64  `json:"offset"`
	QueryHash  string `json:"queryHash"`
	ExpiresAt  string `json:"expiresAt"`
}

type HistorySearchResult struct {
	Page     model.HistorySearchPage
	HasMore  bool
	Position HistorySearchPosition
}

func ParseHistorySearchQuery(query model.HistorySearchQuery) (HistorySearchSpec, error) {
	if query.Q == "" || len([]byte(query.Q)) > model.HistorySearchMaximumQuery || !utf8.ValidString(query.Q) ||
		strings.ContainsRune(query.Q, '\x00') || (query.NodeID != "" && !model.ValidUUID(query.NodeID)) ||
		(query.LogicalDialogID != "" && !model.ValidUUID(query.LogicalDialogID)) {
		return HistorySearchSpec{}, ErrHistorySearchScope
	}
	var words []string
	var phrases []string
	var current strings.Builder
	quoted := false
	flush := func(phrase bool) bool {
		value := normalizeSearchText(current.String())
		current.Reset()
		if value == "" {
			return false
		}
		if phrase {
			phrases = append(phrases, value)
		} else {
			words = append(words, strings.Fields(value)...)
		}
		return true
	}
	for _, r := range query.Q {
		switch {
		case r == '"':
			if quoted && !flush(true) {
				return HistorySearchSpec{}, ErrHistorySearchScope
			}
			if !quoted {
				flush(false)
			}
			quoted = !quoted
		case !quoted && (r == ' ' || r == '\t' || r == '\r' || r == '\n'):
			flush(false)
		default:
			current.WriteRune(r)
		}
	}
	if quoted {
		return HistorySearchSpec{}, ErrHistorySearchScope
	}
	flush(false)
	if len(words) == 0 && len(phrases) == 0 {
		return HistorySearchSpec{}, ErrHistorySearchScope
	}
	query.Q = normalizeSearchText(query.Q)
	rankParts := append(append([]string(nil), words...), phrases...)
	return HistorySearchSpec{Query: query, Words: strings.Join(words, " "), RankText: strings.Join(rankParts, " "), Terms: words, Phrases: phrases}, nil
}

func HistorySearchQueryHash(spec HistorySearchSpec) string {
	raw, _ := json.Marshal(struct {
		Query    model.HistorySearchQuery `json:"query"`
		Words    string                   `json:"words"`
		RankText string                   `json:"rankText"`
		Terms    []string                 `json:"terms"`
		Phrases  []string                 `json:"phrases"`
	}{spec.Query, spec.Words, spec.RankText, spec.Terms, spec.Phrases})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func normalizeSearchText(value string) string {
	return strings.TrimSpace(searchSpacePattern.ReplaceAllString(strings.ToLower(value), " "))
}

func indexHistoryEntry(ctx context.Context, tx pgx.Tx, owner string, identity model.HistoryStreamIdentity, entry model.HistoryTranscriptEntry) error {
	if model.ValidateHistorySearchEntry(entry) != nil {
		// R12 intentionally stores opaque content objects. R14 projects only the
		// narrower safe contract and must never reject the underlying replica.
		return nil
	}
	message := model.HistorySearchInlineText(entry)
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_search_documents(
		owner_id,logical_dialog_id,entry_id,node_id,node_dialog_id,role,kind,created_at,message_text)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(owner_id,logical_dialog_id,entry_id) DO UPDATE SET
		message_text=EXCLUDED.message_text WHERE agent_service.history_search_documents.message_text=EXCLUDED.message_text`,
		owner, identity.LogicalDialogID, entry.EntryID, identity.NodeID, identity.NodeDialogID,
		entry.Role, entry.Kind, entry.CreatedAt, message); err != nil {
		return errors.New("history search document unavailable")
	}
	// Only the newest deterministic entry carries title weight A. This makes a
	// title-only query produce one deep-link target per dialog.
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_search_documents SET title_text=''
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND title_text<>''`, owner, identity.LogicalDialogID); err != nil {
		return errors.New("history search title unavailable")
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_search_documents d SET title_text=m.title
		FROM agent_service.history_dialog_metadata m WHERE d.owner_id=$1 AND d.logical_dialog_id=$2
			AND m.owner_id=d.owner_id AND m.logical_dialog_id=d.logical_dialog_id AND d.entry_id=(
				SELECT entry_id FROM agent_service.history_search_documents
				WHERE owner_id=$1 AND logical_dialog_id=$2 ORDER BY created_at DESC,entry_id DESC LIMIT 1)`,
		owner, identity.LogicalDialogID); err != nil {
		return errors.New("history search title unavailable")
	}
	return nil
}

const (
	historySearchToolSegmentMaximum = 256 * 1024
	historySearchToolSegmentOverlap = model.HistorySearchMaximumQuery + 2*utf8.UTFMax
)

// indexHistorySafeTextsForDialog runs only after the R12 Ready proof has
// validated every chunk, offset, size and aggregate digest in the stream. A
// stream's first Ready transition repairs unresolved projections once; later
// Ready tails are bounded to attempts/text IDs changed by that page.
func indexHistorySafeTextsForDialog(
	ctx context.Context,
	tx pgx.Tx,
	owner, logicalDialogID string,
	includeUnresolved bool,
	attemptIDs, textIDs []string,
) error {
	if !includeUnresolved && len(attemptIDs) == 0 && len(textIDs) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `WITH candidates AS MATERIALIZED (
		SELECT m.*
		FROM agent_service.history_text_manifests m
		JOIN agent_service.history_replica_streams r ON r.owner_id=m.owner_id AND r.stream_id=m.source_stream_id AND r.complete
		WHERE m.owner_id=$1 AND m.logical_dialog_id=$2 AND m.complete
			AND ($3 OR m.attempt_id=ANY($4::uuid[]) OR m.text_id=ANY($5::uuid[]))
	)
	SELECT m.text_id::text
	FROM candidates m
	LEFT JOIN agent_service.history_search_tool_sources s ON s.owner_id=m.owner_id
		AND s.logical_dialog_id=m.logical_dialog_id AND s.text_id=m.text_id
	LEFT JOIN LATERAL (
		SELECT e.entry_id
		FROM agent_service.history_entries e
		JOIN agent_service.history_search_documents d ON d.owner_id=e.owner_id
			AND d.logical_dialog_id=e.logical_dialog_id AND d.entry_id=e.entry_id
		WHERE e.owner_id=m.owner_id AND e.logical_dialog_id=m.logical_dialog_id AND e.attempt_id=m.attempt_id
		ORDER BY e.created_at DESC,e.entry_id DESC LIMIT 1
	) target ON true
	WHERE ($3 AND (s.text_id IS NULL OR s.size_bytes<>m.size_bytes OR s.text_sha256<>m.text_sha256
		OR s.entry_id IS DISTINCT FROM target.entry_id))
		OR m.attempt_id=ANY($4::uuid[]) OR m.text_id=ANY($5::uuid[])
	ORDER BY m.text_id`, owner, logicalDialogID, includeUnresolved, attemptIDs, textIDs)
	if err != nil {
		return errors.New("history search text manifests unavailable")
	}
	var targets []string
	for rows.Next() {
		var textID string
		if rows.Scan(&textID) != nil {
			rows.Close()
			return errors.New("history search text manifests unavailable")
		}
		targets = append(targets, textID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return errors.New("history search text manifests unavailable")
	}
	rows.Close()
	for _, textID := range targets {
		if err := indexHistorySafeText(ctx, tx, owner, logicalDialogID, textID); err != nil {
			return err
		}
	}
	return nil
}

func indexHistorySafeText(ctx context.Context, tx pgx.Tx, owner, logicalDialogID, textID string) error {
	var attemptID, expectedSHA string
	var complete bool
	var expectedCount, expectedSize int64
	if err := tx.QueryRow(ctx, `SELECT attempt_id::text,complete,chunk_count,size_bytes,text_sha256
		FROM agent_service.history_text_manifests
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, logicalDialogID, textID).
		Scan(&attemptID, &complete, &expectedCount, &expectedSize, &expectedSHA); err != nil {
		return errors.New("history search text manifest unavailable")
	}
	if !complete {
		return nil
	}
	var entryID string
	// Only a transcript entry already admitted to the safe search projection
	// can become a deep-link target.
	err := tx.QueryRow(ctx, `SELECT e.entry_id::text FROM agent_service.history_entries e
		JOIN agent_service.history_search_documents d ON d.owner_id=e.owner_id
			AND d.logical_dialog_id=e.logical_dialog_id AND d.entry_id=e.entry_id
		WHERE e.owner_id=$1 AND e.logical_dialog_id=$2 AND e.attempt_id=$3
		ORDER BY e.created_at DESC,e.entry_id DESC LIMIT 1`, owner, logicalDialogID, attemptID).Scan(&entryID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return clearHistorySafeText(ctx, tx, owner, logicalDialogID, textID)
		}
		return errors.New("history search text target unavailable")
	}
	var indexedEntryID, indexedSHA string
	var indexedSize int64
	err = tx.QueryRow(ctx, `SELECT entry_id::text,size_bytes,text_sha256
		FROM agent_service.history_search_tool_sources
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, logicalDialogID, textID).
		Scan(&indexedEntryID, &indexedSize, &indexedSHA)
	if err == nil && indexedEntryID == entryID && indexedSize == expectedSize && indexedSHA == expectedSHA {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return errors.New("history search safe text unavailable")
	}
	verified, err := verifyHistorySafeText(ctx, tx, owner, logicalDialogID, textID, expectedCount, expectedSize, expectedSHA)
	if err != nil {
		return err
	}
	if !verified {
		return clearHistorySafeText(ctx, tx, owner, logicalDialogID, textID)
	}
	if err := clearHistorySafeText(ctx, tx, owner, logicalDialogID, textID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_sources(
		owner_id,logical_dialog_id,text_id,entry_id,size_bytes,text_sha256) VALUES($1,$2,$3,$4,$5,$6)`,
		owner, logicalDialogID, textID, entryID, expectedSize, expectedSHA); err != nil {
		return errors.New("history search safe text unavailable")
	}
	segmenter := historySearchTextSegmenter{}
	emit := func(index int64, segment []byte) error {
		if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_search_tool_segments(
			owner_id,logical_dialog_id,text_id,entry_id,segment_index,safe_text) VALUES($1,$2,$3,$4,$5,$6)`,
			owner, logicalDialogID, textID, entryID, index, string(segment)); err != nil {
			return errors.New("history search safe text unavailable")
		}
		return nil
	}
	// Query one bounded R12 chunk at a time so segment insertion never retains
	// the whole tool output (or all generated segments) in process memory.
	for chunkIndex := int64(0); chunkIndex < expectedCount; chunkIndex++ {
		var chunk []byte
		if tx.QueryRow(ctx, `SELECT content FROM agent_service.history_text_chunks
			WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3 AND chunk_index=$4`,
			owner, logicalDialogID, textID, chunkIndex).Scan(&chunk) != nil || segmenter.Write(chunk, emit) != nil {
			return errors.New("history search safe text unavailable")
		}
	}
	if err := segmenter.Flush(emit); err != nil {
		return err
	}
	return nil
}

func clearHistorySafeText(ctx context.Context, tx pgx.Tx, owner, logicalDialogID, textID string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM agent_service.history_search_tool_segments
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, logicalDialogID, textID); err != nil {
		return errors.New("history search safe text unavailable")
	}
	if _, err := tx.Exec(ctx, `DELETE FROM agent_service.history_search_tool_sources
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3`, owner, logicalDialogID, textID); err != nil {
		return errors.New("history search safe text unavailable")
	}
	return nil
}

func verifyHistorySafeText(ctx context.Context, tx pgx.Tx, owner, logicalDialogID, textID string, expectedCount, expectedSize int64, expectedSHA string) (bool, error) {
	rows, err := tx.Query(ctx, `SELECT chunk_index,offset_bytes,size_bytes,chunk_sha256,content
		FROM agent_service.history_text_chunks WHERE owner_id=$1 AND logical_dialog_id=$2 AND text_id=$3
		ORDER BY chunk_index`, owner, logicalDialogID, textID)
	if err != nil {
		return false, errors.New("history search text unavailable")
	}
	defer rows.Close()
	hasher := sha256.New()
	var count, offset int64
	var pending []byte
	valid := true
	for rows.Next() {
		var index, chunkOffset, size int64
		var chunkSHA string
		var content []byte
		if rows.Scan(&index, &chunkOffset, &size, &chunkSHA, &content) != nil {
			return false, errors.New("history search text unavailable")
		}
		if index != count || chunkOffset != offset || size != int64(len(content)) || model.HistoryHashBytes(content) != chunkSHA {
			valid = false
		}
		_, _ = hasher.Write(content)
		if !consumeHistorySearchUTF8(&pending, content) {
			valid = false
		}
		count++
		offset += int64(len(content))
	}
	if rows.Err() != nil {
		return false, errors.New("history search text unavailable")
	}
	valid = valid && len(pending) == 0 && count == expectedCount && offset == expectedSize &&
		hex.EncodeToString(hasher.Sum(nil)) == expectedSHA
	return valid, nil
}

func consumeHistorySearchUTF8(pending *[]byte, chunk []byte) bool {
	data := make([]byte, 0, len(*pending)+len(chunk))
	data = append(data, *pending...)
	data = append(data, chunk...)
	*pending = (*pending)[:0]
	for len(data) > 0 {
		if !utf8.FullRune(data) {
			if len(data) >= utf8.UTFMax {
				return false
			}
			*pending = append(*pending, data...)
			return true
		}
		r, size := utf8.DecodeRune(data)
		if r == utf8.RuneError && size == 1 {
			return false
		}
		data = data[size:]
	}
	return true
}

type historySearchTextSegmenter struct {
	buffer []byte
	index  int64
}

func (s *historySearchTextSegmenter) Write(chunk []byte, emit func(int64, []byte) error) error {
	s.buffer = append(s.buffer, chunk...)
	for len(s.buffer) >= historySearchToolSegmentMaximum {
		end := historySearchToolSegmentMaximum
		for end > 0 && !utf8.Valid(s.buffer[:end]) {
			end--
		}
		if end == 0 {
			return errors.New("history search text is not utf-8")
		}
		segment := append([]byte(nil), s.buffer[:end]...)
		if err := emit(s.index, segment); err != nil {
			return err
		}
		s.index++
		start := end - historySearchToolSegmentOverlap
		if start < 0 {
			start = 0
		}
		for start < end && !utf8.RuneStart(s.buffer[start]) {
			start++
		}
		s.buffer = append([]byte(nil), s.buffer[start:]...)
	}
	return nil
}

func (s *historySearchTextSegmenter) Flush(emit func(int64, []byte) error) error {
	if len(s.buffer) == 0 {
		return nil
	}
	if !utf8.Valid(s.buffer) {
		return errors.New("history search text is not utf-8")
	}
	if err := emit(s.index, append([]byte(nil), s.buffer...)); err != nil {
		return err
	}
	s.index++
	s.buffer = nil
	return nil
}

func searchTextSegments(text []byte) [][]byte {
	var result [][]byte
	segmenter := historySearchTextSegmenter{}
	emit := func(_ int64, segment []byte) error {
		result = append(result, segment)
		return nil
	}
	if segmenter.Write(text, emit) != nil || segmenter.Flush(emit) != nil {
		return nil
	}
	return result
}

func (s *Store) UpsertHistoryDialogMetadata(ctx context.Context, owner, logicalDialogID string, value model.HistoryDialogMetadata) (model.HistoryDialogMetadata, error) {
	if owner == "" || !model.ValidUUID(logicalDialogID) || model.ValidateHistoryDialogMetadata(value) != nil {
		return model.HistoryDialogMetadata{}, ErrHistorySearchScope
	}
	updatedAt, _ := time.Parse(time.RFC3339Nano, value.UpdatedAt)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history metadata transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var marker int
	if err := tx.QueryRow(ctx, `SELECT 1 FROM agent_service.dialog_bindings b JOIN agent_service.logical_dialogs d
		ON d.owner_id=b.owner_id AND d.logical_dialog_id=b.logical_dialog_id
		WHERE b.owner_id=$1 AND b.logical_dialog_id=$2 AND b.node_id=$3 AND b.node_dialog_id=$4 AND b.binding_version=$5
			AND d.deleted_at IS NULL FOR UPDATE OF b,d`, owner, logicalDialogID, value.NodeID, value.NodeDialogID, value.BindingGeneration).Scan(&marker); err != nil {
		return model.HistoryDialogMetadata{}, ErrHistorySearchScope
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_dialog_metadata(
		owner_id,logical_dialog_id,node_id,node_dialog_id,binding_generation,title,archived,source_updated_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(owner_id,logical_dialog_id) DO UPDATE SET node_id=EXCLUDED.node_id,node_dialog_id=EXCLUDED.node_dialog_id,
		binding_generation=EXCLUDED.binding_generation,title=EXCLUDED.title,archived=EXCLUDED.archived,
		source_updated_at=EXCLUDED.source_updated_at,updated_at=EXCLUDED.updated_at
		WHERE agent_service.history_dialog_metadata.source_updated_at<=EXCLUDED.source_updated_at`, owner, logicalDialogID,
		value.NodeID, value.NodeDialogID, value.BindingGeneration, value.Title, value.Archived, updatedAt, s.now().UTC()); err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history metadata unavailable")
	}
	var stored model.HistoryDialogMetadata
	var storedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT node_id::text,node_dialog_id::text,binding_generation,title,archived,source_updated_at
		FROM agent_service.history_dialog_metadata WHERE owner_id=$1 AND logical_dialog_id=$2`, owner, logicalDialogID).
		Scan(&stored.NodeID, &stored.NodeDialogID, &stored.BindingGeneration, &stored.Title, &stored.Archived, &storedAt); err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history metadata unavailable")
	}
	stored.SchemaID, stored.LogicalDialogID, stored.UpdatedAt = model.HistoryMetadataSchemaID, logicalDialogID, storedAt.UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_search_documents SET title_text=''
		WHERE owner_id=$1 AND logical_dialog_id=$2`, owner, logicalDialogID); err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history search title unavailable")
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_search_documents SET title_text=$3
		WHERE owner_id=$1 AND logical_dialog_id=$2 AND entry_id=(SELECT entry_id FROM agent_service.history_search_documents
			WHERE owner_id=$1 AND logical_dialog_id=$2 ORDER BY created_at DESC,entry_id DESC LIMIT 1)`, owner, logicalDialogID, stored.Title); err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history search title unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return model.HistoryDialogMetadata{}, errors.New("history metadata commit unavailable")
	}
	return stored, nil
}

func (s *Store) CreateHistorySearch(ctx context.Context, owner string, spec HistorySearchSpec, queryHash string, limit int) (HistorySearchResult, error) {
	if owner == "" || len(queryHash) != 64 || limit < 1 || limit > model.HistorySearchMaximumPage {
		return HistorySearchResult{}, ErrHistorySearchScope
	}
	snapshotID, err := s.newUUID()
	if err != nil {
		return HistorySearchResult{}, errors.New("history search snapshot unavailable")
	}
	now := s.now().UTC()
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return HistorySearchResult{}, errors.New("history search transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	// Cursor expiry is signed into the opaque cursor, so rows can be reclaimed
	// without changing the required 410 response. A bounded global batch also
	// makes progress for inactive owners after a burst of searches.
	if _, err := tx.Exec(ctx, `DELETE FROM agent_service.history_search_snapshots expired
		WHERE expired.ctid IN (SELECT candidate.ctid FROM agent_service.history_search_snapshots candidate
			WHERE candidate.expires_at<=$1 ORDER BY candidate.expires_at,candidate.snapshot_id LIMIT 100)`, now); err != nil {
		return HistorySearchResult{}, errors.New("history search snapshot cleanup unavailable")
	}
	archivedSet := spec.Query.Archived != nil
	archived := false
	if archivedSet {
		archived = *spec.Query.Archived
	}
	if _, err := tx.Exec(ctx, `INSERT INTO agent_service.history_search_snapshots(
		owner_id,snapshot_id,query_hash,created_at,expires_at,total_count,observed_at,lag_millis,incomplete)
		VALUES($1,$2,$3,$4::timestamptz,$4::timestamptz+interval '5 minutes',0,$4::timestamptz,0,false)`, owner, snapshotID, queryHash, now); err != nil {
		return HistorySearchResult{}, errors.New("history search snapshot unavailable")
	}
	_, err = tx.Exec(ctx, `WITH tool_entries AS (
		SELECT DISTINCT s.logical_dialog_id,s.entry_id
		FROM agent_service.history_search_tool_segments s
		WHERE s.owner_id=$1
	), tool_matches AS (
		SELECT candidate.logical_dialog_id,candidate.entry_id,best.normalized_text,best.rank
		FROM tool_entries candidate
		JOIN LATERAL (
			SELECT s.normalized_text,
				CASE WHEN $3='' THEN 0::real ELSE ts_rank(s.search_vector,plainto_tsquery('simple',$3)) END AS rank
			FROM agent_service.history_search_tool_segments s
			WHERE s.owner_id=$1 AND s.logical_dialog_id=candidate.logical_dialog_id AND s.entry_id=candidate.entry_id
				AND (EXISTS (SELECT 1 FROM unnest($9::text[]) word WHERE s.search_vector @@ plainto_tsquery('simple',word))
					OR EXISTS (SELECT 1 FROM unnest($4::text[]) phrase WHERE strpos(s.normalized_text,phrase)>0))
			ORDER BY rank DESC,s.segment_index LIMIT 1
		) best ON true
	), base_documents AS MATERIALIZED (
		SELECT d.*
		FROM agent_service.history_search_documents d
		LEFT JOIN agent_service.history_dialog_metadata m ON m.owner_id=d.owner_id AND m.logical_dialog_id=d.logical_dialog_id
		JOIN agent_service.logical_dialogs l ON l.owner_id=d.owner_id AND l.logical_dialog_id=d.logical_dialog_id
		LEFT JOIN agent_service.logical_dialog_tombstones t ON t.owner_id=d.owner_id AND t.logical_dialog_id=d.logical_dialog_id
		WHERE d.owner_id=$1 AND l.deleted_at IS NULL AND t.logical_dialog_id IS NULL
			AND ($5='' OR d.node_id=$5::uuid) AND ($6='' OR d.logical_dialog_id=$6::uuid)
			AND (NOT $7 OR COALESCE(m.archived,false)=$8)
	), document_matches AS MATERIALIZED (
		SELECT d.*,
			NOT EXISTS (SELECT 1 FROM unnest($9::text[]) word
				WHERE NOT (d.search_vector @@ plainto_tsquery('simple',word)))
			AND NOT EXISTS (SELECT 1 FROM unnest($4::text[]) phrase
				WHERE NOT (strpos(d.normalized_text,phrase)>0)) AS document_match
		FROM base_documents d
	), matched AS (
		SELECT d.logical_dialog_id,d.entry_id,d.created_at,
			GREATEST(
				CASE WHEN $3='' THEN 0::real ELSE ts_rank(d.search_vector,plainto_tsquery('simple',$3)) END,
				COALESCE(tool.rank,0::real)) AS rank,
			CASE WHEN tool.normalized_text IS NOT NULL THEN tool.normalized_text ELSE d.normalized_text END AS snippet
		FROM document_matches d
		LEFT JOIN tool_matches tool ON tool.logical_dialog_id=d.logical_dialog_id AND tool.entry_id=d.entry_id
		WHERE d.document_match
		UNION ALL
		SELECT d.logical_dialog_id,d.entry_id,d.created_at,
			GREATEST(
				CASE WHEN $3='' THEN 0::real ELSE ts_rank(d.search_vector,plainto_tsquery('simple',$3)) END,
				tool.rank) AS rank,
			tool.normalized_text AS snippet
		FROM document_matches d
		JOIN tool_matches tool ON tool.logical_dialog_id=d.logical_dialog_id AND tool.entry_id=d.entry_id
		WHERE NOT d.document_match
			AND NOT EXISTS (SELECT 1 FROM unnest($9::text[]) word WHERE NOT (
				d.search_vector @@ plainto_tsquery('simple',word) OR EXISTS (
					SELECT 1 FROM agent_service.history_search_tool_segments sw WHERE sw.owner_id=d.owner_id
					AND sw.logical_dialog_id=d.logical_dialog_id AND sw.entry_id=d.entry_id
					AND sw.search_vector @@ plainto_tsquery('simple',word))))
			AND NOT EXISTS (SELECT 1 FROM unnest($4::text[]) phrase WHERE NOT (
				strpos(d.normalized_text,phrase)>0 OR EXISTS (
					SELECT 1 FROM agent_service.history_search_tool_segments sp WHERE sp.owner_id=d.owner_id
					AND sp.logical_dialog_id=d.logical_dialog_id AND sp.entry_id=d.entry_id
					AND strpos(sp.normalized_text,phrase)>0)))
	), ordered AS (
		SELECT logical_dialog_id,entry_id,rank,snippet,row_number() OVER (ORDER BY rank DESC,created_at DESC,entry_id DESC)-1 AS position
		FROM matched
	)
	INSERT INTO agent_service.history_search_snapshot_items(owner_id,snapshot_id,position,logical_dialog_id,entry_id,rank,snippet)
		SELECT $1,$2,position,logical_dialog_id,entry_id,rank,
			agent_service.history_search_snippet(snippet,$10) FROM ordered`, owner, snapshotID, spec.RankText, spec.Phrases,
		spec.Query.NodeID, spec.Query.LogicalDialogID, archivedSet, archived, spec.Terms, model.HistorySearchMaximumSnippet)
	if err != nil {
		return HistorySearchResult{}, errors.New("history search query unavailable")
	}
	var total, lag int64
	var observed time.Time
	var incomplete bool
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM agent_service.history_search_snapshot_items WHERE owner_id=$1 AND snapshot_id=$2`, owner, snapshotID).Scan(&total); err != nil {
		return HistorySearchResult{}, errors.New("history search count unavailable")
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max((extract(epoch FROM ($2-source_captured_at))*1000)::bigint),0),
		COALESCE(bool_or(NOT complete),false),COALESCE(max(observed_at),$2)
		FROM agent_service.history_replica_streams WHERE owner_id=$1`, owner, now).Scan(&lag, &incomplete, &observed); err != nil {
		return HistorySearchResult{}, errors.New("history search coverage unavailable")
	}
	if lag < 0 {
		lag = 0
	}
	if _, err := tx.Exec(ctx, `UPDATE agent_service.history_search_snapshots SET
		total_count=$3,observed_at=$4,lag_millis=$5,incomplete=$6 WHERE owner_id=$1 AND snapshot_id=$2`,
		owner, snapshotID, total, observed, lag, incomplete); err != nil {
		return HistorySearchResult{}, errors.New("history search snapshot unavailable")
	}
	if err := tx.Commit(ctx); err != nil {
		return HistorySearchResult{}, errors.New("history search commit unavailable")
	}
	return s.ReadHistorySearch(ctx, owner, spec.Query, HistorySearchPosition{
		SnapshotID: snapshotID, QueryHash: queryHash, ExpiresAt: now.Add(5 * time.Minute).Format(time.RFC3339Nano),
	}, limit)
}

func (s *Store) ReadHistorySearch(ctx context.Context, owner string, query model.HistorySearchQuery, position HistorySearchPosition, limit int) (HistorySearchResult, error) {
	cursorExpiry, expiryErr := time.Parse(time.RFC3339Nano, position.ExpiresAt)
	if owner == "" || !model.ValidUUID(position.SnapshotID) || len(position.QueryHash) != 64 || position.Offset < 0 ||
		expiryErr != nil || cursorExpiry.Format(time.RFC3339Nano) != position.ExpiresAt ||
		limit < 1 || limit > model.HistorySearchMaximumPage {
		return HistorySearchResult{}, ErrHistorySearchScope
	}
	if !s.now().UTC().Before(cursorExpiry) {
		return HistorySearchResult{}, ErrHistorySearchExpired
	}
	var expires, observed time.Time
	var total, lag int64
	var incomplete bool
	var storedHash string
	if err := s.pool.QueryRow(ctx, `SELECT query_hash,expires_at,observed_at,lag_millis,incomplete
		FROM agent_service.history_search_snapshots WHERE owner_id=$1 AND snapshot_id=$2`, owner, position.SnapshotID).
		Scan(&storedHash, &expires, &observed, &lag, &incomplete); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return HistorySearchResult{}, pgx.ErrNoRows
		}
		return HistorySearchResult{}, errors.New("history search snapshot unavailable")
	}
	if storedHash != position.QueryHash {
		return HistorySearchResult{}, ErrHistorySearchScope
	}
	if !s.now().UTC().Before(expires) {
		return HistorySearchResult{}, ErrHistorySearchExpired
	}
	// Count only rows that remain visible now. The snapshot preserves order and
	// append stability, while live joins enforce deletion/ACL semantics on every
	// read, including the count shown to the operator.
	if err := s.pool.QueryRow(ctx, `SELECT count(*)
		FROM agent_service.history_search_snapshot_items i
		JOIN agent_service.history_search_documents d ON d.owner_id=i.owner_id AND d.logical_dialog_id=i.logical_dialog_id AND d.entry_id=i.entry_id
		JOIN agent_service.logical_dialogs l ON l.owner_id=d.owner_id AND l.logical_dialog_id=d.logical_dialog_id AND l.deleted_at IS NULL
		LEFT JOIN agent_service.logical_dialog_tombstones t ON t.owner_id=d.owner_id AND t.logical_dialog_id=d.logical_dialog_id
		WHERE i.owner_id=$1 AND i.snapshot_id=$2 AND t.logical_dialog_id IS NULL`, owner, position.SnapshotID).Scan(&total); err != nil {
		return HistorySearchResult{}, errors.New("history search count unavailable")
	}
	rows, err := s.pool.Query(ctx, `SELECT i.position,d.logical_dialog_id::text,d.entry_id::text,d.node_id::text,d.node_dialog_id::text,
		COALESCE(m.title,''),d.role,d.kind,d.created_at,i.rank,i.snippet
		FROM agent_service.history_search_snapshot_items i
		JOIN agent_service.history_search_documents d ON d.owner_id=i.owner_id AND d.logical_dialog_id=i.logical_dialog_id AND d.entry_id=i.entry_id
		JOIN agent_service.logical_dialogs l ON l.owner_id=d.owner_id AND l.logical_dialog_id=d.logical_dialog_id AND l.deleted_at IS NULL
		LEFT JOIN agent_service.logical_dialog_tombstones t ON t.owner_id=d.owner_id AND t.logical_dialog_id=d.logical_dialog_id
		LEFT JOIN agent_service.history_dialog_metadata m ON m.owner_id=d.owner_id AND m.logical_dialog_id=d.logical_dialog_id
		WHERE i.owner_id=$1 AND i.snapshot_id=$2 AND i.position>=$3 AND t.logical_dialog_id IS NULL
		ORDER BY i.position LIMIT $4`, owner, position.SnapshotID, position.Offset, limit+1)
	if err != nil {
		return HistorySearchResult{}, errors.New("history search page unavailable")
	}
	defer rows.Close()
	items := make([]model.HistorySearchItem, 0, limit)
	lastPosition := position.Offset
	hasMore := false
	for rows.Next() {
		var item model.HistorySearchItem
		var created time.Time
		var normalized string
		var itemPosition int64
		if rows.Scan(&itemPosition, &item.LogicalDialogID, &item.EntryID, &item.NodeID, &item.NodeDialogID, &item.Title, &item.Role, &item.Kind, &created, &item.Rank, &normalized) != nil {
			return HistorySearchResult{}, errors.New("history search page unavailable")
		}
		if len(items) == limit {
			hasMore = true
			break
		}
		lastPosition = itemPosition + 1
		item.CreatedAt = created.UTC().Format(time.RFC3339Nano)
		item.Snippet = truncateUTF8(normalized, model.HistorySearchMaximumSnippet)
		items = append(items, item)
	}
	if rows.Err() != nil {
		return HistorySearchResult{}, errors.New("history search page unavailable")
	}
	page := model.HistorySearchPage{SchemaID: model.HistorySearchSchemaID, Query: query, Items: items, TotalCount: total,
		ObservedAt: observed.UTC().Format(time.RFC3339Nano), LagMillis: lag, Incomplete: incomplete}
	return HistorySearchResult{Page: page, HasMore: hasMore, Position: HistorySearchPosition{
		SnapshotID: position.SnapshotID, Offset: lastPosition, QueryHash: position.QueryHash,
		ExpiresAt: expires.UTC().Format(time.RFC3339Nano),
	}}, nil
}

func truncateUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func (s *Store) HistoryEntryByID(ctx context.Context, owner, logicalDialogID, entryID string) (model.HistoryEntryLookup, error) {
	if owner == "" || !model.ValidUUID(logicalDialogID) || !model.ValidUUID(entryID) {
		return model.HistoryEntryLookup{}, ErrHistorySearchScope
	}
	var raw []byte
	var observed, source time.Time
	var incomplete bool
	err := s.pool.QueryRow(ctx, `SELECT e.payload,COALESCE(max(r.observed_at),e.imported_at),COALESCE(max(r.source_captured_at),e.imported_at),COALESCE(bool_or(NOT r.complete),false)
		FROM agent_service.history_entries e LEFT JOIN agent_service.history_replica_streams r ON r.owner_id=e.owner_id AND r.logical_dialog_id=e.logical_dialog_id
		JOIN agent_service.logical_dialogs d ON d.owner_id=e.owner_id AND d.logical_dialog_id=e.logical_dialog_id AND d.deleted_at IS NULL
		LEFT JOIN agent_service.logical_dialog_tombstones t ON t.owner_id=e.owner_id AND t.logical_dialog_id=e.logical_dialog_id
		WHERE e.owner_id=$1 AND e.logical_dialog_id=$2 AND e.entry_id=$3 AND t.logical_dialog_id IS NULL
		GROUP BY e.payload,e.imported_at`, owner, logicalDialogID, entryID).Scan(&raw, &observed, &source, &incomplete)
	if err != nil {
		return model.HistoryEntryLookup{}, err
	}
	var entry model.HistoryTranscriptEntry
	if json.Unmarshal(raw, &entry) != nil || model.ValidateHistorySearchEntry(entry) != nil {
		// Unsafe opaque replica records are deliberately indistinguishable from
		// absent records at the search/read projection boundary.
		return model.HistoryEntryLookup{}, pgx.ErrNoRows
	}
	lag := s.now().UTC().Sub(source).Milliseconds()
	if lag < 0 {
		lag = 0
	}
	return model.HistoryEntryLookup{SchemaID: model.HistoryEntryLookupSchemaID, Entry: entry, ObservedAt: observed.UTC().Format(time.RFC3339Nano), LagMillis: lag, Incomplete: incomplete}, nil
}
