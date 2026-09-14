package harnessrouter

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

type registryTrust struct {
	public  ed25519.PublicKey
	private ed25519.PrivateKey
	roots   *x509.CertPool
	client  tls.Certificate
}

func newRegistryTrust(t *testing.T) registryTrust {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, public, private)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	clientPublic, clientPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	client := &x509.Certificate{
		SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	clientDER, err := x509.CreateCertificate(rand.Reader, client, parsed, clientPublic, private)
	if err != nil {
		t.Fatal(err)
	}
	return registryTrust{public: public, private: private, roots: roots, client: tls.Certificate{Certificate: [][]byte{clientDER}, PrivateKey: clientPrivate}}
}

func signedRegistry(t *testing.T, manifest harnessclient.Manifest, key ed25519.PrivateKey) []byte {
	t.Helper()
	canonical, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(harnessclient.SignedManifest{
		Manifest: manifest, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(key, canonical)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func registryNode(index int, adapter string) harnessclient.Node {
	pin := sha256.Sum256([]byte("registry-node-" + string(rune('0'+index))))
	return harnessclient.Node{
		NodeID: "20000000-0000-4000-8000-00000000000" + string(rune('0'+index)),
		Name:   "Agent " + string(rune('0'+index)), Adapter: adapter,
		URL: "https://node-" + string(rune('0'+index)) + ".invalid:9443", CertificateSHA256: hex.EncodeToString(pin[:]),
	}
}

func registryClient(t *testing.T, trust registryTrust, manifest harnessclient.Manifest) (*harnessclient.Client, []byte) {
	t.Helper()
	raw := signedRegistry(t, manifest, trust.private)
	client, err := harnessclient.New(raw, trust.public, trust.roots, trust.client)
	if err != nil {
		t.Fatal(err)
	}
	return client, raw
}

func legacyManifest(nodes ...harnessclient.Node) harnessclient.Manifest {
	return harnessclient.Manifest{
		RegistryVersion: 1, OwnerID: "owner-1", Mode: "fixture",
		Nodes: append([]harnessclient.Node{}, nodes...),
	}
}

func dynamicManifest(version int64, nodes ...harnessclient.Node) harnessclient.Manifest {
	return harnessclient.Manifest{
		SchemaID: harnessclient.RouterRegistrySchemaID, RegistryVersion: version, OwnerID: "owner-1", Mode: "fixture",
		WireSchemaSHA256: hp.SchemaSHA256, Nodes: nodes,
	}
}

func managedRegistryRouter(t *testing.T, backend Backend, state State, factory registryFactory) (*Router, string, string) {
	t.Helper()
	directory, err := os.MkdirTemp("/private/tmp", "hl283-router-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(directory, "router-state.json")
	lock, err := acquireStateLock(directory, true)
	if err != nil {
		t.Fatal(err)
	}
	lock.close()
	if _, err := writeState(statePath, state, false); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "router.sock")
	router, err := newManaged(backend, statePath, socketPath)
	if err != nil {
		t.Fatal(err)
	}
	router.registryFactory = factory
	return router, statePath, socketPath
}

type closeTrackingBackend struct {
	Backend
	closed atomic.Int32
}

func (backend *closeTrackingBackend) Close() {
	backend.closed.Add(1)
	backend.Backend.Close()
}

func requireRegistryFault(t *testing.T, err error, code string) {
	t.Helper()
	var fault *controlFault
	if !errors.As(err, &fault) || fault.code != code {
		t.Fatalf("registry fault=%v, want %s", err, code)
	}
}

func TestRegistryInstallRejectsInvalidSignatureOwnerAndStaleExpectedState(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	current := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256,
		Nodes: map[string]NodeState{
			first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"},
		},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, _ := managedRegistryRouter(t, legacy, state, factory)
	defer router.Close()
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	projection := dynamicManifest(2, first, second)
	valid := registryInstallRequest{
		OperationID: "registry-validation", Expected: registryExpected{RegistryVersion: 1, RegistrySHA256: current.ManifestSHA256},
		Registry: signedRegistry(t, projection, trust.private),
	}

	_, foreignPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	invalidSignature := valid
	invalidSignature.Registry = signedRegistry(t, projection, foreignPrivate)
	_, err = router.installRegistry(invalidSignature)
	requireRegistryFault(t, err, "registry_rejected")

	wrongOwnerManifest := projection
	wrongOwnerManifest.OwnerID = "owner-2"
	wrongOwner := valid
	wrongOwner.Registry = signedRegistry(t, wrongOwnerManifest, trust.private)
	_, err = router.installRegistry(wrongOwner)
	requireRegistryFault(t, err, "stale")

	stale := valid
	stale.Expected.RegistryVersion = 2
	_, err = router.installRegistry(stale)
	requireRegistryFault(t, err, "stale")
	if router.snapshot().RegistryVersion != current.RegistryVersion || len(router.snapshot().Nodes) != 1 {
		t.Fatal("rejected registry changed Router state")
	}
}

func TestRegistryInstallBootstrapsFirstNodeFromEmptyLegacyRegistry(t *testing.T) {
	trust := newRegistryTrust(t)
	legacy, _ := registryClient(t, trust, legacyManifest())
	current := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256, Nodes: map[string]NodeState{},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, _ := managedRegistryRouter(t, legacy, state, factory)
	defer router.Close()
	first := registryNode(1, "cursor")
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 1, "compatible"
	input := registryInstallRequest{
		OperationID: "registry-first-node",
		Expected:    registryExpected{RegistryVersion: 1, RegistrySHA256: current.ManifestSHA256},
		Registry:    signedRegistry(t, dynamicManifest(2, first), trust.private),
	}
	result, err := router.installRegistry(input)
	if err != nil {
		t.Fatal(err)
	}
	node := result.Nodes[first.NodeID]
	if result.RegistryVersion != 2 || len(result.Nodes) != 1 || node.Mode != ModeSealed ||
		node.OperationID != input.OperationID || node.RegistrationRevision != 1 || node.IdentityEpoch != 1 {
		t.Fatalf("invalid first-node projection: %+v", result)
	}
}

func TestProjectNodesRejectsRevisionOnlyBumpAndNonInitialNodeEpoch(t *testing.T) {
	firstID := "20000000-0000-4000-8000-000000000001"
	current := harnessclient.RoutingRegistry{
		SchemaID: harnessclient.RouterRegistrySchemaID, RegistryVersion: 2, OwnerID: "owner-1", Mode: "fixture",
		WireSchemaSHA256: hp.SchemaSHA256,
		Nodes: []harnessclient.RoutingNode{{
			NodeID: firstID, Name: "Agent", Adapter: "cursor", RegistrationRevision: 1,
			RegistrationEpoch: 1, Compatibility: "compatible", BindingSHA256: strings.Repeat("a", 64),
		}},
	}
	router := &Router{nodes: map[string]*nodeRoute{
		firstID: {state: NodeState{
			Mode: ModeEligible, StateVersion: 1, IdentityEpoch: 1, RegistrationRevision: 1,
			Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "1.2.3",
		}},
	}}
	revisionOnly := current
	revisionOnly.RegistryVersion = 3
	revisionOnly.Nodes = append([]harnessclient.RoutingNode(nil), current.Nodes...)
	revisionOnly.Nodes[0].RegistrationRevision = 2
	revisionOnly.Nodes[0].RegistrationEpoch = 2
	if _, _, _, err := router.projectNodes(current, revisionOnly, "registry-revision-only"); err == nil {
		t.Fatal("revision-only node bump was accepted")
	}
	badAddition := current
	badAddition.RegistryVersion = 3
	badAddition.Nodes = append([]harnessclient.RoutingNode(nil), current.Nodes...)
	badAddition.Nodes = append(badAddition.Nodes, harnessclient.RoutingNode{
		NodeID: "20000000-0000-4000-8000-000000000002", Name: "New", Adapter: "codex",
		RegistrationRevision: 1, RegistrationEpoch: 2, Compatibility: "compatible", BindingSHA256: strings.Repeat("b", 64),
	})
	if _, _, _, err := router.projectNodes(current, badAddition, "registry-bad-add"); err == nil {
		t.Fatal("new node with a non-initial epoch was accepted")
	}
}

func TestRegistryInstallAddsOneNodeAndPreservesUntouchedFenceAcrossRestart(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	tracked := &closeTrackingBackend{Backend: legacy}
	legacyRouting := legacy.RoutingRegistry()
	untouched := NodeState{Mode: ModeEligible, StateVersion: 9, Generation: 4, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"}
	state := State{
		Schema: LegacyStateSchema, OwnerID: legacyRouting.OwnerID, RegistryVersion: legacyRouting.RegistryVersion,
		RegistrySHA256: legacyRouting.ManifestSHA256, Nodes: map[string]NodeState{first.NodeID: untouched},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, statePath, socketPath := managedRegistryRouter(t, tracked, state, factory)

	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	projectionClient, projection := registryClient(t, trust, dynamicManifest(2, first, second))
	projectionClient.Close()
	input := registryInstallRequest{
		OperationID: "registry-add-1",
		Expected:    registryExpected{RegistryVersion: 1, RegistrySHA256: legacyRouting.ManifestSHA256},
		Registry:    projection,
	}
	result, err := router.installRegistry(input)
	if err != nil {
		router.Close()
		t.Fatal(err)
	}
	if tracked.closed.Load() != 1 {
		router.Close()
		t.Fatal("install did not release the prior backend's idle transports")
	}
	gotUntouched := result.Nodes[first.NodeID]
	if gotUntouched.Mode != untouched.Mode || gotUntouched.StateVersion != untouched.StateVersion ||
		gotUntouched.Generation != untouched.Generation || gotUntouched.IdentityEpoch != untouched.IdentityEpoch ||
		gotUntouched.AdapterVersion != untouched.AdapterVersion || gotUntouched.RegistrationRevision != 1 {
		router.Close()
		t.Fatalf("untouched node fence changed: before=%+v after=%+v", untouched, gotUntouched)
	}
	added := result.Nodes[second.NodeID]
	if result.Schema != ProjectionStateSchema || result.RegistryVersion != 2 || len(result.RegistryEnvelope) != 0 ||
		added.Mode != ModeSealed || added.OperationID != input.OperationID || added.RegistrationRevision != 1 || added.IdentityEpoch != 1 {
		router.Close()
		t.Fatalf("invalid projection readback: %+v", result)
	}
	repeated, err := router.installRegistry(input)
	if err != nil || repeated.RegistryRequestSHA256 != result.RegistryRequestSHA256 || len(repeated.Nodes) != 2 {
		router.Close()
		t.Fatal("same operation did not return the installed result", repeated, err)
	}
	conflicting := input
	alternateSecond := second
	alternateSecond.Name = "Other Agent"
	conflicting.Registry = signedRegistry(t, dynamicManifest(2, first, alternateSecond), trust.private)
	if _, err := router.installRegistry(conflicting); err == nil {
		router.Close()
		t.Fatal("same operation with a different projection was accepted")
	}
	router.Close()
	if tracked.closed.Load() != 1 {
		t.Fatal("prior backend was closed more than once")
	}

	restored, err := harnessclient.New(projection, trust.public, trust.roots, trust.client)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newManaged(restored, statePath, socketPath)
	if err != nil {
		restored.Close()
		t.Fatal("durable signed projection did not restart", err)
	}
	if restarted.snapshot().Nodes[first.NodeID] != gotUntouched || restarted.snapshot().RegistryVersion != 2 {
		restarted.Close()
		t.Fatal("restart changed untouched node state")
	}
	restarted.Close()
}

func TestRegistryInstallUnknownCommitPoisonsUntilVerifiedRestart(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	current := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256,
		Nodes: map[string]NodeState{
			first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"},
		},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, statePath, socketPath := managedRegistryRouter(t, legacy, state, factory)
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	projectionClient, projection := registryClient(t, trust, dynamicManifest(2, first, second))
	projectionClient.Close()
	input := registryInstallRequest{
		OperationID: "registry-unknown-commit",
		Expected:    registryExpected{RegistryVersion: 1, RegistrySHA256: current.ManifestSHA256},
		Registry:    projection,
	}
	writes := 0
	router.persist = func(path string, next State, replacing bool, _ func() error) (bool, error) {
		writes++
		committed, err := writeState(path, next, replacing)
		if err != nil {
			return committed, err
		}
		return true, errors.New("synthetic post-commit durability uncertainty")
	}
	_, err := router.installRegistry(input)
	requireRegistryFault(t, err, "state_unavailable")
	if !router.poisoned.Load() || router.snapshot().RegistryVersion != 2 || writes != 1 {
		router.Close()
		t.Fatalf("ambiguous commit was not fenced: poisoned=%t version=%d writes=%d", router.poisoned.Load(), router.snapshot().RegistryVersion, writes)
	}
	_, err = router.installRegistry(input)
	requireRegistryFault(t, err, "state_unavailable")
	if writes != 1 {
		router.Close()
		t.Fatal("poisoned Router retried an ambiguous registry write")
	}
	response, err := unixClient(socketPath).Get("http://router/v1/state")
	if err != nil {
		router.Close()
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		router.Close()
		t.Fatal("poisoned Router exposed a usable state", response.StatusCode)
	}
	router.Close()

	restoredBackend, err := harnessclient.New(projection, trust.public, trust.roots, trust.client)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := newManaged(restoredBackend, statePath, socketPath)
	if err != nil {
		restoredBackend.Close()
		t.Fatal("verified state did not recover after restart", err)
	}
	defer restarted.Close()
	restarted.registryFactory = factory
	readback, err := restarted.installRegistry(input)
	if err != nil || readback.RegistryVersion != 2 || len(readback.Nodes) != 2 {
		t.Fatalf("idempotent readback after restart=%+v err=%v", readback, err)
	}
}

func TestRegistryInstallReadbackMustExactlyMatchPersistedState(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	current := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256,
		Nodes: map[string]NodeState{
			first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"},
		},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, _ := managedRegistryRouter(t, legacy, state, factory)
	defer router.Close()
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "compatible"
	projectionClient, projection := registryClient(t, trust, dynamicManifest(2, first, second))
	projectionClient.Close()
	input := registryInstallRequest{
		OperationID: "registry-readback-mismatch",
		Expected:    registryExpected{RegistryVersion: 1, RegistrySHA256: current.ManifestSHA256},
		Registry:    projection,
	}
	router.persist = func(path string, next State, replacing bool, _ func() error) (bool, error) {
		altered := next
		altered.Nodes = make(map[string]NodeState, len(next.Nodes))
		for nodeID, node := range next.Nodes {
			altered.Nodes[nodeID] = node
		}
		untouched := altered.Nodes[first.NodeID]
		untouched.Generation++
		altered.Nodes[first.NodeID] = untouched
		return writeState(path, altered, replacing)
	}
	_, err := router.installRegistry(input)
	requireRegistryFault(t, err, "state_unavailable")
	if !router.poisoned.Load() || router.snapshot().Nodes[first.NodeID].Generation != 1 {
		t.Fatal("valid but different Router readback was accepted")
	}
}

func TestRegistryInstallChangesOnlySealedTargetAndConcurrentCASHasOneWinner(t *testing.T) {
	trust := newRegistryTrust(t)
	first, second := registryNode(1, "cursor"), registryNode(2, "codex")
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 9, "compatible"
	currentClient, currentRaw := registryClient(t, trust, dynamicManifest(2, first, second))
	current := currentClient.RoutingRegistry()
	state := State{
		Schema: ProjectionStateSchema, OwnerID: current.OwnerID, RegistryVersion: current.RegistryVersion,
		RegistrySHA256: current.ManifestSHA256, RegistryOperationID: "bootstrap", RegistryRequestSHA256: current.ManifestSHA256,
		RegistryEnvelope: currentRaw,
		Nodes: map[string]NodeState{
			first.NodeID:  {Mode: ModeSealed, StateVersion: 3, Generation: 2, OperationID: "registry-update", RegistrationRevision: 1, IdentityEpoch: 7, Compatibility: "compatible", AdapterKind: "cursor", AdapterVersion: "0.153.4"},
			second.NodeID: {Mode: ModeEligible, StateVersion: 8, Generation: 5, RegistrationRevision: 1, IdentityEpoch: 9, Compatibility: "compatible", AdapterKind: "codex", AdapterVersion: "0.153.4"},
		},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, _ := managedRegistryRouter(t, currentClient, state, factory)
	defer router.Close()
	unchanged := state.Nodes[second.NodeID]
	first.Name, first.URL = "Updated Agent", "https://updated-node.invalid:9443"
	newPin := sha256.Sum256([]byte("updated-node"))
	first.CertificateSHA256 = hex.EncodeToString(newPin[:])
	first.RegistrationRevision, first.RegistrationEpoch = 2, 8
	projectionClient, projection := registryClient(t, trust, dynamicManifest(3, first, second))
	projectionClient.Close()
	input := registryInstallRequest{
		OperationID: "registry-update", Expected: registryExpected{RegistryVersion: 2, RegistrySHA256: current.ManifestSHA256}, Registry: projection,
	}
	result, err := router.installRegistry(input)
	if err != nil {
		t.Fatal(err)
	}
	if result.Nodes[second.NodeID] != unchanged {
		t.Fatalf("neighbor operation fence changed: before=%+v after=%+v", unchanged, result.Nodes[second.NodeID])
	}
	target := result.Nodes[first.NodeID]
	if target.StateVersion != 4 || target.RegistrationRevision != 2 || target.IdentityEpoch != 8 || target.AdapterVersion != "" || target.Mode != ModeSealed {
		t.Fatalf("target registration was not fenced: %+v", target)
	}

	// Both requests target the same exact installed generation. Neither may win.
	staleA := input
	staleA.OperationID = "stale-a"
	staleB := input
	staleB.OperationID = "stale-b"
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var group sync.WaitGroup
	for _, candidate := range []registryInstallRequest{staleA, staleB} {
		candidate := candidate
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, installErr := router.installRegistry(candidate)
			errorsOut <- installErr
		}()
	}
	close(start)
	group.Wait()
	close(errorsOut)
	for err := range errorsOut {
		var fault *controlFault
		if !errors.As(err, &fault) || fault.code != "stale" {
			t.Fatal("stale concurrent CAS was not rejected", err)
		}
	}
}

func TestConcurrentRegistryCASHasExactlyOneWinner(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	registry := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion,
		RegistrySHA256: registry.ManifestSHA256,
		Nodes:          map[string]NodeState{first.NodeID: {Mode: ModeEligible, StateVersion: 2, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"}},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, _ := managedRegistryRouter(t, legacy, state, factory)
	defer router.Close()
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	makeInput := func(operationID string, added harnessclient.Node) registryInstallRequest {
		added.RegistrationRevision, added.RegistrationEpoch, added.Compatibility = 1, 1, "compatible"
		return registryInstallRequest{
			OperationID: operationID,
			Expected:    registryExpected{RegistryVersion: 1, RegistrySHA256: registry.ManifestSHA256},
			Registry:    signedRegistry(t, dynamicManifest(2, first, added), trust.private),
		}
	}
	inputs := []registryInstallRequest{
		makeInput("registry-cas-a", registryNode(2, "codex")),
		makeInput("registry-cas-b", registryNode(3, "codex")),
	}
	start := make(chan struct{})
	errorsOut := make(chan error, 2)
	var group sync.WaitGroup
	for _, input := range inputs {
		input := input
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := router.installRegistry(input)
			errorsOut <- err
		}()
	}
	close(start)
	group.Wait()
	close(errorsOut)
	winners, conflicts := 0, 0
	for err := range errorsOut {
		if err == nil {
			winners++
			continue
		}
		var fault *controlFault
		if errors.As(err, &fault) && fault.code == "stale" {
			conflicts++
			continue
		}
		t.Fatal(err)
	}
	if winners != 1 || conflicts != 1 || len(router.snapshot().Nodes) != 2 {
		t.Fatalf("registry CAS winners=%d conflicts=%d nodes=%d", winners, conflicts, len(router.snapshot().Nodes))
	}
}

func TestRegistryOperatorSocketReturnsSafeMandatoryReadback(t *testing.T) {
	trust := newRegistryTrust(t)
	first := registryNode(1, "cursor")
	legacy, _ := registryClient(t, trust, legacyManifest(first))
	registry := legacy.RoutingRegistry()
	state := State{
		Schema: LegacyStateSchema, OwnerID: registry.OwnerID, RegistryVersion: registry.RegistryVersion,
		RegistrySHA256: registry.ManifestSHA256,
		Nodes:          map[string]NodeState{first.NodeID: {Mode: ModeEligible, StateVersion: 1, Generation: 1, IdentityEpoch: 7, AdapterKind: "cursor", AdapterVersion: "0.153.4"}},
	}
	factory := registryFactory(func(raw []byte) (Backend, error) {
		return harnessclient.New(raw, trust.public, trust.roots, trust.client)
	})
	router, _, socket := managedRegistryRouter(t, legacy, state, factory)
	defer router.Close()
	first.RegistrationRevision, first.RegistrationEpoch, first.Compatibility = 1, 7, "compatible"
	second := registryNode(2, "codex")
	second.RegistrationRevision, second.RegistrationEpoch, second.Compatibility = 1, 1, "legacy_readonly"
	projectionClient, projection := registryClient(t, trust, dynamicManifest(2, first, second))
	projectionClient.Close()
	input := registryInstallRequest{
		OperationID: "socket-install", Expected: registryExpected{RegistryVersion: 1, RegistrySHA256: registry.ManifestSHA256}, Registry: projection,
	}
	body, _ := json.Marshal(input)
	request, _ := http.NewRequest(http.MethodPost, "http://router/v1/registry/install", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response, err := unixClient(socket).Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var readback State
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&readback) != nil || readback.RegistryVersion != 2 {
		t.Fatal("operator install failed", response.StatusCode, readback)
	}
	encoded, _ := json.Marshal(readback)
	if strings.Contains(string(encoded), "node-1.invalid") || strings.Contains(string(encoded), "certificateSHA256") || strings.Contains(string(encoded), "registryEnvelope") {
		t.Fatal("operator readback exposed routing projection", string(encoded))
	}
}
