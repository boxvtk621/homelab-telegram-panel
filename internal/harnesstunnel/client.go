package harnesstunnel

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"time"
)

type DialError struct{ Code string }

func (e *DialError) Error() string { return e.Code }

type Client struct {
	socket string
	dialer *net.Dialer
	now    func() time.Time
}

func NewClient(socket string) (*Client, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || len(socket) > 100 || strings.ContainsAny(socket, "\x00\r\n") {
		return nil, errors.New("invalid Harness tunnel socket")
	}
	return &Client{socket: socket, dialer: &net.Dialer{Timeout: 5 * time.Second}, now: time.Now}, nil
}

func (c *Client) DialContext(ctx context.Context, owner string, binding EndpointBinding, purpose Purpose) (net.Conn, error) {
	if c == nil || c.dialer == nil || binding.Validate() != nil || !refPattern.MatchString(owner) || !purpose.Valid() {
		return nil, &DialError{Code: "invalid_request"}
	}
	now := c.now()
	deadline := now.Add(10 * time.Second)
	if value, ok := ctx.Deadline(); ok && value.Before(deadline) {
		deadline = value
	}
	if !deadline.After(now) {
		return nil, ctx.Err()
	}
	connection, err := c.dialer.DialContext(ctx, "unix", c.socket)
	if err != nil {
		return nil, &DialError{Code: "dialer_unavailable"}
	}
	ready := false
	defer func() {
		if !ready {
			_ = connection.Close()
		}
	}()
	_ = connection.SetDeadline(deadline)
	request := DialRequest{
		SchemaID: RequestSchemaID, OwnerID: owner, NodeID: binding.NodeID,
		RegistrationRevision: binding.RegistrationRevision, RegistrationEpoch: binding.RegistrationEpoch,
		EndpointRevision: binding.EndpointRevision, Purpose: purpose, DeadlineUnixMilli: deadline.UnixMilli(),
	}
	if err := WriteDialRequest(connection, request); err != nil {
		return nil, &DialError{Code: "dialer_unavailable"}
	}
	reply, err := ReadDialReply(connection)
	if err != nil {
		return nil, &DialError{Code: "dialer_unavailable"}
	}
	if reply.Status != "ready" {
		return nil, &DialError{Code: reply.Code}
	}
	_ = connection.SetDeadline(time.Time{})
	ready = true
	return connection, nil
}
