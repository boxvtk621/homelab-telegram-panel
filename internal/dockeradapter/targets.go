package dockeradapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/boxvtk621/homelab-telegram-panel/internal/strictjson"
)

type FileTargets struct {
	path string
}

type targetManifest struct {
	SchemaID string                       `json:"schemaId"`
	Owners   map[string]map[string]Target `json:"owners"`
}

func NewFileTargets(path string) (*FileTargets, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 512 {
		return nil, errors.New("invalid target manifest path")
	}
	resolver := &FileTargets{path: path}
	if _, err := resolver.load(); err != nil {
		return nil, err
	}
	return resolver, nil
}

func (f *FileTargets) ResolveTarget(ctx context.Context, owner, ref string) (Target, error) {
	if ctx.Err() != nil || !refPattern.MatchString(owner) || !refPattern.MatchString(ref) {
		return Target{}, errors.New("target unavailable")
	}
	manifest, err := f.load()
	if err != nil {
		return Target{}, errors.New("target unavailable")
	}
	ownerTargets, ok := manifest.Owners[owner]
	if !ok {
		return Target{}, errors.New("target unavailable")
	}
	target, ok := ownerTargets[ref]
	if !ok || target.Ref != ref || validateTarget(target) != nil {
		return Target{}, errors.New("target unavailable")
	}
	return target, nil
}

func (f *FileTargets) load() (targetManifest, error) {
	info, err := os.Lstat(f.path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > 1<<20 {
		return targetManifest{}, errors.New("private target manifest required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return targetManifest{}, errors.New("private target manifest required")
	}
	file, err := os.OpenFile(f.path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return targetManifest{}, errors.New("private target manifest required")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return targetManifest{}, errors.New("private target manifest required")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 || !strictjson.Valid(raw) {
		return targetManifest{}, errors.New("invalid target manifest")
	}
	var manifest targetManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&manifest) != nil || decoder.Decode(new(any)) != io.EOF || manifest.SchemaID != "docker-adapter-targets-v1" || len(manifest.Owners) == 0 {
		return targetManifest{}, errors.New("invalid target manifest")
	}
	for owner, targets := range manifest.Owners {
		if !refPattern.MatchString(owner) || len(targets) == 0 {
			return targetManifest{}, errors.New("invalid target manifest")
		}
		for ref, target := range targets {
			if ref != target.Ref || validateTarget(target) != nil {
				return targetManifest{}, errors.New("invalid target manifest")
			}
		}
	}
	return manifest, nil
}
