package harnessbarrier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSchemaArtifactPin(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "api", "harness-barrier-v1.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		ID   string                     `json:"$id"`
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.ID != "https://homelab.invalid/contracts/harness-barrier-v1.schema.json" || len(schema.Defs) != 6 {
		t.Fatal("barrier schema identity or definition count changed")
	}
	sum := sha256.Sum256(data)
	if actual := hex.EncodeToString(sum[:]); actual != SchemaSHA256 {
		t.Fatalf("barrier schema hash %s, compiled pin %s", actual, SchemaSHA256)
	}
}

func TestVersionedBarrierContract(t *testing.T) {
	fixtures := map[string]any{
		"installRequest": InstallRequest{
			ProtocolVersion: 1, SchemaID: SchemaID, OperationID: "hold-op-1",
			NodeID: "10000000-0000-4000-8000-000000000001", ExpectedEpoch: 1, BindingGeneration: 3,
			Scope: Scope{Kind: "dialog", DialogID: "20000000-0000-4000-8000-000000000001"}, ExpectedScopeRevision: 0,
		},
		"holdReceipt": HoldReceipt{
			ProtocolVersion: 1, SchemaID: SchemaID, Kind: "hold.installed", OperationID: "hold-op-1",
			ReceiptID: "30000000-0000-4000-8000-000000000001", NodeID: "10000000-0000-4000-8000-000000000001",
			Epoch: 1, BindingGeneration: 3, Scope: Scope{Kind: "node"}, HoldVersion: 1, ScopeRevision: 1,
			InstalledAt: "2026-09-16T00:00:00Z",
		},
		"rejectionReceipt": RejectionReceipt{
			ProtocolVersion: 1, SchemaID: SchemaID, Kind: "command.rejected",
			CommandID: "40000000-0000-4000-8000-000000000001", CommandKind: "message.enqueue",
			ReceiptID: "50000000-0000-4000-8000-000000000001", NodeID: "10000000-0000-4000-8000-000000000001",
			Epoch: 1, CanonicalPayloadHash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			RejectedAt: "2026-09-16T00:00:00Z", Reason: "administrative_hold", HoldOperationID: "hold-op-1",
			HoldVersion: 1, Scope: Scope{Kind: "dialog", DialogID: "20000000-0000-4000-8000-000000000001"}, ScopeRevision: 1,
		},
	}
	for wireType, fixture := range fixtures {
		raw, err := json.Marshal(fixture)
		if err != nil || Validate(wireType, raw) != nil {
			t.Fatalf("%s fixture invalid: %v / %v", wireType, err, Validate(wireType, raw))
		}
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("decode fixture")
		}
		object["unexpected"] = true
		invalid, _ := json.Marshal(object)
		if Validate(wireType, invalid) == nil {
			t.Fatalf("%s accepted an unknown field", wireType)
		}
	}
}

func TestBarrierContractRejectsUnsafeIntegers(t *testing.T) {
	request := InstallRequest{
		ProtocolVersion: 1, SchemaID: SchemaID, OperationID: "hold-op-1",
		NodeID: "10000000-0000-4000-8000-000000000001", ExpectedEpoch: MaximumSafeInteger + 1, BindingGeneration: 1,
		Scope: Scope{Kind: "node"}, ExpectedScopeRevision: 0,
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if Validate("installRequest", raw) == nil {
		t.Fatal("unsafe integer was accepted")
	}
}

func TestBarrierContractRequiresExactScopeShape(t *testing.T) {
	for _, scope := range []string{
		`{"kind":"node","dialogId":""}`,
		`{"kind":"node","dialogId":null}`,
		`{"kind":"dialog"}`,
	} {
		raw := []byte(`{"protocolVersion":1,"schemaId":"harness-barrier-v1","operationId":"hold-op-1","nodeId":"10000000-0000-4000-8000-000000000001","expectedEpoch":1,"bindingGeneration":1,"scope":` + scope + `,"expectedScopeRevision":0}`)
		if Validate("installRequest", raw) == nil {
			t.Fatalf("accepted non-canonical scope shape: %s", scope)
		}
	}
}
