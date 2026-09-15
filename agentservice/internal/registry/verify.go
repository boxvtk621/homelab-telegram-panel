// Package registry verifies the existing operator-signed registry without
// constructing transports or contacting Harness.
package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/exactjson"
	"github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model"
)

const (
	maximumRegistryBytes = 256 << 10
	maximumSignerBytes   = 16 << 10
	maximumSafeInteger   = int64(1<<53 - 1)
	routerRegistrySchema = "harness-router-registry-v1"
	harnessWireSHA256    = "5bd97f2ea08854a8e56d46ff11a1539e6bc54e8ca6d42841b366561accba73d9"
)

type legacyNode struct {
	NodeID            string `json:"nodeId"`
	Name              string `json:"name"`
	Adapter           string `json:"adapter"`
	URL               string `json:"url"`
	CertificateSHA256 string `json:"certificateSHA256"`
}

type projectedNode struct {
	NodeID               string `json:"nodeId"`
	Name                 string `json:"name"`
	Adapter              string `json:"adapter"`
	URL                  string `json:"url"`
	CertificateSHA256    string `json:"certificateSHA256"`
	RegistrationRevision int64  `json:"registrationRevision"`
	RegistrationEpoch    int64  `json:"registrationEpoch"`
	Compatibility        string `json:"compatibility"`
}

type legacyManifest struct {
	RegistryVersion int64        `json:"registryVersion"`
	OwnerID         string       `json:"ownerId"`
	Mode            string       `json:"mode"`
	Nodes           []legacyNode `json:"nodes"`
}

type projectedManifest struct {
	SchemaID         string          `json:"schemaId"`
	RegistryVersion  int64           `json:"registryVersion"`
	OwnerID          string          `json:"ownerId"`
	Mode             string          `json:"mode"`
	WireSchemaSHA256 string          `json:"wireSchemaSHA256"`
	Nodes            []projectedNode `json:"nodes"`
}

type signedLegacyManifest struct {
	Manifest  legacyManifest `json:"manifest"`
	Signature string         `json:"signature"`
}

type signedProjectedManifest struct {
	Manifest  projectedManifest `json:"manifest"`
	Signature string            `json:"signature"`
}

type Verified struct {
	Manifest       model.RegistryManifest
	ManifestSHA256 string
	Envelope       json.RawMessage
}

func Verify(raw, signerPEM []byte) (Verified, error) {
	if len(raw) == 0 || len(raw) > maximumRegistryBytes || !UniqueJSON(raw) {
		return Verified{}, errors.New("invalid signed registry")
	}
	signer, err := parseSigner(signerPEM)
	if err != nil {
		return Verified{}, err
	}

	var probe struct {
		Manifest struct {
			SchemaID string `json:"schemaId"`
		} `json:"manifest"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return Verified{}, errors.New("invalid signed registry shape")
	}
	var manifest model.RegistryManifest
	var canonical []byte
	var signatureText string
	if probe.Manifest.SchemaID == "" {
		var envelope signedLegacyManifest
		if !exactjson.Shape(raw, envelope) || decodeExact(raw, &envelope) != nil {
			return Verified{}, errors.New("invalid signed registry shape")
		}
		manifest = fromLegacy(envelope.Manifest)
		canonical, err = json.Marshal(envelope.Manifest)
		signatureText = envelope.Signature
	} else {
		var envelope signedProjectedManifest
		if !exactjson.Shape(raw, envelope) || decodeExact(raw, &envelope) != nil {
			return Verified{}, errors.New("invalid signed registry shape")
		}
		manifest = fromProjected(envelope.Manifest)
		canonical, err = json.Marshal(envelope.Manifest)
		signatureText = envelope.Signature
	}
	if err != nil {
		return Verified{}, errors.New("invalid signed registry")
	}
	if err := validateManifest(manifest); err != nil {
		return Verified{}, err
	}
	signature, err := base64.StdEncoding.DecodeString(signatureText)
	if err != nil || base64.StdEncoding.EncodeToString(signature) != signatureText ||
		!ed25519.Verify(signer, canonical, signature) {
		return Verified{}, errors.New("registry signature rejected")
	}
	digest := sha256.Sum256(canonical)
	return Verified{Manifest: manifest, ManifestSHA256: hex.EncodeToString(digest[:]), Envelope: append(json.RawMessage(nil), raw...)}, nil
}

func ValidateSigner(signerPEM []byte) error {
	_, err := parseSigner(signerPEM)
	return err
}

func parseSigner(signerPEM []byte) (ed25519.PublicKey, error) {
	if len(signerPEM) == 0 || len(signerPEM) > maximumSignerBytes {
		return nil, errors.New("invalid registry signer")
	}
	block, rest := pem.Decode(signerPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid registry signer")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid registry signer")
	}
	signer, ok := key.(ed25519.PublicKey)
	if !ok || len(signer) != ed25519.PublicKeySize {
		return nil, errors.New("registry signer is not Ed25519")
	}
	return signer, nil
}

// ProjectionDelta proves the same one-node/additive transition enforced by
// Router and identifies every existing node that must be reserved in Postgres.
// Legacy adoption reserves all existing nodes because it establishes their
// first signed registration revision/epoch.
func ProjectionDelta(current, candidate model.RegistryManifest) ([]string, string, error) {
	if candidate.SchemaID != routerRegistrySchema || candidate.WireSchemaSHA256 != harnessWireSHA256 ||
		candidate.OwnerID != current.OwnerID || candidate.Mode != current.Mode ||
		current.RegistryVersion >= maximumSafeInteger || candidate.RegistryVersion != current.RegistryVersion+1 ||
		len(candidate.Nodes) < len(current.Nodes) || len(candidate.Nodes) > len(current.Nodes)+1 {
		return nil, "", errors.New("invalid registry projection transition")
	}
	currentNodes := make(map[string]model.RegistryNode, len(current.Nodes))
	for _, node := range current.Nodes {
		currentNodes[node.NodeID] = node
	}
	affected := []string{}
	newNode := ""
	for _, next := range candidate.Nodes {
		prior, exists := currentNodes[next.NodeID]
		if !exists {
			if newNode != "" || next.RegistrationRevision != 1 || next.RegistrationEpoch != 1 {
				return nil, "", errors.New("invalid added registry node")
			}
			newNode = next.NodeID
			affected = append(affected, next.NodeID)
			continue
		}
		delete(currentNodes, next.NodeID)
		if current.SchemaID == "" {
			if prior.NodeID != next.NodeID || prior.Name != next.Name || prior.Adapter != next.Adapter || prior.URL != next.URL ||
				prior.CertificateSHA256 != next.CertificateSHA256 {
				return nil, "", errors.New("legacy adoption changed a node binding")
			}
			affected = append(affected, next.NodeID)
			continue
		}
		bindingChanged := prior.NodeID != next.NodeID || prior.Name != next.Name || prior.Adapter != next.Adapter ||
			prior.URL != next.URL || prior.CertificateSHA256 != next.CertificateSHA256 ||
			prior.Compatibility != next.Compatibility
		if !bindingChanged {
			if prior.RegistrationRevision != next.RegistrationRevision || prior.RegistrationEpoch != next.RegistrationEpoch {
				return nil, "", errors.New("untouched node registration changed")
			}
			continue
		}
		if prior.RegistrationRevision >= maximumSafeInteger || prior.RegistrationEpoch >= maximumSafeInteger ||
			next.RegistrationRevision != prior.RegistrationRevision+1 || next.RegistrationEpoch != prior.RegistrationEpoch+1 {
			return nil, "", errors.New("invalid node registration revision")
		}
		affected = append(affected, next.NodeID)
	}
	if len(currentNodes) != 0 || current.SchemaID != "" && len(affected) != 1 {
		return nil, "", errors.New("registry projection must change exactly one node")
	}
	if current.SchemaID == "" {
		expectedAffected := len(current.Nodes)
		if newNode != "" {
			expectedAffected++
		}
		if len(affected) != expectedAffected {
			return nil, "", errors.New("incomplete legacy registry adoption")
		}
	}
	return affected, newNode, nil
}

func decodeExact(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid signed registry shape")
	}
	return nil
}

func fromLegacy(value legacyManifest) model.RegistryManifest {
	manifest := model.RegistryManifest{
		RegistryVersion: value.RegistryVersion, OwnerID: value.OwnerID, Mode: value.Mode,
		Nodes: make([]model.RegistryNode, len(value.Nodes)),
	}
	for index, node := range value.Nodes {
		manifest.Nodes[index] = model.RegistryNode{
			NodeID: node.NodeID, Name: node.Name, Adapter: node.Adapter,
			URL: node.URL, CertificateSHA256: node.CertificateSHA256,
		}
	}
	return manifest
}

func fromProjected(value projectedManifest) model.RegistryManifest {
	manifest := model.RegistryManifest{
		SchemaID: value.SchemaID, RegistryVersion: value.RegistryVersion, OwnerID: value.OwnerID,
		Mode: value.Mode, WireSchemaSHA256: value.WireSchemaSHA256,
		Nodes: make([]model.RegistryNode, len(value.Nodes)),
	}
	for index, node := range value.Nodes {
		manifest.Nodes[index] = model.RegistryNode{
			NodeID: node.NodeID, Name: node.Name, Adapter: node.Adapter, URL: node.URL,
			CertificateSHA256: node.CertificateSHA256, RegistrationRevision: node.RegistrationRevision,
			RegistrationEpoch: node.RegistrationEpoch, Compatibility: node.Compatibility,
		}
	}
	return manifest
}

func validateManifest(manifest model.RegistryManifest) error {
	dynamic := manifest.SchemaID == routerRegistrySchema
	if manifest.RegistryVersion < 1 || manifest.RegistryVersion > maximumSafeInteger ||
		!model.ValidActor(manifest.OwnerID) ||
		(manifest.Mode != "live" && manifest.Mode != "fixture") ||
		manifest.Nodes == nil || dynamic && (len(manifest.Nodes) == 0 || len(manifest.Nodes) > 1000) ||
		(dynamic && manifest.WireSchemaSHA256 != harnessWireSHA256) ||
		(!dynamic && (manifest.SchemaID != "" || manifest.WireSchemaSHA256 != "")) {
		return errors.New("invalid registry identity")
	}
	seenNodes := map[string]bool{}
	seenCertificates := map[string]bool{}
	for _, node := range manifest.Nodes {
		endpoint, err := url.Parse(node.URL)
		certificate, certificateErr := hex.DecodeString(node.CertificateSHA256)
		if !model.ValidUUID(node.NodeID) || seenNodes[node.NodeID] ||
			node.Name == "" || !utf8.ValidString(node.Name) || len(node.Name) > 200 ||
			strings.ContainsAny(node.Name, "\x00\r\n") ||
			(node.Adapter != "cursor" && node.Adapter != "codex") ||
			err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" ||
			endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
			endpoint.Fragment != "" || endpoint.RawPath != "" || endpoint.Path != "" ||
			certificateErr != nil || len(certificate) != sha256.Size ||
			strings.ToLower(node.CertificateSHA256) != node.CertificateSHA256 ||
			(dynamic && (node.RegistrationRevision < 1 || node.RegistrationRevision > maximumSafeInteger ||
				node.RegistrationEpoch < 1 || node.RegistrationEpoch > maximumSafeInteger ||
				(node.Compatibility != "compatible" && node.Compatibility != "legacy_readonly"))) ||
			(!dynamic && (node.RegistrationRevision != 0 || node.RegistrationEpoch != 0 || node.Compatibility != "")) ||
			seenCertificates[node.CertificateSHA256] {
			return errors.New("invalid registry node binding")
		}
		seenNodes[node.NodeID] = true
		seenCertificates[node.CertificateSHA256] = true
	}
	return nil
}

// UniqueJSON rejects duplicate object keys before typed decoding.
func UniqueJSON(raw []byte) bool {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if !uniqueValue(decoder) {
		return false
	}
	return decoder.Decode(new(any)) == io.EOF
}

func uniqueValue(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return true
	}
	switch delimiter {
	case '{':
		keys := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || keys[name] {
				return false
			}
			keys[name] = true
			if !uniqueValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for decoder.More() {
			if !uniqueValue(decoder) {
				return false
			}
		}
		end, err := decoder.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}
