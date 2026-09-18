// Package historysearch owns the versioned agent-search-v1 DTO shared by
// agent-service consumers. It does not contain storage or execution controls.
package historysearch

import "encoding/json"

const (
	SchemaID            = "agent-search-v1"
	SchemaSHA256        = "9be6ba5c42dbdf75b70009032b31a4ffb0cc3182321167ff316fca0b6e7155c6"
	MetadataSchemaID    = "agent-search-dialog-metadata-v1"
	EntryLookupSchemaID = "agent-history-entry-v1"
	MaximumPageSize     = 50
	MaximumQueryBytes   = 1000
	MaximumTitleBytes   = 1000
	MaximumSnippetBytes = 512
)

type Query struct {
	Q               string `json:"q"`
	NodeID          string `json:"nodeId,omitempty"`
	LogicalDialogID string `json:"logicalDialogId,omitempty"`
	Archived        *bool  `json:"archived,omitempty"`
}

type Item struct {
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

type Page struct {
	SchemaID   string  `json:"schemaId"`
	Query      Query   `json:"query"`
	Items      []Item  `json:"items"`
	NextCursor *string `json:"nextCursor"`
	TotalCount int64   `json:"totalCount"`
	ObservedAt string  `json:"observedAt"`
	LagMillis  int64   `json:"lagMillis"`
	Incomplete bool    `json:"incomplete"`
}

type DialogMetadata struct {
	SchemaID          string `json:"schemaId"`
	LogicalDialogID   string `json:"logicalDialogId,omitempty"`
	NodeID            string `json:"nodeId"`
	NodeDialogID      string `json:"nodeDialogId"`
	BindingGeneration int64  `json:"bindingGeneration"`
	Title             string `json:"title"`
	Archived          bool   `json:"archived"`
	UpdatedAt         string `json:"updatedAt"`
}

// SourceDialogMetadata is the bounded live-Harness projection fetched once per
// node sync cycle. Durable archive/retirement evidence remains owned by later
// lifecycle stages.
type SourceDialogMetadata struct {
	Title    string
	Archived bool
}

type EntryLookup struct {
	SchemaID   string          `json:"schemaId"`
	Entry      json.RawMessage `json:"entry"`
	ObservedAt string          `json:"observedAt"`
	LagMillis  int64           `json:"lagMillis"`
	Incomplete bool            `json:"incomplete"`
}
