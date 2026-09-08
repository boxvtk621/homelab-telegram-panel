package mobilegateway

import (
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontract"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

func TestPublicProjectionRejectsEveryUnsafeJSONInteger(t *testing.T) {
	unsafe := mobilecontract.MaximumSafeJSONInteger + 1

	tests := []struct {
		name     string
		validate func() error
	}{
		{
			name: "Dialog list snapshot version",
			validate: func() error {
				page := validFakeReadController().dialogs
				page.SnapshotVersion = unsafe
				return validateDialogList(page, 1)
			},
		},
		{
			name: "Dialog object version",
			validate: func() error {
				page := validFakeReadController().dialogs
				page.Dialogs[0].ObjectVersion = unsafe
				page.SnapshotVersion = unsafe
				return validateDialogList(page, 1)
			},
		},
		{
			name: "Dialog snapshot version",
			validate: func() error {
				snapshot := validFakeReadController().dialog
				snapshot.SnapshotVersion = unsafe
				return validateDialogSnapshot(snapshot, readTestDialogID)
			},
		},
		{
			name: "event page snapshot version",
			validate: func() error {
				page := validFakeReadController().events
				page.SnapshotVersion = unsafe
				return validateEventList(page, 1)
			},
		},
		{
			name: "event ID",
			validate: func() error {
				page := validFakeReadController().events
				page.Events[0].EventID = unsafe
				return validateEventList(page, 1)
			},
		},
		{
			name: "event owner version",
			validate: func() error {
				page := validFakeReadController().events
				page.Events[0].OwnerVersion = unsafe
				page.SnapshotVersion = unsafe
				return validateEventList(page, 1)
			},
		},
		{
			name: "event object version",
			validate: func() error {
				page := validFakeReadController().events
				page.Events[0].ObjectVersion = unsafe
				page.Events[0].OwnerVersion = unsafe
				page.SnapshotVersion = unsafe
				return validateEventList(page, 1)
			},
		},
		{
			name: "message sequence",
			validate: func() error {
				page := validFakeReadController().messages
				page.Messages[0].Sequence = unsafe
				return validateMessageList(page, readTestDialogID, 1, 0)
			},
		},
		{
			name: "next message sequence",
			validate: func() error {
				page := validFakeReadController().messages
				page.HasMore = true
				page.NextAfterSequence = &unsafe
				return validateMessageList(page, readTestDialogID, 1, 0)
			},
		},
		{
			name: "Task expected revision",
			validate: func() error {
				task := validFakeReadController().task.Task
				task.ExpectedRevision = unsafe
				return validateTask(task)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.validate(); err == nil {
				t.Fatal("unsafe JSON integer was accepted")
			}
		})
	}
}

func TestPublicProjectionAcceptsMaximumSafeJSONInteger(t *testing.T) {
	maximum := mobilecontract.MaximumSafeJSONInteger
	task := validFakeReadController().task.Task
	task.ExpectedRevision = maximum
	if err := validateTask(task); err != nil {
		t.Fatalf("maximum safe Task revision rejected: %v", err)
	}

	page := mobilecontrollerclient.MessageList{
		Messages: []mobilecontrollerclient.Message{{
			ID: readTestMessageID, DialogID: readTestDialogID, Sequence: maximum,
			Actor: "owner", Kind: "input", Content: "safe", CreatedAt: readTestTime,
		}},
		FreshnessAt: readTestTime,
	}
	if err := validateMessageList(page, readTestDialogID, 1, maximum-1); err != nil {
		t.Fatalf("maximum safe message sequence rejected: %v", err)
	}
}
