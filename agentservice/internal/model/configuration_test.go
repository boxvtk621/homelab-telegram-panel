package model

import "testing"

func TestBuildContextManifestBinding(t *testing.T) {
	revision := "33333333-3333-4333-8333-333333333333"
	manifest := &BuildContextManifest{Revision: revision, Assets: []BuildContextAsset{{
		Path: "src/main.go", AssetID: "44444444-4444-4444-8444-444444444444", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}}
	managed := `{"deployment":{"kind":"managed","buildContextRevision":"` + revision + `"}}`
	if !ValidateBuildContextBinding(managed, manifest) {
		t.Fatal("matching managed manifest rejected")
	}
	for _, raw := range []string{
		`{"deployment":{"kind":"external","endpointRef":"harness/a"}}`,
		`{"deployment":{"kind":"managed","buildContextRevision":"55555555-5555-4555-8555-555555555555"}}`,
		`{"deployment":{"kind":"managed"}}`,
	} {
		if ValidateBuildContextBinding(raw, manifest) {
			t.Fatalf("unbound manifest accepted for %s", raw)
		}
	}
	if !ValidateBuildContextBinding(`{`, manifest) {
		t.Fatal("malformed raw draft with valid manifest must remain durable")
	}
	if !ValidateConfigurationSave(ConfigurationSave{SchemaID: ConfigurationSaveSchema, RawJSONText: `{"deployment":{"kind":"external"}}`, RawDockerfileText: "FROM scratch\n", BuildContextManifest: manifest}) {
		t.Fatal("semantic manifest mismatch must reach pure validation and remain durable")
	}
}

func TestBuildContextManifestRejectsUnsafeOrDuplicateAssets(t *testing.T) {
	base := BuildContextManifest{Revision: "33333333-3333-4333-8333-333333333333", Assets: []BuildContextAsset{{
		Path: "src/main.go", AssetID: "44444444-4444-4444-8444-444444444444", SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}}
	if !ValidateBuildContextManifest(&base) {
		t.Fatal("valid manifest rejected")
	}
	for _, path := range []string{"", "../secret", "/absolute", "src/", "src//main.go", "src/./x", "src/../x", "src/new\nline", "src/\x00file", `src\main.go`} {
		invalid := base
		invalid.Assets = append([]BuildContextAsset(nil), base.Assets...)
		invalid.Assets[0].Path = path
		if ValidateBuildContextManifest(&invalid) {
			t.Fatalf("unsafe path %q accepted", path)
		}
	}
	allowedDots := base
	allowedDots.Assets = append([]BuildContextAsset(nil), base.Assets...)
	allowedDots.Assets[0].Path = "src/foo..bar"
	if !ValidateBuildContextManifest(&allowedDots) {
		t.Fatal("portable path containing two dots inside a segment rejected")
	}
	dotfile := base
	dotfile.Assets = append([]BuildContextAsset(nil), base.Assets...)
	dotfile.Assets[0].Path = ".dockerignore"
	if !ValidateBuildContextManifest(&dotfile) {
		t.Fatal("safe root dotfile rejected")
	}
	duplicate := base
	duplicate.Assets = append(duplicate.Assets, duplicate.Assets[0])
	if ValidateBuildContextManifest(&duplicate) {
		t.Fatal("duplicate asset accepted")
	}
	reusedAsset := base
	reusedAsset.Assets = append(reusedAsset.Assets, BuildContextAsset{Path: "src/other.go", AssetID: base.Assets[0].AssetID, SHA256: base.Assets[0].SHA256})
	if !ValidateBuildContextManifest(&reusedAsset) {
		t.Fatal("one immutable asset mapped to distinct paths was rejected")
	}
	tooMany := base
	tooMany.Assets = make([]BuildContextAsset, 1001)
	if ValidateBuildContextManifest(&tooMany) {
		t.Fatal("1001 assets accepted")
	}
}
