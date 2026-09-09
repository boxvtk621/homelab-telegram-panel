package harnessclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	hp "github.com/boxvtk621/homelab-telegram-panel/internal/harnessprotocol"
)

func TestArtifactIntegrityBeforeFullOrRangeDelivery(t *testing.T) {
	id := fixture(t, "read.identity")
	var metadata hp.ArtifactMetadata
	_ = json.Unmarshal(fixture(t, "read.artifact"), &metadata)
	data := []byte("0123456789")
	sum := sha256.Sum256(data)
	metadata.SizeBytes = int64(len(data))
	metadata.SHA256 = hex.EncodeToString(sum[:])
	var reads atomic.Int32
	rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "identity") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(id)
			return
		}
		if strings.HasSuffix(r.URL.Path, "metadata") {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metadata)
			return
		}
		reads.Add(1)
		if r.Header.Get("Range") != "" || r.Header.Get("Accept") != "application/octet-stream" {
			t.Error("binary request did not fetch whole content")
		}
		w.Header().Set("Content-Type", metadata.MediaType)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(metadata.Disposition, map[string]string{"filename": metadata.Name}))
		_, _ = w.Write(data)
	})
	for _, tc := range []struct {
		rangeValue, want, contentRange string
		status                         int
	}{{"", "0123456789", "", 200}, {"bytes=2-4", "234", "bytes 2-4/10", 206}, {"bytes=8-", "89", "bytes 8-9/10", 206}, {"bytes=-3", "789", "bytes 7-9/10", 206}, {"bytes=8-20", "89", "bytes 8-9/10", 206}} {
		out, err := rig.client.Artifact(context.Background(), testNode, testOwner, metadata.ArtifactID, tc.rangeValue)
		if err != nil || out.Status != tc.status || string(out.Body) != tc.want || out.ContentRange != tc.contentRange {
			t.Fatalf("range %q: %+v %v", tc.rangeValue, out, err)
		}
	}
	before := reads.Load()
	for _, invalid := range []string{"bytes=10-", "bytes=5-2", "bytes=0-1,3-4", "bytes=-0", "items=0-1", "bytes=01-2"} {
		_, err := rig.client.Artifact(context.Background(), testNode, testOwner, metadata.ArtifactID, invalid)
		var e *RangeError
		if !errors.As(err, &e) || e.Size != 10 {
			t.Fatal(invalid, err)
		}
	}
	if reads.Load() != before {
		t.Fatal("invalid range fetched binary")
	}
}

func TestArtifactRejectsCorruptionAndForeignMetadata(t *testing.T) {
	id := fixture(t, "read.identity")
	for _, mode := range []string{"wrong hash", "wrong size", "wrong headers", "wrong artifact", "oversized metadata"} {
		t.Run(mode, func(t *testing.T) {
			var m hp.ArtifactMetadata
			_ = json.Unmarshal(fixture(t, "read.artifact"), &m)
			content := "bounded durable bytes"
			sum := sha256.Sum256([]byte(content))
			m.SizeBytes = int64(len(content))
			m.SHA256 = hex.EncodeToString(sum[:])
			requested := m.ArtifactID
			switch mode {
			case "wrong hash":
				m.SHA256 = strings.Repeat("0", 64)
			case "wrong size":
				m.SizeBytes++
			case "wrong artifact":
				m.ArtifactID = "b0000000-0000-4000-8000-000000000002"
			case "oversized metadata":
				m.SizeBytes = hp.MaximumArtifactBytes + 1
			}
			var reads atomic.Int32
			rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "identity") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(id)
					return
				}
				if strings.HasSuffix(r.URL.Path, "metadata") {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(m)
					return
				}
				reads.Add(1)
				w.Header().Set("Content-Type", m.MediaType)
				w.Header().Set("Content-Disposition", mime.FormatMediaType(m.Disposition, map[string]string{"filename": m.Name}))
				if mode == "wrong headers" {
					w.Header().Set("Content-Type", "text/html")
				}
				_, _ = io.WriteString(w, content)
			})
			out, err := rig.client.Artifact(context.Background(), testNode, testOwner, requested, "bytes=0-1")
			if len(out.Body) != 0 {
				t.Fatal("partial bytes escaped before integrity check")
			}
			if mode == "wrong artifact" || mode == "oversized metadata" {
				requireFault(t, err, 409, "schema_mismatch")
				if reads.Load() != 0 {
					t.Fatal("invalid metadata allowed binary fetch")
				}
			} else {
				requireFault(t, err, 503, "not_durable")
			}
		})
	}
}

func TestCommandErrorCodesAndAmbiguousInvalidResponse(t *testing.T) {
	id, command := fixture(t, "read.identity"), fixture(t, "command.2.message.enqueue")
	for _, tc := range []struct {
		code   string
		status int
		valid  bool
	}{{"unsupported", 422, true}, {"schema_mismatch", 409, true}, {"protocol_mismatch", 409, true}, {"node_unavailable", 503, true}, {"unsupported", 400, false}, {"schema_mismatch", 503, false}, {"invalid", 422, false}} {
		t.Run(tc.code+http.StatusText(tc.status), func(t *testing.T) {
			failure := hp.Error{ProtocolVersion: hp.ProtocolVersion, SchemaID: hp.SchemaID, Code: tc.code, SafeMessage: "synthetic", Retryable: tc.code == "node_unavailable", CorrelationID: "10000000-0000-4000-8000-000000000002"}
			var posts atomic.Int32
			rig := newRig(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == "GET" {
					_, _ = w.Write(id)
					return
				}
				posts.Add(1)
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(failure)
			})
			got, err := rig.client.Command(context.Background(), testNode, testOwner, command)
			if tc.valid {
				if err != nil || got.Status != tc.status || hp.Validate("error", got.Body) != nil {
					t.Fatal(got, err)
				}
			} else {
				requireFault(t, err, 503, "node_unavailable")
				if len(got.Body) != 0 {
					t.Fatal("invalid response treated as definite rejection")
				}
			}
			if posts.Load() != 1 {
				t.Fatal("POST retried")
			}
		})
	}
}
