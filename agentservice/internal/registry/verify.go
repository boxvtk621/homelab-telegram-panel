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
)

type signedManifest struct {
	Manifest  model.RegistryManifest `json:"manifest"`
	Signature string                 `json:"signature"`
}

type Verified struct {
	Manifest       model.RegistryManifest
	ManifestSHA256 string
}

func Verify(raw, signerPEM []byte) (Verified, error) {
	if len(raw) == 0 || len(raw) > maximumRegistryBytes || len(signerPEM) == 0 ||
		len(signerPEM) > maximumSignerBytes || !UniqueJSON(raw) ||
		!exactjson.Shape(raw, signedManifest{}) {
		return Verified{}, errors.New("invalid signed registry")
	}
	block, rest := pem.Decode(signerPEM)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return Verified{}, errors.New("invalid registry signer")
	}
	key, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return Verified{}, errors.New("invalid registry signer")
	}
	signer, ok := key.(ed25519.PublicKey)
	if !ok || len(signer) != ed25519.PublicKeySize {
		return Verified{}, errors.New("registry signer is not Ed25519")
	}

	var envelope signedManifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF {
		return Verified{}, errors.New("invalid signed registry shape")
	}
	if err := validateManifest(envelope.Manifest); err != nil {
		return Verified{}, err
	}
	canonical, err := json.Marshal(envelope.Manifest)
	if err != nil {
		return Verified{}, errors.New("invalid signed registry")
	}
	signature, err := base64.StdEncoding.DecodeString(envelope.Signature)
	if err != nil || base64.StdEncoding.EncodeToString(signature) != envelope.Signature ||
		!ed25519.Verify(signer, canonical, signature) {
		return Verified{}, errors.New("registry signature rejected")
	}
	digest := sha256.Sum256(canonical)
	return Verified{Manifest: envelope.Manifest, ManifestSHA256: hex.EncodeToString(digest[:])}, nil
}

func validateManifest(manifest model.RegistryManifest) error {
	if manifest.RegistryVersion < 1 || manifest.RegistryVersion > maximumSafeInteger ||
		!model.ValidActor(manifest.OwnerID) ||
		(manifest.Mode != "live" && manifest.Mode != "fixture") ||
		manifest.Nodes == nil {
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
