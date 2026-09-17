package model

import (
	"encoding/json"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/configdraft"
)

const (
	ConfigurationDraftSchema    = "agent-configuration-draft-v2"
	ConfigurationValidateSchema = "agent-configuration-validate-v2"
	ConfigurationSaveSchema     = "agent-configuration-save-v2"
)

type ConfigurationInput struct {
	SchemaID             string                `json:"schemaId"`
	RawJSONText          string                `json:"rawJsonText"`
	RawDockerfileText    string                `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest `json:"buildContextManifest,omitempty"`
}

type ConfigurationSave struct {
	SchemaID             string                `json:"schemaId"`
	ExpectedDraftVersion int64                 `json:"expectedDraftVersion"`
	RawJSONText          string                `json:"rawJsonText"`
	RawDockerfileText    string                `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest `json:"buildContextManifest,omitempty"`
}

type BuildContextAsset struct {
	Path    string `json:"path"`
	AssetID string `json:"assetId"`
	SHA256  string `json:"sha256"`
}
type BuildContextManifest struct {
	Revision string              `json:"revision"`
	Assets   []BuildContextAsset `json:"assets"`
}

type ConfigurationDraft struct {
	SchemaID             string                 `json:"schemaId"`
	NodeID               string                 `json:"nodeId"`
	DraftVersion         int64                  `json:"draftVersion"`
	RawJSONText          string                 `json:"rawJsonText"`
	RawDockerfileText    string                 `json:"rawDockerfileText"`
	BuildContextManifest *BuildContextManifest  `json:"buildContextManifest,omitempty"`
	Validation           configdraft.Validation `json:"validation"`
}

type ConfigurationValidation struct {
	SchemaID             string                 `json:"schemaId"`
	BuildContextManifest *BuildContextManifest  `json:"buildContextManifest,omitempty"`
	Validation           configdraft.Validation `json:"validation"`
}

func ValidateConfigurationInput(input ConfigurationInput) bool {
	return input.SchemaID == ConfigurationValidateSchema && len(input.RawJSONText) <= configdraft.JSONLimit && len(input.RawDockerfileText) <= configdraft.DockerfileLimit && ValidateBuildContextManifest(input.BuildContextManifest)
}

func ValidateConfigurationSave(input ConfigurationSave) bool {
	return input.SchemaID == ConfigurationSaveSchema && input.ExpectedDraftVersion >= 0 && input.ExpectedDraftVersion <= MaximumSafeInt && len(input.RawJSONText) <= configdraft.JSONLimit && len(input.RawDockerfileText) <= configdraft.DockerfileLimit && ValidateBuildContextManifest(input.BuildContextManifest)
}

func ValidateBuildContextManifest(manifest *BuildContextManifest) bool {
	if manifest == nil {
		return true
	}
	if !ValidUUID(manifest.Revision) || manifest.Assets == nil || len(manifest.Assets) > 1000 {
		return false
	}
	paths := map[string]bool{}
	for _, asset := range manifest.Assets {
		if !validBuildContextPath(asset.Path) || !ValidUUID(asset.AssetID) || !sha256Pattern.MatchString(asset.SHA256) || paths[asset.Path] {
			return false
		}
		paths[asset.Path] = true
	}
	return true
}

func validBuildContextPath(value string) bool {
	if len(value) < 1 || len(value) > 256 || !utf8.ValidString(value) || value[0] == '/' || strings.Contains(value, "\\") || path.Clean(value) != value {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-') {
				return false
			}
		}
	}
	return true
}

func ValidateBuildContextBinding(rawJSON string, manifest *BuildContextManifest) bool {
	if manifest == nil {
		return true
	}
	var configuration struct {
		Deployment struct {
			Kind                 string `json:"kind"`
			BuildContextRevision string `json:"buildContextRevision"`
		} `json:"deployment"`
	}
	if json.Unmarshal([]byte(rawJSON), &configuration) != nil {
		return true
	}
	switch configuration.Deployment.Kind {
	case "managed":
		return configuration.Deployment.BuildContextRevision == manifest.Revision
	case "external":
		return false
	default:
		return true
	}
}
