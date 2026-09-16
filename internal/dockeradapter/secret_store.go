package dockeradapter

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const maximumSecretBytes = 64 << 10

var ErrSecretUnavailable = errors.New("credential unavailable")
var ErrSecretConflict = errors.New("credential provisioning operation conflict")
var ErrSecretNotFound = errors.New("credential provisioning operation not found")

type SecretStore struct {
	directory     string
	key           []byte
	lock          *os.File
	syncDirectory func() error
	mu            sync.RWMutex
	closed        bool
}

type secretEnvelope struct {
	SchemaID    string `json:"schemaId"`
	Ref         string `json:"ref"`
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	Nonce       string `json:"nonce"`
	Ciphertext  string `json:"ciphertext"`
}

type sealedSecret struct {
	OperationID string `json:"operationId"`
	Kind        string `json:"kind"`
	RequestHMAC string `json:"requestHMAC"`
	Payload     []byte `json:"payload"`
}

type SecretProvision struct {
	SchemaID      string `json:"schemaId"`
	OperationID   string `json:"operationId"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	CredentialRef string `json:"credentialRef"`
	Created       bool   `json:"-"`
}

type sshSecretPayload struct {
	PrivateKey []byte `json:"privateKey"`
	Passphrase []byte `json:"passphrase,omitempty"`
}

// RegistryCredentialProbe verifies only that the owner-scoped opaque
// credential can be decrypted by the adapter. R07 deliberately does not claim
// registry authentication without a selected registry operation.
type RegistryCredentialProbe struct {
	Store interface {
		ResolveRegistry(context.Context, string, string) ([]byte, error)
	}
}

func (p RegistryCredentialProbe) Check(ctx context.Context, owner, ref string) *probeFault {
	if p.Store == nil {
		return &probeFault{stage: "registry_auth", code: "registry_credential_unavailable", next: "Сохраните registry credentials повторно."}
	}
	payload, err := p.Store.ResolveRegistry(ctx, owner, ref)
	if err != nil || len(payload) == 0 {
		zeroBytes(payload)
		return &probeFault{stage: "registry_auth", code: "registry_credential_unavailable", next: "Проверьте owner-scoped registry credential ref."}
	}
	zeroBytes(payload)
	return nil
}

func NewSecretStore(directory string, masterKey []byte) (*SecretStore, error) {
	if len(masterKey) != 32 || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("invalid credential store configuration")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return nil, errors.New("private credential directory required")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, errors.New("private credential directory required")
	}
	lockPath := filepath.Join(directory, ".adapter.lock")
	lock, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, errors.New("private credential directory required")
	}
	lockInfo, err := lock.Stat()
	if err != nil || !lockInfo.Mode().IsRegular() || lockInfo.Mode().Perm() != 0o600 {
		_ = lock.Close()
		return nil, errors.New("private credential directory required")
	}
	lockStat, ok := lockInfo.Sys().(*syscall.Stat_t)
	if !ok || int(lockStat.Uid) != os.Geteuid() || syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		_ = lock.Close()
		return nil, errors.New("credential store already active")
	}
	store := &SecretStore{directory: directory, key: append([]byte(nil), masterKey...), lock: lock}
	store.syncDirectory = store.syncCredentialDirectory
	return store, nil
}

func (s *SecretStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	zeroBytes(s.key)
	s.key = nil
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
	s.closed = true
}

func (s *SecretStore) ProvisionSSH(ctx context.Context, owner, operationID string, privateKey, passphrase []byte) (SecretProvision, error) {
	if len(privateKey) == 0 || len(privateKey) > maximumSecretBytes || len(passphrase) > 4<<10 {
		return SecretProvision{}, ErrSecretUnavailable
	}
	payload, err := json.Marshal(sshSecretPayload{PrivateKey: privateKey, Passphrase: passphrase})
	if err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	defer zeroBytes(payload)
	return s.provision(ctx, owner, operationID, "ssh", payload)
}

func (s *SecretStore) ProvisionRegistry(ctx context.Context, owner, operationID string, payload []byte) (SecretProvision, error) {
	if len(payload) == 0 || len(payload) > maximumSecretBytes {
		return SecretProvision{}, ErrSecretUnavailable
	}
	return s.provision(ctx, owner, operationID, "registry", payload)
}

func (s *SecretStore) provision(ctx context.Context, owner, operationID, kind string, payload []byte) (SecretProvision, error) {
	if ctx.Err() != nil || !refPattern.MatchString(owner) || !hostUUIDPattern.MatchString(operationID) || (kind != "ssh" && kind != "registry") {
		return SecretProvision{}, ErrSecretUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.key) != 32 {
		return SecretProvision{}, ErrSecretUnavailable
	}
	ref := s.secretRef(owner, operationID)
	if _, err := os.Lstat(filepath.Join(s.directory, ref+".json")); err == nil {
		if s.syncDirectory == nil || s.syncDirectory() != nil {
			return SecretProvision{}, ErrSecretUnavailable
		}
		existing, sealed, err := s.openLocked(ctx, owner, operationID, ref)
		if err != nil {
			return SecretProvision{}, ErrSecretUnavailable
		}
		defer zeroBytes(sealed.Payload)
		requestHMAC := s.requestHMAC(owner, operationID, kind, payload)
		defer zeroBytes(requestHMAC)
		storedHMAC, decodeErr := hex.DecodeString(sealed.RequestHMAC)
		defer zeroBytes(storedHMAC)
		if decodeErr != nil || existing.Kind != kind || sealed.Kind != kind ||
			subtle.ConstantTimeCompare(storedHMAC, requestHMAC) != 1 || subtle.ConstantTimeCompare(sealed.Payload, payload) != 1 {
			return SecretProvision{}, ErrSecretConflict
		}
		return existing, nil
	} else if !os.IsNotExist(err) {
		return SecretProvision{}, ErrSecretUnavailable
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	requestHMAC := s.requestHMAC(owner, operationID, kind, payload)
	sealedRaw, err := json.Marshal(sealedSecret{
		OperationID: operationID, Kind: kind, RequestHMAC: hex.EncodeToString(requestHMAC), Payload: payload,
	})
	zeroBytes(requestHMAC)
	if err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	defer zeroBytes(sealedRaw)
	aad := secretAAD(owner, operationID, ref, kind)
	ciphertext := gcm.Seal(nil, nonce, sealedRaw, aad)
	envelope := secretEnvelope{
		SchemaID: "docker-adapter-secret-v1", Ref: ref, OperationID: operationID, Kind: kind,
		Nonce: hex.EncodeToString(nonce), Ciphertext: hex.EncodeToString(ciphertext),
	}
	raw, err := json.Marshal(envelope)
	zeroBytes(ciphertext)
	if err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	defer zeroBytes(raw)
	if err := s.write(ref, raw); err != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	return secretProvision(operationID, kind, ref, true), nil
}

func (s *SecretStore) ProvisionStatus(ctx context.Context, owner, operationID string) (SecretProvision, error) {
	if ctx.Err() != nil || !refPattern.MatchString(owner) || !hostUUIDPattern.MatchString(operationID) {
		return SecretProvision{}, ErrSecretNotFound
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || len(s.key) != 32 {
		return SecretProvision{}, ErrSecretUnavailable
	}
	ref := s.secretRef(owner, operationID)
	if s.syncDirectory == nil || s.syncDirectory() != nil {
		return SecretProvision{}, ErrSecretUnavailable
	}
	provision, sealed, err := s.openLocked(ctx, owner, operationID, ref)
	if err != nil {
		if os.IsNotExist(err) {
			return SecretProvision{}, ErrSecretNotFound
		}
		return SecretProvision{}, ErrSecretUnavailable
	}
	zeroBytes(sealed.Payload)
	return provision, nil
}

func (s *SecretStore) ResolveSSH(ctx context.Context, owner, ref string) (SSHCredential, error) {
	payload, err := s.resolve(ctx, owner, ref, "ssh")
	if err != nil {
		return SSHCredential{}, err
	}
	defer zeroBytes(payload)
	var secret sshSecretPayload
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&secret) != nil || decoder.Decode(new(any)) != io.EOF ||
		len(secret.PrivateKey) == 0 || len(secret.PrivateKey) > maximumSecretBytes || len(secret.Passphrase) > 4<<10 {
		zeroBytes(secret.PrivateKey)
		zeroBytes(secret.Passphrase)
		return SSHCredential{}, ErrSecretUnavailable
	}
	return SSHCredential{PrivateKey: secret.PrivateKey, Passphrase: secret.Passphrase}, nil
}

func (s *SecretStore) ResolveRegistry(ctx context.Context, owner, ref string) ([]byte, error) {
	return s.resolve(ctx, owner, ref, "registry")
}

func (s *SecretStore) resolve(ctx context.Context, owner, ref, kind string) ([]byte, error) {
	if ctx.Err() != nil || !refPattern.MatchString(owner) || !refPattern.MatchString(ref) {
		return nil, ErrSecretUnavailable
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed || len(s.key) != 32 {
		return nil, ErrSecretUnavailable
	}
	provision, sealed, err := s.openLocked(ctx, owner, "", ref)
	if err != nil || provision.Kind != kind || sealed.Kind != kind {
		zeroBytes(sealed.Payload)
		return nil, ErrSecretUnavailable
	}
	return sealed.Payload, nil
}

func (s *SecretStore) openLocked(ctx context.Context, owner, expectedOperationID, ref string) (SecretProvision, sealedSecret, error) {
	if ctx.Err() != nil {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	raw, err := s.read(ref)
	if err != nil {
		return SecretProvision{}, sealedSecret{}, err
	}
	defer zeroBytes(raw)
	var envelope secretEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.SchemaID != "docker-adapter-secret-v1" ||
		envelope.Ref != ref || !hostUUIDPattern.MatchString(envelope.OperationID) ||
		(expectedOperationID != "" && envelope.OperationID != expectedOperationID) ||
		(envelope.Kind != "ssh" && envelope.Kind != "registry") || s.secretRef(owner, envelope.OperationID) != ref {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	nonce, err := hex.DecodeString(envelope.Nonce)
	if err != nil {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	defer zeroBytes(nonce)
	ciphertext, err := hex.DecodeString(envelope.Ciphertext)
	if err != nil {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	defer zeroBytes(ciphertext)
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(nonce) != gcm.NonceSize() {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	sealedRaw, err := gcm.Open(nil, nonce, ciphertext, secretAAD(owner, envelope.OperationID, ref, envelope.Kind))
	if err != nil {
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	defer zeroBytes(sealedRaw)
	var sealed sealedSecret
	sealedDecoder := json.NewDecoder(bytes.NewReader(sealedRaw))
	sealedDecoder.DisallowUnknownFields()
	if sealedDecoder.Decode(&sealed) != nil || sealedDecoder.Decode(new(any)) != io.EOF ||
		sealed.OperationID != envelope.OperationID || sealed.Kind != envelope.Kind || len(sealed.Payload) == 0 || len(sealed.Payload) > 2*maximumSecretBytes {
		zeroBytes(sealed.Payload)
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	wantHMAC := s.requestHMAC(owner, envelope.OperationID, envelope.Kind, sealed.Payload)
	gotHMAC, err := hex.DecodeString(sealed.RequestHMAC)
	valid := err == nil && subtle.ConstantTimeCompare(gotHMAC, wantHMAC) == 1
	zeroBytes(gotHMAC)
	zeroBytes(wantHMAC)
	if !valid {
		zeroBytes(sealed.Payload)
		return SecretProvision{}, sealedSecret{}, ErrSecretUnavailable
	}
	return secretProvision(envelope.OperationID, envelope.Kind, ref, false), sealed, nil
}

func (s *SecretStore) secretRef(owner, operationID string) string {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("hl263-adapter-secret-ref/v1\x00" + owner + "\x00" + operationID))
	digest := mac.Sum(nil)
	defer zeroBytes(digest)
	return "cred_" + hex.EncodeToString(digest[:16])
}

func (s *SecretStore) requestHMAC(owner, operationID, kind string, payload []byte) []byte {
	mac := hmac.New(sha256.New, s.key)
	_, _ = mac.Write([]byte("hl263-adapter-secret-request/v1\x00" + owner + "\x00" + operationID + "\x00" + kind + "\x00"))
	_, _ = mac.Write(payload)
	return mac.Sum(nil)
}

func secretAAD(owner, operationID, ref, kind string) []byte {
	return []byte("hl263-adapter-secret/v1\x00" + owner + "\x00" + operationID + "\x00" + ref + "\x00" + kind)
}

func secretProvision(operationID, kind, ref string, created bool) SecretProvision {
	return SecretProvision{SchemaID: "docker-secret-provision-v1", OperationID: operationID, Kind: kind, Status: "provisioned", CredentialRef: ref, Created: created}
}

func (s *SecretStore) write(ref string, raw []byte) error {
	temporary := filepath.Join(s.directory, "."+ref+".tmp")
	final := filepath.Join(s.directory, ref+".json")
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if os.IsExist(err) {
		info, statErr := os.Lstat(temporary)
		stat, statOK := infoSyscallStat(info)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 ||
			!statOK || int(stat.Uid) != os.Geteuid() || os.Remove(temporary) != nil {
			return ErrSecretUnavailable
		}
		file, err = os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	}
	if err != nil {
		return err
	}
	remove := true
	defer func() {
		_ = file.Close()
		if remove {
			_ = os.Remove(temporary)
		}
	}()
	if _, err = file.Write(raw); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Link(temporary, final); err != nil {
		return err
	}
	if err = os.Remove(temporary); err != nil {
		return err
	}
	remove = false
	if s.syncDirectory == nil {
		return ErrSecretUnavailable
	}
	return s.syncDirectory()
}

func (s *SecretStore) syncCredentialDirectory() error {
	directory, err := os.Open(s.directory)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func infoSyscallStat(info os.FileInfo) (*syscall.Stat_t, bool) {
	if info == nil {
		return nil, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return stat, ok
}

func (s *SecretStore) read(ref string) ([]byte, error) {
	path := filepath.Join(s.directory, ref+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || info.Size() <= 0 || info.Size() > 256<<10 {
		return nil, ErrSecretUnavailable
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return nil, ErrSecretUnavailable
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrSecretUnavailable
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, ErrSecretUnavailable
	}
	return io.ReadAll(io.LimitReader(file, 256<<10))
}
