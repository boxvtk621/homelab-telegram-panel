// Package importer decodes the operator-produced read-only migration snapshot.
// It has no Harness command, stream, lifecycle, or network dependency.
package importer

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/exactjson"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/registry"
)

const maximumSnapshotBytes = 4 << 20

func DecodeSnapshot(raw []byte, manifest model.RegistryManifest) (model.ImportSnapshot, error) {
	if len(raw) == 0 || len(raw) > maximumSnapshotBytes || !registry.UniqueJSON(raw) ||
		!exactjson.Shape(raw, model.ImportSnapshot{}) {
		return model.ImportSnapshot{}, errors.New("invalid import snapshot")
	}
	var snapshot model.ImportSnapshot
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&snapshot) != nil || decoder.Decode(new(any)) != io.EOF {
		return model.ImportSnapshot{}, errors.New("invalid import snapshot shape")
	}
	if err := model.ValidateSnapshot(manifest, snapshot); err != nil {
		return model.ImportSnapshot{}, err
	}
	return snapshot, nil
}
