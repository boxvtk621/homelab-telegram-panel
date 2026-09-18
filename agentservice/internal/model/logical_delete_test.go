package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLogicalDeleteNodeReceiptBindsEmbeddedCommand(t *testing.T) {
	deletedAt := "2026-09-17T10:00:00Z"
	receipt := LogicalDeleteNodeReceipt{
		SchemaID: LogicalDeleteNodeSchemaID, OperationID: "61000000-0000-4000-8000-000000000001",
		NodeRequestHash: strings.Repeat("a", 64), CoordinatorRequestHash: strings.Repeat("b", 64),
		ReceiptID: "61000000-0000-4000-8000-000000000006", CommandID: "61000000-0000-4000-8000-000000000002",
		LogicalDialogID: "61000000-0000-4000-8000-000000000007",
		NodeID:          "61000000-0000-4000-8000-000000000003", NodeDialogID: "61000000-0000-4000-8000-000000000004",
		Epoch: 1, RegistryVersion: 1, BindingVersion: 1, DeletedDialogVersion: 3,
		HoldVersion: 1, HoldScopeRevision: 2, TombstoneEventSeq: 9, DeletedAt: deletedAt,
		CommandReceipt: json.RawMessage(`{"protocolVersion":1,"schemaId":"harness-wire-v2","commandId":"61000000-0000-4000-8000-000000000002","commandKind":"dialog.delete","receiptId":"61000000-0000-4000-8000-000000000005","acceptedAt":"2026-09-17T10:00:00Z","nodeId":"61000000-0000-4000-8000-000000000003","eventSeq":9,"result":"deleted","references":{"dialogId":"61000000-0000-4000-8000-000000000004"}}`),
	}
	if err := ValidateLogicalDeleteNodeReceipt(receipt); err != nil {
		t.Fatal(err)
	}
	receipt.TombstoneEventSeq++
	if ValidateLogicalDeleteNodeReceipt(receipt) == nil {
		t.Fatal("mismatched embedded event sequence was accepted")
	}
}

func TestLogicalDeleteAdvanceRejectsUnusedAndUnsafeFields(t *testing.T) {
	base := LogicalDeleteAdvance{
		SchemaID: LogicalDeleteAdvanceSchemaID, OperationID: "61000000-0000-4000-8000-000000000001",
		RequestHash: strings.Repeat("a", 64), ExpectedOperationVersion: 1, Action: "unknown",
	}
	if err := ValidateLogicalDeleteAdvance(base); err != nil {
		t.Fatal(err)
	}
	base.ObservedHoldScopeRevision = 1
	if ValidateLogicalDeleteAdvance(base) == nil {
		t.Fatal("unused hold scope revision was accepted")
	}
	base.Action = "reject"
	base.ResultCode = "unsafe result"
	if ValidateLogicalDeleteAdvance(base) == nil {
		t.Fatal("unsafe rejection result code was accepted")
	}
	base.ResultCode = "hold_rejected"
	base.ObservedHoldScopeRevision = 0
	if err := ValidateLogicalDeleteAdvance(base); err != nil {
		t.Fatalf("zero-revision pre-hold rejection was rejected: %v", err)
	}
}
