package mobilecontrollerclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

type SubmissionRequest struct {
	CommandID             string `json:"command_id"`
	ClientInstanceID      string `json:"client_instance_id"`
	ExpectedObjectVersion int64  `json:"expected_object_version"`
	RequestedIssueID      string `json:"requested_issue_id"`
	Text                  string `json:"text"`
}

type SubmissionReceipt struct {
	CommandID      string `json:"command_id"`
	Acknowledgment string `json:"acknowledgment"`
	DialogID       string `json:"dialog_id"`
	AdmissionID    string `json:"admission_id"`
	MessageID      string `json:"message_id"`
	TaskID         string `json:"task_id"`
	ObjectVersion  int64  `json:"object_version"`
	ReceivedAt     string `json:"received_at"`
	Replayed       bool   `json:"replayed"`
}

func (client *Client) SubmitTask(ctx context.Context, requestID, dialogID string, input SubmissionRequest) (SubmissionReceipt, error) {
	var result SubmissionReceipt
	if client == nil || !safePathSegment(dialogID) || !safePathSegment(input.CommandID) || !safePathSegment(input.ClientInstanceID) || input.ExpectedObjectVersion < 1 || input.ExpectedObjectVersion >= 1<<53-1 {
		return result, errors.New("Controller submission is invalid")
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return result, errors.New("Controller submission is invalid")
	}
	if err := client.do(ctx, client.business, http.MethodPost, internalPrefix+"/dialogs/"+url.PathEscape(dialogID)+"/submissions", requestID, payload, &result); err != nil {
		return SubmissionReceipt{}, err
	}
	if !ValidSubmissionReceipt(result, dialogID, input.CommandID, input.ExpectedObjectVersion) {
		return SubmissionReceipt{}, errors.New("Controller submission outcome is invalid")
	}
	return result, nil
}

var submissionObjectIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidSubmissionReceipt(receipt SubmissionReceipt, dialogID, commandID string, expected int64) bool {
	_, err := time.Parse(time.RFC3339Nano, receipt.ReceivedAt)
	return receipt.CommandID == commandID && receipt.DialogID == dialogID && receipt.Acknowledgment == "received" &&
		expected > 0 && expected < 1<<53-1 && receipt.ObjectVersion == expected+1 && submissionObjectIDPattern.MatchString(receipt.AdmissionID) && submissionObjectIDPattern.MatchString(receipt.MessageID) && safePathSegment(receipt.TaskID) && err == nil
}
