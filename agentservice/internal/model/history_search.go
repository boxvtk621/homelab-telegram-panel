package model

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	HistorySearchSchemaID       = "agent-search-v1"
	HistoryMetadataSchemaID     = "agent-search-dialog-metadata-v1"
	HistoryEntryLookupSchemaID  = "agent-history-entry-v1"
	HistorySearchMaximumPage    = 50
	HistorySearchMaximumQuery   = 1000
	HistorySearchMaximumTitle   = 1000
	HistorySearchMaximumSnippet = 512
)

type HistorySearchQuery struct {
	Q               string `json:"q"`
	NodeID          string `json:"nodeId,omitempty"`
	LogicalDialogID string `json:"logicalDialogId,omitempty"`
	Archived        *bool  `json:"archived,omitempty"`
}

type HistorySearchItem struct {
	LogicalDialogID string  `json:"logicalDialogId"`
	EntryID         string  `json:"entryId"`
	NodeID          string  `json:"nodeId"`
	NodeDialogID    string  `json:"nodeDialogId"`
	Title           string  `json:"title"`
	Role            string  `json:"role"`
	Kind            string  `json:"kind"`
	CreatedAt       string  `json:"createdAt"`
	Rank            float32 `json:"rank"`
	Snippet         string  `json:"snippet"`
}

type HistorySearchPage struct {
	SchemaID   string              `json:"schemaId"`
	Query      HistorySearchQuery  `json:"query"`
	Items      []HistorySearchItem `json:"items"`
	NextCursor *string             `json:"nextCursor"`
	TotalCount int64               `json:"totalCount"`
	ObservedAt string              `json:"observedAt"`
	LagMillis  int64               `json:"lagMillis"`
	Incomplete bool                `json:"incomplete"`
}

type HistoryDialogMetadata struct {
	SchemaID          string `json:"schemaId"`
	LogicalDialogID   string `json:"logicalDialogId,omitempty"`
	NodeID            string `json:"nodeId"`
	NodeDialogID      string `json:"nodeDialogId"`
	BindingGeneration int64  `json:"bindingGeneration"`
	Title             string `json:"title"`
	Archived          bool   `json:"archived"`
	UpdatedAt         string `json:"updatedAt"`
}

type HistoryEntryLookup struct {
	SchemaID   string                 `json:"schemaId"`
	Entry      HistoryTranscriptEntry `json:"entry"`
	ObservedAt string                 `json:"observedAt"`
	LagMillis  int64                  `json:"lagMillis"`
	Incomplete bool                   `json:"incomplete"`
}

func ValidateHistoryDialogMetadata(value HistoryDialogMetadata) error {
	if value.SchemaID != HistoryMetadataSchemaID || value.LogicalDialogID != "" || !ValidUUID(value.NodeID) ||
		!ValidUUID(value.NodeDialogID) || value.BindingGeneration < 1 || value.BindingGeneration > HistoryMaximumSafeInt ||
		!utf8.ValidString(value.Title) || len([]byte(value.Title)) > HistorySearchMaximumTitle || strings.ContainsRune(value.Title, '\x00') {
		return errors.New("history dialog metadata is invalid")
	}
	if parsed, err := time.Parse(time.RFC3339Nano, value.UpdatedAt); err != nil || parsed.Format(time.RFC3339Nano) != value.UpdatedAt {
		return errors.New("history dialog metadata is invalid")
	}
	return nil
}

// ValidateHistorySearchEntry applies the narrower safe-content contract used
// by search and exact deep-link reads. History replica payloads remain opaque
// for storage, but R14 must never index or return native/provider objects.
func ValidateHistorySearchEntry(entry HistoryTranscriptEntry) error {
	if !validHistoryPayload(&entry) || !validHistorySearchContent(entry.Content, entry.Role) {
		return errors.New("history search entry is invalid")
	}
	return nil
}

func HistorySearchInlineText(entry HistoryTranscriptEntry) string {
	if ValidateHistorySearchEntry(entry) != nil {
		return ""
	}
	var content struct {
		Kind    string `json:"kind"`
		Content string `json:"content"`
	}
	if json.Unmarshal(entry.Content, &content) != nil || content.Kind != "inline" {
		return ""
	}
	return content.Content
}

func validHistorySearchContent(raw json.RawMessage, role string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return false
	}
	exact := func(names ...string) bool {
		if len(fields) != len(names) {
			return false
		}
		for _, name := range names {
			if _, ok := fields[name]; !ok {
				return false
			}
		}
		return true
	}
	var kind string
	if json.Unmarshal(fields["kind"], &kind) != nil {
		return false
	}
	if role == "user" {
		var content string
		return kind == "inline" && exact("kind", "content") && json.Unmarshal(fields["content"], &content) == nil &&
			utf8.ValidString(content) && len([]byte(content)) <= 64<<10 && !strings.ContainsRune(content, '\x00')
	}
	var redaction string
	var truncated bool
	if role != "assistant" || json.Unmarshal(fields["redaction"], &redaction) != nil ||
		json.Unmarshal(fields["truncated"], &truncated) != nil {
		return false
	}
	switch kind {
	case "inline":
		var content string
		return exact("kind", "content", "redaction", "truncated") && json.Unmarshal(fields["content"], &content) == nil &&
			utf8.ValidString(content) && len([]byte(content)) <= 64<<10 && !strings.ContainsRune(content, '\x00') &&
			(redaction == "none" || redaction == "applied")
	case "artifact":
		var artifactID, digest string
		var size int64
		return exact("kind", "artifactId", "sizeBytes", "sha256", "redaction", "truncated") &&
			json.Unmarshal(fields["artifactId"], &artifactID) == nil && ValidUUID(artifactID) &&
			json.Unmarshal(fields["sizeBytes"], &size) == nil && size >= 0 && size <= HistoryMaximumSafeInt &&
			json.Unmarshal(fields["sha256"], &digest) == nil && ValidSHA256(digest) &&
			(redaction == "none" || redaction == "applied")
	case "unavailable":
		var reason string
		if !exact("kind", "reason", "redaction", "truncated") || json.Unmarshal(fields["reason"], &reason) != nil {
			return false
		}
		validReason := reason == "not_observed" || reason == "provider_redacted" || reason == "output_limit" || reason == "unmapped"
		validRedaction := redaction == "none" || redaction == "applied" || redaction == "unknown"
		return validReason && validRedaction
	default:
		return false
	}
}
