package mobilecontract

import (
	"strings"
	"testing"
	"time"
)

func TestWireIdentities(t *testing.T) {
	for _, value := range []string{"018f0c9e-8f4b-4a6b-8c9d-000000000099", "018f0c9e-8f4b-8a6b-bc9d-000000000099"} {
		if ValidateDialogID(value) != nil {
			t.Fatalf("valid UUID rejected: %s", value)
		}
	}
	for _, value := range []string{"", "018F0C9E-8f4b-4a6b-8c9d-000000000099", "018f0c9e-8f4b-0a6b-8c9d-000000000099", "018f0c9e-8f4b-4a6b-7c9d-000000000099"} {
		if ValidateDialogID(value) == nil {
			t.Fatalf("invalid UUID accepted: %s", value)
		}
	}
	if ValidateTaskReadID(strings.Repeat("x", 256)) != nil {
		t.Fatal("256-byte ID rejected")
	}
	for _, value := range []string{"", " ", " leading", "trailing ", "a\x00b", "a\u0085b", string([]byte{255}), strings.Repeat("x", 257), "a/b", "a?b", "a#b"} {
		if ValidateTaskReadID(value) == nil {
			t.Fatalf("invalid task ID accepted: %q", value)
		}
	}
}

func TestWireTimestampPrecisionAndRange(t *testing.T) {
	for _, value := range []time.Time{time.UnixMicro(1), time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)} {
		if ValidateTimestamp("time", value) != nil {
			t.Fatalf("valid timestamp rejected: %v", value)
		}
	}
	for _, value := range []time.Time{{}, time.UnixMicro(0), time.UnixMicro(-1), time.Unix(1, 1), time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if ValidateTimestamp("time", value) == nil {
			t.Fatalf("invalid timestamp accepted: %v", value)
		}
	}
}
