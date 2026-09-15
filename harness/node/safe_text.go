package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessadapter"
	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
	"github.com/boxvtk621/homelab-telegram-panel/internal/transcriptview"
)

const (
	safeTextArtifactNamePrefix = "safe-text-"
	safeTextManifestMediaType  = "application/vnd.homelab.transcript-view-v1+json"
	safeTextChunkMediaType     = "text/plain; charset=utf-8"
	safeTextDisposition        = "transcript_internal"
)

type safeTextArtifactRow struct {
	artifactID  string
	dialogID    string
	attemptID   string
	callID      string
	name        string
	mediaType   string
	sizeBytes   int64
	sha256      string
	redaction   string
	truncated   bool
	disposition string
	relative    string
}

type safeTextPublishedArtifact struct {
	id       string
	name     string
	media    string
	size     int64
	hash     string
	relative string
}

type safeTextQuery interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

const safeTextArtifactColumns = `artifact_id,dialog_id,attempt_id,COALESCE(call_id,''),name,media_type,size_bytes,sha256,redaction,truncated,disposition,relative_path`

func stableSafeTextUUID(namespace, value string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + value))
	raw := sum[:16]
	raw[6] = raw[6]&0x0f | 0x50
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:])
}

func safeTextID(reference harnessadapter.AttemptRef, source transcriptview.Source) string {
	value := strings.Join([]string{
		reference.NodeID, reference.DialogID, reference.RequestID, reference.AttemptID,
		strconv.FormatInt(reference.Generation, 10), source.Kind, source.ID,
		strconv.FormatInt(source.Index, 10), source.Stream,
	}, "\x00")
	return stableSafeTextUUID("transcript-text-v1", value)
}

func safeTextManifestID(textID string) string {
	return stableSafeTextUUID("transcript-manifest-v1", textID)
}

func safeTextChunkID(textID string, index int64) string {
	return stableSafeTextUUID("transcript-chunk-v1", textID+"\x00"+strconv.FormatInt(index, 10))
}

func safeTextManifestName(textID string) string {
	return safeTextArtifactNamePrefix + "manifest-" + textID + ".json"
}

func safeTextChunkName(_ string, index int64) string {
	return fmt.Sprintf("%s%02d.txt", safeTextArtifactNamePrefix, index)
}

func reservedSafeTextArtifact(name, mediaType string) bool {
	return strings.HasPrefix(name, safeTextArtifactNamePrefix) || mediaType == safeTextManifestMediaType
}

func safeTextPreview(value string) string {
	if len(value) <= transcriptview.MaximumPreview {
		return value
	}
	preview := value[:transcriptview.MaximumPreview]
	for preview != "" && !utf8.ValidString(preview) {
		preview = preview[:len(preview)-1]
	}
	return preview
}

func safeTextValue(content harnessprotocol.SafeContent, full *string, incomplete bool) (string, bool, error) {
	if content.Kind != "inline" {
		if full != nil || incomplete {
			return "", false, errors.New("non-inline content carried full safe text")
		}
		return "", false, nil
	}
	if content.Redaction != "none" && content.Redaction != "applied" {
		return "", false, errors.New("safe text redaction is invalid")
	}
	if full == nil {
		if incomplete {
			return "", false, errors.New("incomplete safe text has no retained prefix")
		}
		if content.Truncated {
			return "", false, nil
		}
		return content.Content, true, nil
	}
	if !utf8.ValidString(*full) {
		return "", false, errors.New("full safe text is not UTF-8")
	}
	preview := safeTextPreview(*full)
	if content.Content != preview || content.Truncated != (incomplete || len(preview) != len(*full)) {
		return "", false, errors.New("full safe text does not match preview")
	}
	return *full, true, nil
}

func safeTextManifestMatches(manifest transcriptview.Manifest, reference harnessadapter.AttemptRef, source transcriptview.Source, preview, redaction string, size int64, hash string, upstreamIncomplete bool) bool {
	return manifest.NodeID == reference.NodeID && manifest.DialogID == reference.DialogID &&
		manifest.AttemptID == reference.AttemptID && manifest.Generation == reference.Generation && manifest.Source == source &&
		manifest.Preview == preview && manifest.PreviewTruncated == (upstreamIncomplete || int64(len(preview)) != size) &&
		manifest.Redaction == redaction && manifest.SizeBytes == size && manifest.SHA256 == hash &&
		(!upstreamIncomplete || !manifest.Complete)
}

func expectedSafeTextCallID(source transcriptview.Source) string {
	if source.Kind == "assistant_message" {
		return ""
	}
	return source.ID
}

func scanSafeTextArtifact(scanner interface{ Scan(...any) error }, row *safeTextArtifactRow) error {
	return scanner.Scan(
		&row.artifactID, &row.dialogID, &row.attemptID, &row.callID, &row.name, &row.mediaType,
		&row.sizeBytes, &row.sha256, &row.redaction, &row.truncated, &row.disposition, &row.relative,
	)
}

func (node *Node) readSafeTextArtifact(row safeTextArtifactRow) ([]byte, error) {
	metadata := harnessprotocol.ArtifactMetadata{ArtifactID: row.artifactID, SizeBytes: row.sizeBytes, SHA256: row.sha256}
	return node.readArtifactFile(row.relative, metadata)
}

func validateSafeTextManifestRow(row safeTextArtifactRow, raw []byte, manifest transcriptview.Manifest, reference harnessadapter.AttemptRef) error {
	if manifest.NodeID != reference.NodeID || manifest.DialogID != reference.DialogID ||
		manifest.AttemptID != reference.AttemptID || manifest.Generation != reference.Generation ||
		manifest.TextID != safeTextID(reference, manifest.Source) ||
		row.artifactID != safeTextManifestID(manifest.TextID) || row.dialogID != reference.DialogID ||
		row.attemptID != reference.AttemptID || row.callID != expectedSafeTextCallID(manifest.Source) ||
		row.name != safeTextManifestName(manifest.TextID) || row.mediaType != safeTextManifestMediaType ||
		row.sizeBytes != int64(len(raw)) || row.sha256 != digestBytes(raw) || row.redaction != manifest.Redaction ||
		row.truncated || row.disposition != safeTextDisposition || row.relative != filepath.Join("artifacts", row.artifactID) {
		return errors.New("safe text manifest binding mismatch")
	}
	return nil
}

func (node *Node) readSafeTextManifest(ctx context.Context, query safeTextQuery, reference harnessadapter.AttemptRef, source transcriptview.Source) (transcriptview.Manifest, []byte, error) {
	textID := safeTextID(reference, source)
	manifestID := safeTextManifestID(textID)
	var row safeTextArtifactRow
	if err := scanSafeTextArtifact(query.QueryRowContext(ctx, "SELECT "+safeTextArtifactColumns+" FROM artifacts WHERE artifact_id=?", manifestID), &row); err != nil {
		return transcriptview.Manifest{}, nil, err
	}
	raw, err := node.readSafeTextArtifact(row)
	if err != nil {
		return transcriptview.Manifest{}, nil, err
	}
	manifest, err := transcriptview.Decode(raw)
	if err != nil || manifest.Source != source || manifest.TextID != textID {
		return transcriptview.Manifest{}, nil, errors.New("safe text manifest is invalid")
	}
	if err := validateSafeTextManifestRow(row, raw, manifest, reference); err != nil {
		return transcriptview.Manifest{}, nil, err
	}
	if err := node.validateSafeTextRows(ctx, query, manifest); err != nil {
		return transcriptview.Manifest{}, nil, err
	}
	return manifest, raw, nil
}

// validateExistingSafeTextReplay compares private producer bytes on an event
// replay without creating new state. Older durable rows legitimately have no
// transcript manifest; a new replay must not manufacture integration history.
func (node *Node) validateExistingSafeTextReplay(ctx context.Context, tx *sql.Tx, reference harnessadapter.AttemptRef, source transcriptview.Source, content harnessprotocol.SafeContent, full *string, upstreamIncomplete bool) error {
	value, available, err := safeTextValue(content, full, upstreamIncomplete)
	if err != nil || !available {
		return err
	}
	if err := transcriptview.ValidateSource(source); err != nil {
		return err
	}
	manifest, _, err := node.readSafeTextManifest(ctx, tx, reference, source)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(value))
	if !safeTextManifestMatches(manifest, reference, source, safeTextPreview(value), content.Redaction, int64(len(value)), hex.EncodeToString(digest[:]), upstreamIncomplete) {
		return errors.New("conflicting safe text source")
	}
	return nil
}

// captureSafeText persists producer-observed safe bytes before its frozen
// wire-v2 preview is committed. Reserved artifact rows retain the manifest and
// chunks without changing the Harness database or wire schema.
func (node *Node) captureSafeText(ctx context.Context, tx *sql.Tx, reference harnessadapter.AttemptRef, source transcriptview.Source, content harnessprotocol.SafeContent, full *string, upstreamIncomplete bool) error {
	value, available, err := safeTextValue(content, full, upstreamIncomplete)
	if err != nil || !available {
		return err
	}
	if err := transcriptview.ValidateSource(source); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(value))
	textHash := hex.EncodeToString(digest[:])
	preview := safeTextPreview(value)

	manifest, _, err := node.readSafeTextManifest(ctx, tx, reference, source)
	if err == nil {
		if !safeTextManifestMatches(manifest, reference, source, preview, content.Redaction, int64(len(value)), textHash, upstreamIncomplete) {
			return errors.New("conflicting safe text source")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	captured, err := node.safeTextCapturedBytes(ctx, tx, reference)
	if err != nil {
		return err
	}
	complete := safeTextCanComplete(captured, int64(len(value)), upstreamIncomplete)
	textID := safeTextID(reference, source)
	manifest = transcriptview.Manifest{
		SchemaID: transcriptview.SchemaID, NodeID: reference.NodeID, DialogID: reference.DialogID,
		AttemptID: reference.AttemptID, Generation: reference.Generation, TextID: textID, Source: source,
		Preview: preview, PreviewTruncated: upstreamIncomplete || len(preview) != len(value), Redaction: content.Redaction,
		Complete: complete, SizeBytes: int64(len(value)), SHA256: textHash, Chunks: []transcriptview.Chunk{},
	}
	if !complete {
		manifest.Reason = "output_limit_exceeded"
	}

	published := make([]safeTextPublishedArtifact, 0)
	if complete {
		for start, index := 0, int64(0); start < len(value); index++ {
			end := start + transcriptview.MaximumChunkBytes
			if end > len(value) {
				end = len(value)
			}
			for end > start && !utf8.ValidString(value[start:end]) {
				end--
			}
			if end == start {
				return errors.New("safe text chunk boundary is invalid")
			}
			chunkBytes := []byte(value[start:end])
			chunkHash := digestBytes(chunkBytes)
			artifactID := safeTextChunkID(textID, index)
			relative, err := node.publishSafeTextArtifact(artifactID, chunkBytes)
			if err != nil {
				return err
			}
			manifest.Chunks = append(manifest.Chunks, transcriptview.Chunk{
				Index: index, OffsetBytes: int64(start), ArtifactID: artifactID,
				SizeBytes: int64(len(chunkBytes)), SHA256: chunkHash,
			})
			published = append(published, safeTextPublishedArtifact{
				id: artifactID, name: safeTextChunkName(textID, index), media: safeTextChunkMediaType,
				size: int64(len(chunkBytes)), hash: chunkHash, relative: relative,
			})
			start = end
		}
	}
	manifestJSON, err := transcriptview.Encode(manifest)
	if err != nil {
		return err
	}
	manifestID := safeTextManifestID(textID)
	manifestRelative, err := node.publishSafeTextArtifact(manifestID, manifestJSON)
	if err != nil {
		return err
	}
	callID := expectedSafeTextCallID(source)
	for _, artifact := range published {
		if err := insertSafeTextArtifact(ctx, tx, reference, callID, artifact, content.Redaction); err != nil {
			return err
		}
	}
	return insertSafeTextArtifact(ctx, tx, reference, callID, safeTextPublishedArtifact{
		id: manifestID, name: safeTextManifestName(textID), media: safeTextManifestMediaType,
		size: int64(len(manifestJSON)), hash: digestBytes(manifestJSON), relative: manifestRelative,
	}, content.Redaction)
}

func insertSafeTextArtifact(ctx context.Context, tx *sql.Tx, reference harnessadapter.AttemptRef, callID string, artifact safeTextPublishedArtifact, redaction string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO artifacts(artifact_id,dialog_id,attempt_id,call_id,name,media_type,size_bytes,sha256,redaction,truncated,disposition,relative_path) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		artifact.id, reference.DialogID, reference.AttemptID, nullText(callID), artifact.name, artifact.media,
		artifact.size, artifact.hash, redaction, false, safeTextDisposition, artifact.relative)
	return err
}

func (node *Node) safeTextCapturedBytes(ctx context.Context, query safeTextQuery, reference harnessadapter.AttemptRef) (int64, error) {
	rows, err := query.QueryContext(ctx, "SELECT "+safeTextArtifactColumns+" FROM artifacts WHERE attempt_id=? AND media_type=? ORDER BY artifact_id", reference.AttemptID, safeTextManifestMediaType)
	if err != nil {
		return 0, err
	}
	var records []safeTextArtifactRow
	for rows.Next() {
		var row safeTextArtifactRow
		if err := scanSafeTextArtifact(rows, &row); err != nil {
			rows.Close()
			return 0, err
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	var captured int64
	for _, row := range records {
		raw, err := node.readSafeTextArtifact(row)
		if err != nil {
			return 0, err
		}
		manifest, err := transcriptview.Decode(raw)
		if err != nil || validateSafeTextManifestRow(row, raw, manifest, reference) != nil {
			return 0, errors.New("safe text accounting manifest is invalid")
		}
		if err := node.validateSafeTextRows(ctx, query, manifest); err != nil {
			return 0, err
		}
		if manifest.Complete {
			if captured > transcriptview.MaximumTextBytes-manifest.SizeBytes {
				return 0, errors.New("safe text accounting exceeds limit")
			}
			captured += manifest.SizeBytes
		}
	}
	return captured, nil
}

func safeTextCanComplete(captured, size int64, upstreamIncomplete bool) bool {
	return !upstreamIncomplete && captured >= 0 && size >= 0 &&
		size <= transcriptview.MaximumTextBytes && captured <= transcriptview.MaximumTextBytes-size
}

func (node *Node) publishSafeTextArtifact(artifactID string, content []byte) (string, error) {
	directory := filepath.Join(node.config.DataDir, "artifacts")
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm()&0o077 != 0 || validateOwner(info) != nil {
		return "", errors.New("artifact directory is unsafe")
	}
	relative := filepath.Join("artifacts", artifactID)
	final := filepath.Join(directory, artifactID)
	metadata := harnessprotocol.ArtifactMetadata{ArtifactID: artifactID, SizeBytes: int64(len(content)), SHA256: digestBytes(content)}
	if _, err := os.Lstat(final); err == nil {
		body, readErr := node.readArtifactFile(relative, metadata)
		if readErr != nil || !bytes.Equal(body, content) {
			return "", errors.New("orphan safe text artifact conflicts with source")
		}
		return relative, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	temporaryID, err := node.newID()
	if err != nil {
		return "", err
	}
	temporary := filepath.Join(directory, "."+temporaryID+".tmp")
	file, err := secureOpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	keep := false
	defer func() {
		_ = file.Close()
		if !keep {
			_ = os.Remove(temporary)
		}
	}()
	written, err := io.Copy(file, bytes.NewReader(content))
	if err != nil || written != int64(len(content)) {
		return "", errors.New("safe text artifact write failed")
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, final); err != nil {
		return "", err
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return "", err
	}
	if err := directoryHandle.Sync(); err != nil {
		directoryHandle.Close()
		return "", err
	}
	if err := directoryHandle.Close(); err != nil {
		return "", err
	}
	keep = true
	return relative, nil
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func (node *Node) SafeTextManifest(ctx context.Context, trust TrustContext, dialogID, attemptID string, source transcriptview.Source) Result {
	if denied := node.authorizeRead(trust); denied != nil {
		return *denied
	}
	if !uuidPattern.MatchString(dialogID) || !uuidPattern.MatchString(attemptID) || transcriptview.ValidateSource(source) != nil {
		return node.Invalid("transcript source is invalid")
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	tx, err := node.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return node.errorResult(503, "not_durable", "safe text is unavailable", source.ID, nil, "")
	}
	defer tx.Rollback()
	state, err := loadState(ctx, tx)
	if err != nil {
		return node.errorResult(503, "not_durable", "safe text is unavailable", source.ID, nil, "")
	}
	var requestID string
	var generation int64
	err = tx.QueryRowContext(ctx, `SELECT a.request_id,a.generation FROM attempts a JOIN dialogs d ON d.dialog_id=a.dialog_id
		WHERE a.attempt_id=? AND a.dialog_id=? AND d.node_id=? AND d.owner_id=? AND NOT EXISTS (
			SELECT 1 FROM events deleted WHERE deleted.dialog_id=d.dialog_id AND deleted.projection_key='dialog.deleted')`,
		attemptID, dialogID, state.NodeID, state.OwnerID).Scan(&requestID, &generation)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return node.errorResult(404, "not_found", "safe text was not found", source.ID, nil, "")
		}
		return node.errorResult(503, "not_durable", "safe text is unavailable", source.ID, nil, "")
	}
	reference := harnessadapter.AttemptRef{
		NodeID: state.NodeID, DialogID: dialogID, RequestID: requestID, AttemptID: attemptID, Generation: generation,
	}
	_, raw, err := node.readSafeTextManifest(ctx, tx, reference, source)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return node.errorResult(404, "not_found", "safe text was not found", source.ID, nil, "")
		}
		return node.errorResult(503, "not_durable", "safe text manifest mismatch", source.ID, nil, "")
	}
	return Result{HTTPStatus: 200, Body: bytes.Clone(raw)}
}

func (node *Node) validateSafeTextRows(ctx context.Context, query safeTextQuery, manifest transcriptview.Manifest) error {
	expectedCallID := expectedSafeTextCallID(manifest.Source)
	for _, chunk := range manifest.Chunks {
		var row safeTextArtifactRow
		if err := scanSafeTextArtifact(query.QueryRowContext(ctx, "SELECT "+safeTextArtifactColumns+" FROM artifacts WHERE artifact_id=?", chunk.ArtifactID), &row); err != nil {
			return err
		}
		if chunk.ArtifactID != safeTextChunkID(manifest.TextID, chunk.Index) || row.artifactID != chunk.ArtifactID ||
			row.dialogID != manifest.DialogID || row.attemptID != manifest.AttemptID || row.callID != expectedCallID ||
			row.name != safeTextChunkName(manifest.TextID, chunk.Index) || row.mediaType != safeTextChunkMediaType ||
			row.sizeBytes != chunk.SizeBytes || row.sha256 != chunk.SHA256 || row.redaction != manifest.Redaction ||
			row.truncated || row.disposition != safeTextDisposition || row.relative != filepath.Join("artifacts", row.artifactID) {
			return errors.New("safe text chunk binding mismatch")
		}
	}
	return nil
}
