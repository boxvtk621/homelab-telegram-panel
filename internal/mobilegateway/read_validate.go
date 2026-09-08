package mobilegateway

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontract"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
)

var errInvalidControllerProjection = errors.New("Controller read projection is invalid")

func validateDialogList(page mobilecontrollerclient.DialogList, limit int) error {
	if !safeJSONInteger(page.SnapshotVersion, false) || len(page.Dialogs) > limit || !validReadTimestamp(page.FreshnessAt) {
		return errInvalidControllerProjection
	}
	if len(page.Dialogs) > 0 && page.SnapshotVersion == 0 {
		return errInvalidControllerProjection
	}
	seen := make(map[string]struct{}, len(page.Dialogs))
	for index, dialog := range page.Dialogs {
		if err := validateDialog(dialog); err != nil || dialog.ObjectVersion > page.SnapshotVersion {
			return errInvalidControllerProjection
		}
		if _, duplicate := seen[dialog.ID]; duplicate {
			return errInvalidControllerProjection
		}
		seen[dialog.ID] = struct{}{}
		if index > 0 && !publicDialogPrecedes(page.Dialogs[index-1], dialog) {
			return errInvalidControllerProjection
		}
	}
	if page.NextCursor != "" {
		if page.EventCheckpoint != "" || len(page.Dialogs) == 0 || len(page.Dialogs) != limit ||
			!validPublicCursor(page.NextCursor) {
			return errInvalidControllerProjection
		}
	} else if !validPublicCursor(page.EventCheckpoint) {
		return errInvalidControllerProjection
	}
	return nil
}

func validateDialog(dialog mobilecontrollerclient.Dialog) error {
	createdAt, createdOK := parseReadTimestamp(dialog.CreatedAt)
	updatedAt, updatedOK := parseReadTimestamp(dialog.UpdatedAt)
	if mobilecontract.ValidateDialogID(dialog.ID) != nil || !safeJSONInteger(dialog.ObjectVersion, true) ||
		(dialog.ActiveTaskID != "" && mobilecontract.ValidateTaskReadID(dialog.ActiveTaskID) != nil) ||
		(dialog.PendingAdmissionID != "" && (dialog.ActiveTaskID != "" || mobilecontract.ValidateCanonicalUUID("pending admission", dialog.PendingAdmissionID) != nil)) ||
		(dialog.AdmissionNextCheckAt != "" && (dialog.PendingAdmissionID == "" || !validReadTimestamp(dialog.AdmissionNextCheckAt))) ||
		(dialog.Lifecycle != string(mobilecontract.DialogLifecycleActive) &&
			dialog.Lifecycle != string(mobilecontract.DialogLifecycleArchived)) ||
		!createdOK || !updatedOK || updatedAt.Before(createdAt) {
		return errInvalidControllerProjection
	}
	return nil
}

func validateDialogSnapshot(snapshot mobilecontrollerclient.DialogSnapshot, dialogID string) error {
	if snapshot.Dialog.ID != dialogID || !safeJSONInteger(snapshot.SnapshotVersion, true) ||
		snapshot.Dialog.ObjectVersion > snapshot.SnapshotVersion || !validReadTimestamp(snapshot.FreshnessAt) {
		return errInvalidControllerProjection
	}
	return validateDialog(snapshot.Dialog)
}

func validateEventList(page mobilecontrollerclient.EventList, limit int) error {
	if !safeJSONInteger(page.SnapshotVersion, false) || len(page.Events) > limit || !validReadTimestamp(page.FreshnessAt) {
		return errInvalidControllerProjection
	}
	previousEventID := int64(0)
	previousOwnerVersion := int64(0)
	for index, event := range page.Events {
		if !safeJSONInteger(event.EventID, true) || event.EventID <= previousEventID ||
			!safeJSONInteger(event.OwnerVersion, true) ||
			event.OwnerVersion > page.SnapshotVersion || event.ObjectVersion < 1 ||
			!safeJSONInteger(event.ObjectVersion, true) || event.ObjectVersion > event.OwnerVersion ||
			mobilecontract.ValidateDialogID(event.DialogID) != nil ||
			!validManagementEventKind(event.Kind) || event.Lifecycle != string(mobilecontract.DialogLifecycleActive) ||
			!validReadTimestamp(event.OccurredAt) {
			return errInvalidControllerProjection
		}
		if index > 0 && event.OwnerVersion != previousOwnerVersion+1 {
			return errInvalidControllerProjection
		}
		previousEventID = event.EventID
		previousOwnerVersion = event.OwnerVersion
	}
	if len(page.Events) == 0 {
		if page.HasMore || (page.NextCursor != "" && !validPublicCursor(page.NextCursor)) {
			return errInvalidControllerProjection
		}
		return nil
	}
	if !validPublicCursor(page.NextCursor) {
		return errInvalidControllerProjection
	}
	last := page.Events[len(page.Events)-1]
	if page.HasMore {
		if len(page.Events) != limit || last.OwnerVersion >= page.SnapshotVersion {
			return errInvalidControllerProjection
		}
	} else if last.OwnerVersion != page.SnapshotVersion {
		return errInvalidControllerProjection
	}
	return nil
}

func validManagementEventKind(value string) bool {
	switch value {
	case mobilecontract.ManagementEventDialogCreated,
		mobilecontract.ManagementEventAdmissionPending,
		mobilecontract.ManagementEventAdmissionDeferred,
		mobilecontract.ManagementEventAdmissionAccepted,
		mobilecontract.ManagementEventAdmissionRejected,
		mobilecontract.ManagementEventDialogTaskReleased:
		return true
	default:
		return false
	}
}

func validateMessageList(page mobilecontrollerclient.MessageList, dialogID string, limit int, afterSequence int64) error {
	if len(page.Messages) > limit || !validReadTimestamp(page.FreshnessAt) {
		return errInvalidControllerProjection
	}
	seen := make(map[string]struct{}, len(page.Messages))
	previousSequence := afterSequence
	totalContentBytes := 0
	for _, message := range page.Messages {
		if mobilecontract.ValidateDialogID(message.ID) != nil || message.DialogID != dialogID ||
			mobilecontract.ValidateDialogID(message.DialogID) != nil ||
			!safeJSONInteger(message.Sequence, true) || message.Sequence <= previousSequence ||
			(message.Actor != "owner" && message.Actor != "controller" && message.Actor != "worker") ||
			(message.Kind != "input" && message.Kind != "output" && message.Kind != "system") ||
			!validPublicMessageContent(message.Content) || !validReadTimestamp(message.CreatedAt) {
			return errInvalidControllerProjection
		}
		if message.SupersedesID != "" {
			if mobilecontract.ValidateDialogID(message.SupersedesID) != nil || message.SupersedesID == message.ID {
				return errInvalidControllerProjection
			}
		}
		if _, duplicate := seen[message.ID]; duplicate {
			return errInvalidControllerProjection
		}
		seen[message.ID] = struct{}{}
		previousSequence = message.Sequence
		if len(message.Content) > mobilecontract.MaximumMessagePageContentBytes-totalContentBytes {
			return errInvalidControllerProjection
		}
		totalContentBytes += len(message.Content)
	}
	if page.HasMore {
		if len(page.Messages) == 0 || page.NextAfterSequence == nil ||
			!safeJSONInteger(*page.NextAfterSequence, true) || *page.NextAfterSequence != previousSequence {
			return errInvalidControllerProjection
		}
	} else if page.NextAfterSequence != nil {
		return errInvalidControllerProjection
	}
	return nil
}

func validPublicMessageContent(value string) bool {
	return utf8.ValidString(value) && len(value) <= mobilecontract.MaximumMessageContentBytes &&
		strings.IndexFunc(value, func(character rune) bool {
			return character == '\x00' ||
				(unicode.IsControl(character) && character != '\n' && character != '\r' && character != '\t')
		}) < 0
}

func validateTaskList(page mobilecontrollerclient.TaskList, limit int) error {
	if len(page.Tasks) > limit || !validReadTimestamp(page.FreshnessAt) {
		return errInvalidControllerProjection
	}
	seen := make(map[string]struct{}, len(page.Tasks))
	for index, task := range page.Tasks {
		if err := validateTask(task); err != nil {
			return errInvalidControllerProjection
		}
		if _, duplicate := seen[task.ID]; duplicate {
			return errInvalidControllerProjection
		}
		seen[task.ID] = struct{}{}
		if index > 0 && !publicTaskPrecedes(page.Tasks[index-1], task) {
			return errInvalidControllerProjection
		}
	}
	return nil
}

func validateTaskSnapshot(snapshot mobilecontrollerclient.TaskSnapshot, taskID string) error {
	if snapshot.Task.ID != taskID || !validReadTimestamp(snapshot.FreshnessAt) {
		return errInvalidControllerProjection
	}
	return validateTask(snapshot.Task)
}

func validateTask(task mobilecontrollerclient.Task) error {
	createdAt, createdOK := parseReadTimestamp(task.CreatedAt)
	updatedAt, updatedOK := parseReadTimestamp(task.UpdatedAt)
	if mobilecontract.ValidateTaskReadID(task.ID) != nil || mobilecontract.ValidateDialogID(task.DialogID) != nil ||
		mobilecontract.ValidateOpaqueID("issue ID", task.IssueID) != nil ||
		!safeJSONInteger(task.ExpectedRevision, true) ||
		!validTaskState(task.State) || !validTaskCancelState(task.CancelState) ||
		!createdOK || !updatedOK || updatedAt.Before(createdAt) {
		return errInvalidControllerProjection
	}
	if task.NextCheckAt != "" && !validReadTimestamp(task.NextCheckAt) {
		return errInvalidControllerProjection
	}
	return nil
}

func validTaskState(value string) bool {
	switch value {
	case "ready", "running", "waiting", "completed", "failed", "cancelled", "needs_review":
		return true
	default:
		return false
	}
}

func validTaskCancelState(value string) bool {
	switch value {
	case "none", "requested", "stop_uncertain", "stopped":
		return true
	default:
		return false
	}
}

func validateControl(summary mobilecontrollerclient.ControlSummary) error {
	if len(summary.Components) < 1 || len(summary.Components) > 8 ||
		len(summary.Capacity) < 1 || len(summary.Capacity) > 4 ||
		!validReadTimestamp(summary.FreshnessAt) {
		return errInvalidControllerProjection
	}
	components := make(map[string]struct{}, len(summary.Components))
	for _, component := range summary.Components {
		if !validControlComponent(component.Name) || !validControlState(component.State) {
			return errInvalidControllerProjection
		}
		if _, duplicate := components[component.Name]; duplicate {
			return errInvalidControllerProjection
		}
		components[component.Name] = struct{}{}
	}
	capacity := make(map[string]struct{}, len(summary.Capacity))
	for _, item := range summary.Capacity {
		if (item.Class != "general" && item.Class != "reserve") || item.Limit < 1 || item.Limit > 1_000_000 ||
			item.InFlight < 0 || item.InFlight > item.Limit {
			return errInvalidControllerProjection
		}
		if _, duplicate := capacity[item.Class]; duplicate {
			return errInvalidControllerProjection
		}
		capacity[item.Class] = struct{}{}
	}
	return nil
}

func validControlComponent(value string) bool {
	switch value {
	case "controller", "persistence", "scheduler", "worker_pool", "delivery":
		return true
	default:
		return false
	}
}

func validControlState(value string) bool {
	return value == "ready" || value == "degraded" || value == "unavailable"
}

func safeJSONInteger(value int64, positive bool) bool {
	minimum := int64(0)
	if positive {
		minimum = 1
	}
	return value >= minimum && value <= mobilecontract.MaximumSafeJSONInteger
}

func validReadTimestamp(value string) bool {
	_, ok := parseReadTimestamp(value)
	return ok
}

func parseReadTimestamp(value string) (time.Time, bool) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC || mobilecontract.ValidateTimestamp("read timestamp", parsed) != nil {
		return time.Time{}, false
	}
	return parsed, true
}

func publicDialogPrecedes(previous, next mobilecontrollerclient.Dialog) bool {
	previousTime, previousOK := parseReadTimestamp(previous.CreatedAt)
	nextTime, nextOK := parseReadTimestamp(next.CreatedAt)
	if !previousOK || !nextOK {
		return false
	}
	if previousTime.Equal(nextTime) {
		return previous.ID > next.ID
	}
	return previousTime.After(nextTime)
}

func publicTaskPrecedes(previous, next mobilecontrollerclient.Task) bool {
	previousTime, previousOK := parseReadTimestamp(previous.UpdatedAt)
	nextTime, nextOK := parseReadTimestamp(next.UpdatedAt)
	if !previousOK || !nextOK {
		return false
	}
	if previousTime.Equal(nextTime) {
		return previous.ID > next.ID
	}
	return previousTime.After(nextTime)
}
