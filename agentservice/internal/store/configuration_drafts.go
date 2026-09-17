package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/configdraft"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/jackc/pgx/v5"
)

var (
	ErrConfigurationDraftNotFound = errors.New("configuration draft not found")
	ErrConfigurationDraftConflict = errors.New("configuration draft version conflict")
)

func (s *Store) GetConfigurationDraft(ctx context.Context, owner, nodeID string) (model.ConfigurationDraft, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(nodeID) {
		return model.ConfigurationDraft{}, ErrConfigurationDraftNotFound
	}
	return getConfigurationDraft(ctx, s.pool, owner, nodeID)
}

func (s *Store) SaveConfigurationDraft(ctx context.Context, owner, nodeID string, input model.ConfigurationSave, validation configdraft.Result) (model.ConfigurationDraft, error) {
	if !model.ValidActor(owner) || !model.ValidUUID(nodeID) || !model.ValidateConfigurationSave(input) {
		return model.ConfigurationDraft{}, errors.New("invalid configuration draft")
	}
	encoded, err := json.Marshal(validation.Validation)
	if err != nil {
		return model.ConfigurationDraft{}, errors.New("configuration validation unavailable")
	}
	var manifest any
	if input.BuildContextManifest != nil {
		raw, marshalErr := json.Marshal(input.BuildContextManifest)
		if marshalErr != nil {
			return model.ConfigurationDraft{}, errors.New("build context manifest unavailable")
		}
		manifest = raw
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return model.ConfigurationDraft{}, errors.New("configuration transaction unavailable")
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", "configuration-draft:"+owner+":"+nodeID); err != nil {
		return model.ConfigurationDraft{}, errors.New("configuration lock unavailable")
	}
	var current int64
	err = tx.QueryRow(ctx, "SELECT draft_version FROM agent_service.configuration_drafts WHERE owner_id=$1 AND node_id=$2 FOR UPDATE", owner, nodeID).Scan(&current)
	if input.ExpectedDraftVersion == 0 {
		if err == nil || !errors.Is(err, pgx.ErrNoRows) {
			return model.ConfigurationDraft{}, ErrConfigurationDraftConflict
		}
		command, insertErr := tx.Exec(ctx, `INSERT INTO agent_service.configuration_drafts(owner_id,node_id,draft_version,raw_json_text,raw_dockerfile_text,build_context_manifest,validation) SELECT $1,$2,1,$3,$4,$5::jsonb,$6::jsonb WHERE EXISTS(SELECT 1 FROM agent_service.instances WHERE owner_id=$1 AND node_id=$2)`, owner, nodeID, input.RawJSONText, input.RawDockerfileText, manifest, encoded)
		if insertErr != nil {
			return model.ConfigurationDraft{}, errors.New("configuration save unavailable")
		}
		if command.RowsAffected() != 1 {
			return model.ConfigurationDraft{}, ErrConfigurationDraftNotFound
		}
	} else {
		if errors.Is(err, pgx.ErrNoRows) {
			return model.ConfigurationDraft{}, ErrConfigurationDraftNotFound
		}
		if err != nil || current != input.ExpectedDraftVersion || current >= model.MaximumSafeInt {
			return model.ConfigurationDraft{}, ErrConfigurationDraftConflict
		}
		command, updateErr := tx.Exec(ctx, `UPDATE agent_service.configuration_drafts SET draft_version=draft_version+1,raw_json_text=$4,raw_dockerfile_text=$5,build_context_manifest=$6::jsonb,validation=$7::jsonb,updated_at=clock_timestamp() WHERE owner_id=$1 AND node_id=$2 AND draft_version=$3`, owner, nodeID, input.ExpectedDraftVersion, input.RawJSONText, input.RawDockerfileText, manifest, encoded)
		if updateErr != nil || command.RowsAffected() != 1 {
			return model.ConfigurationDraft{}, ErrConfigurationDraftConflict
		}
	}
	draft, err := getConfigurationDraft(ctx, tx, owner, nodeID)
	if err != nil {
		return model.ConfigurationDraft{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return model.ConfigurationDraft{}, errors.New("configuration commit unavailable")
	}
	return draft, nil
}

type configRow interface{ Scan(...any) error }
type configQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func getConfigurationDraft(ctx context.Context, query configQuerier, owner, nodeID string) (model.ConfigurationDraft, error) {
	var draft model.ConfigurationDraft
	var rawValidation []byte
	var rawManifest []byte
	err := query.QueryRow(ctx, `SELECT schema_id,node_id::text,draft_version,raw_json_text,raw_dockerfile_text,build_context_manifest,validation FROM agent_service.configuration_drafts WHERE owner_id=$1 AND node_id=$2`, owner, nodeID).Scan(&draft.SchemaID, &draft.NodeID, &draft.DraftVersion, &draft.RawJSONText, &draft.RawDockerfileText, &rawManifest, &rawValidation)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ConfigurationDraft{}, ErrConfigurationDraftNotFound
	}
	if err != nil || json.Unmarshal(rawValidation, &draft.Validation) != nil {
		return model.ConfigurationDraft{}, errors.New("configuration draft unavailable")
	}
	if len(rawManifest) > 0 && json.Unmarshal(rawManifest, &draft.BuildContextManifest) != nil {
		return model.ConfigurationDraft{}, errors.New("configuration draft unavailable")
	}
	return draft, nil
}
