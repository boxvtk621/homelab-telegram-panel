// Package mobilecontract validates the versioned Mobile API wire vocabulary.
// It has no domain services, storage, admission or execution authority.
// Rules originate from Fixik aea6ae78, private protocol v2 / public API v1.
package mobilecontract

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	DialogLifecycleActive                   = "active"
	DialogLifecycleArchived                 = "archived"
	ManagementEventDialogCreated            = "dialog.created"
	ManagementEventAdmissionPending         = "admission.pending"
	ManagementEventAdmissionDeferred        = "admission.deferred"
	ManagementEventAdmissionAccepted        = "admission.accepted"
	ManagementEventAdmissionRejected        = "admission.rejected"
	ManagementEventDialogTaskReleased       = "dialog.task.released"
	DefaultPageSize                         = 20
	MaximumPageSize                         = 100
	MaximumMessageContentBytes              = 64 * 1024
	MaximumMessagePageContentBytes          = 128 * 1024
	MaximumSafeJSONInteger            int64 = 1<<53 - 1
)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-8][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func ValidateCanonicalUUID(name, value string) error {
	if !canonicalUUIDPattern.MatchString(value) {
		return fmt.Errorf("%s must be a canonical UUID", name)
	}
	return nil
}

func ValidateDialogID(value string) error {
	return ValidateCanonicalUUID("dialog ID", value)
}

func ValidateOpaqueID(name, value string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) ||
		len(value) > 256 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("%s is missing or invalid", name)
	}
	return nil
}

func ValidateTaskReadID(value string) error {
	if err := ValidateOpaqueID("task ID", value); err != nil {
		return err
	}
	if strings.ContainsAny(value, "/?#") {
		return fmt.Errorf("task ID contains a URL separator")
	}
	return nil
}

func ValidateTimestamp(name string, value time.Time) error {
	encoded := value.UTC().UnixMicro()
	if value.IsZero() || encoded <= 0 || value.UTC().Year() > 9999 ||
		!time.UnixMicro(encoded).UTC().Equal(value.UTC()) {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}
