package harnessrouter

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
)

type backendCall struct {
	method, nodeID, owner, route, query, artifactID, byteRange string
	after                                                      int64
	body                                                       []byte
	ctx                                                        context.Context
}

type fakeBackend struct {
	calls    []backendCall
	registry harnessclient.PublicRegistry
	response harnessclient.Response
	stream   *harnessclient.Stream
	binary   harnessclient.BinaryResponse
	err      error
	publicOK bool
	closed   bool
}

func (f *fakeBackend) Public(owner string) (harnessclient.PublicRegistry, bool) {
	f.calls = append(f.calls, backendCall{method: "Public", owner: owner})
	return f.registry, f.publicOK
}

func (f *fakeBackend) Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error) {
	f.calls = append(f.calls, backendCall{method: "Read", nodeID: nodeID, owner: owner, route: route, query: query, ctx: ctx})
	return f.response, f.err
}

func (f *fakeBackend) Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	f.calls = append(f.calls, backendCall{method: "Command", nodeID: nodeID, owner: owner, body: bytes.Clone(body), ctx: ctx})
	return f.response, f.err
}

func (f *fakeBackend) OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error) {
	f.calls = append(f.calls, backendCall{method: "OpenEvents", nodeID: nodeID, owner: owner, after: after, ctx: ctx})
	return f.stream, f.err
}

func (f *fakeBackend) Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error) {
	f.calls = append(f.calls, backendCall{method: "Artifact", nodeID: nodeID, owner: owner, artifactID: artifactID, byteRange: byteRange, ctx: ctx})
	return f.binary, f.err
}

func (f *fakeBackend) Close() { f.closed = true }

func TestRouterDelegatesExactRequestsOnce(t *testing.T) {
	wantErr := errors.New("synthetic backend error")
	stream := &harnessclient.Stream{}
	backend := &fakeBackend{
		registry: harnessclient.PublicRegistry{RegistryVersion: 7, Mode: "fixture"},
		response: harnessclient.Response{Status: 202, Body: []byte(`{"accepted":true}`)},
		stream:   stream,
		binary:   harnessclient.BinaryResponse{Status: 206, Body: []byte("bytes")},
		err:      wantErr,
		publicOK: true,
	}
	router, err := New(backend)
	if err != nil {
		t.Fatal(err)
	}

	registry, ok := router.Public("owner-1")
	if !ok || !reflect.DeepEqual(registry, backend.registry) {
		t.Fatal("public registry result changed", registry, ok)
	}
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	response, err := router.Read(ctx, "node-1", "owner-1", "dialogs", "limit=20")
	if !reflect.DeepEqual(response, backend.response) || !errors.Is(err, wantErr) {
		t.Fatal("read result changed", response, err)
	}
	body := []byte(`{"kind":"message.enqueue"}`)
	wantBody := bytes.Clone(body)
	response, err = router.Command(ctx, "node-1", "owner-1", body)
	if !reflect.DeepEqual(response, backend.response) || !errors.Is(err, wantErr) {
		t.Fatal("command result changed", response, err)
	}
	body[0] = 'x'
	gotStream, err := router.OpenEvents(ctx, "node-1", "owner-1", 41)
	if gotStream != stream || !errors.Is(err, wantErr) {
		t.Fatal("stream result changed", gotStream, err)
	}
	binary, err := router.Artifact(ctx, "node-1", "owner-1", "artifact-1", "bytes=2-4")
	if !reflect.DeepEqual(binary, backend.binary) || !errors.Is(err, wantErr) {
		t.Fatal("artifact result changed", binary, err)
	}
	router.Close()

	want := []backendCall{
		{method: "Public", owner: "owner-1"},
		{method: "Read", nodeID: "node-1", owner: "owner-1", route: "dialogs", query: "limit=20", ctx: ctx},
		{method: "Command", nodeID: "node-1", owner: "owner-1", body: wantBody, ctx: ctx},
		{method: "OpenEvents", nodeID: "node-1", owner: "owner-1", after: 41, ctx: ctx},
		{method: "Artifact", nodeID: "node-1", owner: "owner-1", artifactID: "artifact-1", byteRange: "bytes=2-4", ctx: ctx},
	}
	if len(backend.calls) != len(want) {
		t.Fatalf("backend calls = %d, want %d", len(backend.calls), len(want))
	}
	for i := range want {
		if backend.calls[i].method != want[i].method || backend.calls[i].nodeID != want[i].nodeID || backend.calls[i].owner != want[i].owner || backend.calls[i].route != want[i].route || backend.calls[i].query != want[i].query || backend.calls[i].artifactID != want[i].artifactID || backend.calls[i].byteRange != want[i].byteRange || backend.calls[i].after != want[i].after || !bytes.Equal(backend.calls[i].body, want[i].body) || backend.calls[i].ctx != want[i].ctx {
			t.Fatalf("call %d = %#v, want %#v", i, backend.calls[i], want[i])
		}
	}
	if !backend.closed {
		t.Fatal("backend was not closed")
	}
}

func TestRouterPreservesMissingPublicRegistry(t *testing.T) {
	backend := &fakeBackend{registry: harnessclient.PublicRegistry{Mode: "live"}}
	router, err := New(backend)
	if err != nil {
		t.Fatal(err)
	}
	registry, ok := router.Public("foreign-owner")
	if ok || !reflect.DeepEqual(registry, backend.registry) {
		t.Fatal("missing public registry result changed", registry, ok)
	}
}

func TestNewRejectsMissingBackend(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("missing backend accepted")
	}
}
