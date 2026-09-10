// Package harnessrouter is the non-authoritative routing boundary between the
// Panel and Harness nodes. It preserves exact node affinity and wire payloads;
// admission policy and drain state can be added here without coupling the
// browser-facing Panel to the private node transport.
package harnessrouter

import (
	"context"
	"errors"

	"github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient"
)

// Backend is the private node transport used by Router.
type Backend interface {
	Public(owner string) (harnessclient.PublicRegistry, bool)
	Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error)
	Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error)
	OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error)
	Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error)
	Close()
}

type Router struct {
	backend Backend
}

func Load(paths harnessclient.Paths) (*Router, error) {
	client, err := harnessclient.Load(paths)
	if err != nil {
		return nil, err
	}
	return &Router{backend: client}, nil
}

func New(backend Backend) (*Router, error) {
	if backend == nil {
		return nil, errors.New("missing Harness Router backend")
	}
	return &Router{backend: backend}, nil
}

func (r *Router) Public(owner string) (harnessclient.PublicRegistry, bool) {
	return r.backend.Public(owner)
}

func (r *Router) Read(ctx context.Context, nodeID, owner, route, query string) (harnessclient.Response, error) {
	return r.backend.Read(ctx, nodeID, owner, route, query)
}

func (r *Router) Command(ctx context.Context, nodeID, owner string, body []byte) (harnessclient.Response, error) {
	return r.backend.Command(ctx, nodeID, owner, body)
}

func (r *Router) OpenEvents(ctx context.Context, nodeID, owner string, after int64) (*harnessclient.Stream, error) {
	return r.backend.OpenEvents(ctx, nodeID, owner, after)
}

func (r *Router) Artifact(ctx context.Context, nodeID, owner, artifactID, byteRange string) (harnessclient.BinaryResponse, error) {
	return r.backend.Artifact(ctx, nodeID, owner, artifactID, byteRange)
}

func (r *Router) Close() {
	r.backend.Close()
}
