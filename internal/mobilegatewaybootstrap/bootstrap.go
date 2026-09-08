// Package mobilegatewaybootstrap wires the standalone, non-authoritative
// Mobile Gateway process. Nothing imports this package except its dedicated
// executable.
package mobilegatewaybootstrap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/buildinfo"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilecontrollerclient"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegateway"
	"github.com/boxvtk621/homelab-telegram-panel/internal/mobilegatewayconfig"
	"github.com/boxvtk621/homelab-telegram-panel/internal/observability"
)

const (
	ExitOK       = 0
	ExitRuntime  = 1
	ExitUsage    = 2
	ExitInternal = 70

	capabilityVersion = 1

	controllerCapabilitiesVersion = "mobile-workspace-r1-v1"
)

// ListenFunc opens the already-reviewed explicit loopback address.
type ListenFunc func(network, address string) (net.Listener, error)

// Dependencies contains only process mechanics and the injected static
// frontend. Authority, credentials, sockets, and public identity remain in the
// validated configuration.
type Dependencies struct {
	StaticHandler http.Handler
	Clock         mobileauth.Clock
	IDs           mobilegateway.IDGenerator
	Listen        ListenFunc
	EffectiveUID  int
}

// DefaultDependencies returns production mechanics for the dedicated binary.
// The caller supplies the real effective UID so serve can fail closed when the
// process is privileged, and supplies the static handler independently.
func DefaultDependencies(staticHandler http.Handler, effectiveUID int) Dependencies {
	return Dependencies{
		StaticHandler: staticHandler,
		Clock:         mobileauth.ClockFunc(time.Now),
		IDs:           mobilegateway.UUIDGenerator{},
		Listen:        net.Listen,
		EffectiveUID:  effectiveUID,
	}
}

// Execute runs serve, validate, or version and returns a stable process code.
func Execute(
	ctx context.Context,
	args []string,
	lookup mobilegatewayconfig.LookupEnv,
	output io.Writer,
	dependencies Dependencies,
) int {
	if ctx == nil {
		ctx = context.Background()
	}
	logger, err := observability.New(output, observability.WithClock(dependencies.Clock))
	if err != nil {
		return ExitInternal
	}
	if isNil(dependencies.IDs) {
		_ = logFailure(ctx, logger, "mobile_gateway.bootstrap.finished", "DEPENDENCY_INVALID", errors.New("ID generator is not configured"))
		return ExitInternal
	}
	traceID, err := dependencies.IDs.New("trace")
	if err != nil {
		_ = logFailure(ctx, logger, "mobile_gateway.bootstrap.finished", "TRACE_ID_FAILED", err)
		return ExitInternal
	}
	ctx = observability.WithCausalContext(ctx, observability.CausalContext{TraceID: traceID})

	mode, ok := parseMode(args)
	if !ok {
		_ = logger.Log(ctx, observability.Event{
			Level: observability.LevelError, Name: "mobile_gateway.command.rejected",
			Outcome: observability.OutcomeFailed, ErrorCode: "COMMAND_INVALID",
			Attributes: map[string]any{"allowed": []string{"serve", "validate", "version"}},
		})
		return ExitUsage
	}
	if mode == "version" {
		if err := logger.Log(ctx, observability.Event{
			Level: observability.LevelInfo, Name: "mobile_gateway.version.reported",
			Outcome:    observability.OutcomeSucceeded,
			Attributes: map[string]any{"version": buildinfo.Version},
		}); err != nil {
			return ExitInternal
		}
		return ExitOK
	}

	configuration, err := mobilegatewayconfig.Load(lookup)
	if err != nil {
		_ = logFailure(ctx, logger, "mobile_gateway.configuration.rejected", "CONFIG_INVALID", err)
		return ExitUsage
	}
	if mode == "serve" && dependencies.EffectiveUID <= 0 {
		_ = logFailure(ctx, logger, "mobile_gateway.process.rejected", "PRIVILEGED_PROCESS", errors.New("Mobile Gateway must run as an unprivileged account"))
		return ExitRuntime
	}

	runtime, err := buildRuntime(configuration, dependencies, logger)
	if err != nil {
		_ = logFailure(ctx, logger, "mobile_gateway.bootstrap.finished", "BOOTSTRAP_FAILED", err)
		return ExitInternal
	}
	defer runtime.close()

	if mode == "validate" {
		if err := logger.Log(ctx, observability.Event{
			Level: observability.LevelInfo, Name: "mobile_gateway.configuration.validated",
			Outcome: observability.OutcomeSucceeded,
		}); err != nil {
			return ExitInternal
		}
		return ExitOK
	}

	if err := logger.Log(ctx, observability.Event{
		Level: observability.LevelInfo, Name: "mobile_gateway.process.started",
		Outcome: observability.OutcomeStarted,
	}); err != nil {
		return ExitInternal
	}
	if err := runtime.serve(ctx); err != nil {
		outcome := observability.OutcomeFailed
		errorCode := "PROCESS_FAILED"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			outcome = observability.OutcomeCancelled
			errorCode = "PROCESS_CANCELLED"
		}
		_ = logger.Log(context.WithoutCancel(ctx), observability.Event{
			Level: observability.LevelError, Name: "mobile_gateway.process.finished",
			Outcome: outcome, ErrorCode: errorCode, Err: err,
		})
		return ExitRuntime
	}
	if err := logger.Log(context.WithoutCancel(ctx), observability.Event{
		Level: observability.LevelInfo, Name: "mobile_gateway.process.finished",
		Outcome: observability.OutcomeSucceeded,
	}); err != nil {
		return ExitInternal
	}
	return ExitOK
}

type gatewayRuntime struct {
	server          *http.Server
	controller      *mobilecontrollerclient.Client
	identityCheck   func(context.Context) error
	sessions        *mobileauth.SessionManager
	listen          ListenFunc
	shutdownTimeout time.Duration
}

func buildRuntime(
	configuration mobilegatewayconfig.Config,
	dependencies Dependencies,
	logger *observability.Logger,
) (*gatewayRuntime, error) {
	if isNil(dependencies.StaticHandler) || isNil(dependencies.Clock) || isNil(dependencies.IDs) ||
		dependencies.Listen == nil || logger == nil {
		return nil, errors.New("Mobile Gateway runtime dependency is not configured")
	}

	expectedPrincipal := "telegram-user:" + strconv.FormatInt(configuration.TelegramOwnerID, 10)
	expectedCapabilities := controllerCapabilitiesVersion
	if configuration.DialogCreationEnabled {
		expectedCapabilities = "mobile-workspace-dialog-create-v1"
	}
	if configuration.TaskSubmissionEnabled {
		if !configuration.DialogCreationEnabled {
			return nil, errors.New("submission requires dialog commands")
		}
		expectedCapabilities = "mobile-workspace-task-submit-v1"
	}
	controller, err := mobilecontrollerclient.New(
		configuration.ControllerBusinessSocket,
		configuration.ControllerHealthSocket,
		configuration.ControllerControlSocket,
		configuration.ControllerRecoverySocket,
		expectedPrincipal,
		expectedCapabilities,
	)
	if err != nil {
		return nil, fmt.Errorf("construct Controller client: %w", err)
	}
	closeController := true
	defer func() {
		if closeController {
			controller.CloseIdleConnections()
		}
	}()

	verifier, err := mobileauth.NewTelegramVerifier(
		configuration.TelegramBotID,
		configuration.TelegramEnvironment,
		[]int64{configuration.TelegramOwnerID},
		dependencies.Clock,
	)
	if err != nil {
		return nil, fmt.Errorf("construct Telegram verifier: %w", err)
	}
	sessions, err := mobileauth.NewSessionManager(mobileauth.SessionConfig{
		CapabilityVersion: capabilityVersion,
		Clock:             dependencies.Clock,
	})
	if err != nil {
		return nil, fmt.Errorf("construct in-memory sessions: %w", err)
	}
	capacity := mobilegateway.NewCapacityGate()
	auditor, err := mobilegateway.NewObservabilityAuthAuditor(logger)
	if err != nil {
		return nil, fmt.Errorf("construct authentication auditor: %w", err)
	}
	auth, err := mobilegateway.NewAuthHandler(
		mobilegateway.AuthConfig{
			PublicOrigin:          configuration.PublicOrigin,
			Capacity:              capacity,
			Audit:                 auditor,
			DialogCreationEnabled: configuration.DialogCreationEnabled,
			TaskSubmissionEnabled: configuration.TaskSubmissionEnabled,
		},
		verifier,
		sessions,
		dependencies.IDs,
		dependencies.Clock,
	)
	if err != nil {
		return nil, fmt.Errorf("construct authentication handler: %w", err)
	}
	boundController := &principalBoundController{
		Client:               controller,
		expectedPrincipal:    expectedPrincipal,
		expectedCapabilities: expectedCapabilities,
	}
	reads, err := mobilegateway.NewReadHandler(auth, boundController)
	if err != nil {
		return nil, fmt.Errorf("construct read handler: %w", err)
	}

	mux := http.NewServeMux()
	if err := auth.Register(mux); err != nil {
		return nil, fmt.Errorf("register authentication routes: %w", err)
	}
	if configuration.DialogCreationEnabled {
		commands, err := mobilegateway.NewDialogCommandsHandler(reads, boundController)
		if err != nil {
			return nil, err
		}
		if configuration.TaskSubmissionEnabled {
			submissions, err := mobilegateway.NewSubmissionHandler(commands, boundController)
			if err != nil {
				return nil, err
			}
			if err := submissions.Register(mux); err != nil {
				return nil, err
			}
		} else if err := commands.Register(mux); err != nil {
			return nil, err
		}
	} else {
		if err := reads.Register(mux); err != nil {
			return nil, fmt.Errorf("register read routes: %w", err)
		}
	}
	// The read handler owns the generic public JSON rejection contract as well
	// as the frozen routes. Unknown API paths therefore never fall through to
	// stdlib text/plain or the embedded frontend.
	mux.Handle("/api", reads)
	mux.Handle("/api/", reads)
	publicHost := strings.TrimPrefix(configuration.PublicOrigin, "https://")
	mux.Handle("/", exactHostHandler{host: publicHost, next: dependencies.StaticHandler})

	server, err := mobilegateway.NewHardenedServer(
		configuration.ListenAddress,
		strictCanonicalHandler{next: mux, apiFallback: reads},
	)
	if err != nil {
		return nil, fmt.Errorf("construct hardened server: %w", err)
	}
	closeController = false
	return &gatewayRuntime{
		server: server, controller: controller,
		identityCheck: func(ctx context.Context) error {
			requestID, err := dependencies.IDs.New("request")
			if err != nil {
				return errors.New("Controller identity is unavailable")
			}
			return boundController.verify(ctx, requestID)
		},
		sessions: sessions, listen: dependencies.Listen,
		shutdownTimeout: configuration.ShutdownTimeout,
	}, nil
}

func (runtime *gatewayRuntime) serve(ctx context.Context) error {
	if runtime == nil || runtime.server == nil || runtime.controller == nil || runtime.identityCheck == nil ||
		runtime.listen == nil || runtime.shutdownTimeout <= 0 {
		return errors.New("Mobile Gateway runtime is not initialized")
	}
	select {
	case <-ctx.Done():
		return nil
	default:
	}
	if err := runtime.identityCheck(ctx); err != nil {
		return errors.New("Mobile Gateway Controller identity check failed")
	}
	listener, err := runtime.listen("tcp", runtime.server.Addr)
	if err != nil {
		return errors.New("Mobile Gateway listener could not start")
	}
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- runtime.server.Serve(listener)
	}()

	select {
	case err := <-serveResult:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		_ = runtime.server.Close()
		return errors.New("Mobile Gateway server stopped unexpectedly")
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), runtime.shutdownTimeout)
		shutdownErr := runtime.server.Shutdown(shutdownContext)
		cancel()
		if shutdownErr != nil {
			_ = runtime.server.Close()
			<-serveResult
			return errors.New("Mobile Gateway graceful shutdown failed")
		}
		serveErr := <-serveResult
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			return errors.New("Mobile Gateway server stopped unexpectedly")
		}
		return nil
	}
}

// principalBoundController keeps the browser actor outside the private UDS
// contract. The expected authority comes only from validated process config;
// Controller identity comes only from the authenticated health socket.
type principalBoundController struct {
	*mobilecontrollerclient.Client
	expectedPrincipal    string
	expectedCapabilities string
}

func (controller *principalBoundController) verify(ctx context.Context, requestID string) error {
	if controller == nil || controller.Client == nil || ctx == nil || controller.expectedPrincipal == "" {
		return errors.New("Controller identity is unavailable")
	}
	identity, err := controller.Client.Identity(ctx, requestID)
	if err != nil || identity.Principal != controller.expectedPrincipal ||
		identity.CapabilitiesVersion != controller.expectedCapabilities {
		return errors.New("Controller identity is unavailable")
	}
	return nil
}

func (controller *principalBoundController) Ready(
	ctx context.Context,
	requestID string,
) (mobilecontrollerclient.Status, error) {
	if err := controller.verify(ctx, requestID); err != nil {
		return mobilecontrollerclient.Status{}, err
	}
	return controller.Client.Ready(ctx, requestID)
}

func (runtime *gatewayRuntime) close() {
	if runtime == nil {
		return
	}
	if runtime.controller != nil {
		runtime.controller.CloseIdleConnections()
	}
	if runtime.server != nil {
		_ = runtime.server.Close()
	}
}

type strictCanonicalHandler struct {
	next        http.Handler
	apiFallback http.Handler
}

func (handler strictCanonicalHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request == nil || request.URL == nil || request.URL.RawPath != "" || request.URL.Path == "" ||
		path.Clean(request.URL.Path) != request.URL.Path || strings.Contains(request.URL.Path, "//") {
		if request != nil && request.URL != nil && strings.HasPrefix(request.URL.Path, "/api/") && handler.apiFallback != nil {
			rejected := request.Clone(request.Context())
			rejectedURL := *request.URL
			rejectedURL.Path = "/api/noncanonical-rejected"
			rejectedURL.RawPath = ""
			rejectedURL.RawQuery = ""
			rejected.URL = &rejectedURL
			handler.apiFallback.ServeHTTP(response, rejected)
			return
		}
		http.NotFound(response, request)
		return
	}
	handler.next.ServeHTTP(response, request)
}

type exactHostHandler struct {
	host string
	next http.Handler
}

func (handler exactHostHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request == nil || request.Host != handler.host || handler.host == "" || handler.next == nil {
		header := response.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		header.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=()")
		header.Set("Referrer-Policy", "no-referrer")
		header.Set("X-Content-Type-Options", "nosniff")
		response.WriteHeader(http.StatusNotFound)
		return
	}
	handler.next.ServeHTTP(response, request)
}

func parseMode(args []string) (string, bool) {
	if len(args) != 1 {
		return "", false
	}
	switch args[0] {
	case "serve", "validate", "version":
		return args[0], true
	default:
		return "", false
	}
}

func logFailure(ctx context.Context, logger *observability.Logger, name, code string, err error) error {
	return logger.Log(ctx, observability.Event{
		Level: observability.LevelError, Name: name,
		Outcome: observability.OutcomeFailed, ErrorCode: code, Err: err,
	})
}

func isNil(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
