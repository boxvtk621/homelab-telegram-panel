// The historical command path is retained for image/build compatibility.
// Its runtime is the independent Panel and Harness Router gateway.
package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegatewayassets"
	"github.com/boxvtk621/homelab-telegram-panel/internal/panel"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	os.Exit(execute(ctx, os.Args[1:], os.LookupEnv, os.Stdout, currentEffectiveUID()))
}

type dependencies struct {
	StaticHandler http.Handler
	EffectiveUID  int
}

func gatewayDependencies(effectiveUID int) (dependencies, error) {
	staticHandler, err := mobilegatewayassets.NewHandler()
	if err != nil {
		return dependencies{}, err
	}
	return dependencies{staticHandler, effectiveUID}, nil
}

func execute(ctx context.Context, args []string, lookup func(string) (string, bool), out io.Writer, uid int) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(out, "COMMAND_INVALID")
		return 2
	}
	if args[0] == "version" {
		_, _ = fmt.Fprintln(out, "homelab-panel", buildinfo.Version)
		return 0
	}
	if args[0] != "serve" && args[0] != "validate" && args[0] != "router-bootstrap" && args[0] != "harness-preflight" {
		_, _ = fmt.Fprintln(out, "COMMAND_INVALID")
		return 2
	}
	cfg, err := panel.Load(lookup)
	if err != nil {
		_, _ = fmt.Fprintln(out, "CONFIG_INVALID")
		return 2
	}
	if (args[0] == "serve" || args[0] == "router-bootstrap") && uid <= 0 {
		_, _ = fmt.Fprintln(out, "PRIVILEGED_PROCESS")
		return 1
	}
	if args[0] == "router-bootstrap" {
		if err := panel.BootstrapRouter(cfg); err != nil {
			_, _ = fmt.Fprintln(out, "ROUTER_BOOTSTRAP_FAILED")
			return 1
		}
		_, _ = fmt.Fprintln(out, "ROUTER_BOOTSTRAPPED")
		return 0
	}
	if args[0] == "harness-preflight" {
		if err := panel.PreflightHarness(ctx, cfg); err != nil {
			_, _ = fmt.Fprintln(out, "HARNESS_PREFLIGHT_FAILED")
			return 1
		}
		_, _ = fmt.Fprintln(out, "HARNESS_PREFLIGHT_OK")
		return 0
	}
	deps, err := gatewayDependencies(uid)
	if err != nil {
		_, _ = fmt.Fprintln(out, "ASSETS_INVALID")
		return 70
	}
	h, err := panel.New(cfg, deps.StaticHandler)
	if err != nil {
		_, _ = fmt.Fprintln(out, "CONFIG_INVALID")
		return 2
	}
	defer h.Close()
	if args[0] == "validate" {
		_, _ = fmt.Fprintln(out, "CONFIG_VALID")
		return 0
	}
	server := &http.Server{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	if cfg.TLSCertificate != "" {
		certificate, err := tls.LoadX509KeyPair(cfg.TLSCertificate, cfg.TLSKey)
		if err != nil {
			_, _ = fmt.Fprintln(out, "TLS_CONFIG_INVALID")
			return 2
		}
		server.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}}
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if server.Shutdown(shutdown) != nil {
				_ = server.Close()
			}
		case <-done:
		}
	}()
	_, _ = fmt.Fprintln(out, "PANEL_STARTED")
	if server.TLSConfig != nil {
		err = server.ListenAndServeTLS("", "")
	} else {
		err = server.ListenAndServe()
	}
	close(done)
	if err != nil && err != http.ErrServerClosed {
		_, _ = fmt.Fprintln(out, "PANEL_FAILED")
		return 1
	}
	return 0
}
