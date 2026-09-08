package mobilegateway

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"
)

const (
	serverReadHeaderTimeout = 5 * time.Second
	serverReadTimeout       = 10 * time.Second
	serverWriteTimeout      = 15 * time.Second
	serverIdleTimeout       = 60 * time.Second
	serverMaximumHeaderSize = 16 * 1024
)

// NewHardenedServer returns the reviewed public HTTP timeout boundary without
// opening a listener. Runtime wiring remains a separate release-gated step.
func NewHardenedServer(address string, handler http.Handler) (*http.Server, error) {
	if handler == nil {
		return nil, errors.New("mobile Gateway handler is not configured")
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" || portText == "" {
		return nil, errors.New("mobile Gateway address must be an explicit loopback IP and port")
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return nil, errors.New("mobile Gateway address must be an explicit loopback IP and port")
	}
	return &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
		MaxHeaderBytes:    serverMaximumHeaderSize,
	}, nil
}
