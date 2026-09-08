// Command fixik-next-mobile-gateway is the only executable entrypoint for the
// production-inactive Mobile Gateway. It starts only when invoked explicitly.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegatewayassets"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegatewaybootstrap"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	dependencies, err := gatewayDependencies(currentEffectiveUID())
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Mobile Gateway embedded frontend is invalid")
		os.Exit(mobilegatewaybootstrap.ExitInternal)
	}
	os.Exit(mobilegatewaybootstrap.Execute(ctx, os.Args[1:], os.LookupEnv, os.Stdout, dependencies))
}

func gatewayDependencies(effectiveUID int) (mobilegatewaybootstrap.Dependencies, error) {
	staticHandler, err := mobilegatewayassets.NewHandler()
	if err != nil {
		return mobilegatewaybootstrap.Dependencies{}, err
	}
	return mobilegatewaybootstrap.DefaultDependencies(staticHandler, effectiveUID), nil
}
