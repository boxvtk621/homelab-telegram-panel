// Package mobilegatewayconfig loads the non-secret, fail-closed configuration
// for the standalone Mobile Gateway process.
package mobilegatewayconfig

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/boxvtk621/homelab-telegram-panel/internal/mobileauth"
)

const (
	EnvListenAddress            = "FIXIK_NEXT_MOBILE_LISTEN_ADDRESS"
	EnvPublicOrigin             = "FIXIK_NEXT_MOBILE_PUBLIC_ORIGIN"
	EnvControllerBusinessSocket = "FIXIK_NEXT_MOBILE_CONTROLLER_BUSINESS_SOCKET"
	EnvControllerHealthSocket   = "FIXIK_NEXT_MOBILE_CONTROLLER_HEALTH_SOCKET"
	EnvControllerControlSocket  = "FIXIK_NEXT_MOBILE_CONTROLLER_CONTROL_SOCKET"
	EnvControllerRecoverySocket = "FIXIK_NEXT_MOBILE_CONTROLLER_RECOVERY_SOCKET"
	EnvTelegramBotID            = "FIXIK_NEXT_MOBILE_TELEGRAM_BOT_ID"
	EnvTelegramOwnerID          = "FIXIK_NEXT_MOBILE_TELEGRAM_OWNER_ID"
	EnvTelegramEnvironment      = "FIXIK_NEXT_MOBILE_TELEGRAM_ENVIRONMENT"
	EnvShutdownTimeoutMS        = "FIXIK_NEXT_MOBILE_SHUTDOWN_TIMEOUT_MS"
	EnvDialogCreationEnabled    = "FIXIK_NEXT_MOBILE_DIALOG_CREATION_ENABLED"
	EnvTaskSubmissionEnabled    = "FIXIK_NEXT_MOBILE_TASK_SUBMISSION_ENABLED"

	defaultShutdownTimeout = 10 * time.Second
	maximumShutdownTimeout = 60 * time.Second
	maximumTelegramID      = int64(1<<52 - 1)
	maximumUnixSocketPath  = 100
)

// LookupEnv makes configuration loading deterministic in tests.
type LookupEnv func(string) (string, bool)

// Config deliberately contains no bot token, database location, worker
// credential, proxy identity, or configurable Telegram verification key.
type Config struct {
	ListenAddress            string
	PublicOrigin             string
	ControllerBusinessSocket string
	ControllerHealthSocket   string
	ControllerControlSocket  string
	ControllerRecoverySocket string
	TelegramBotID            int64
	TelegramOwnerID          int64
	TelegramEnvironment      mobileauth.Environment
	ShutdownTimeout          time.Duration
	DialogCreationEnabled    bool
	TaskSubmissionEnabled    bool
}

// Load requires every authority and network boundary to be explicit. Only the
// bounded graceful-shutdown duration has a reviewed default.
func Load(lookup LookupEnv) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("mobile Gateway environment lookup is not configured")
	}

	listenAddress, err := requiredExact(lookup, EnvListenAddress)
	if err != nil {
		return Config{}, err
	}
	if err := validateLoopbackAddress(listenAddress); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvListenAddress, err)
	}
	publicOrigin, err := requiredExact(lookup, EnvPublicOrigin)
	if err != nil {
		return Config{}, err
	}
	if err := validatePublicOrigin(publicOrigin); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvPublicOrigin, err)
	}
	businessSocket, err := requiredExact(lookup, EnvControllerBusinessSocket)
	if err != nil {
		return Config{}, err
	}
	if err := validateUnixSocketPath(businessSocket); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvControllerBusinessSocket, err)
	}
	healthSocket, err := requiredExact(lookup, EnvControllerHealthSocket)
	if err != nil {
		return Config{}, err
	}
	if err := validateUnixSocketPath(healthSocket); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvControllerHealthSocket, err)
	}
	controlSocket, err := requiredExact(lookup, EnvControllerControlSocket)
	if err != nil {
		return Config{}, err
	}
	if err := validateUnixSocketPath(controlSocket); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvControllerControlSocket, err)
	}
	recoverySocket, err := requiredExact(lookup, EnvControllerRecoverySocket)
	if err != nil {
		return Config{}, err
	}
	if err := validateUnixSocketPath(recoverySocket); err != nil {
		return Config{}, fmt.Errorf("%s is invalid: %w", EnvControllerRecoverySocket, err)
	}
	sockets := []string{businessSocket, healthSocket, controlSocket, recoverySocket}
	for left := range sockets {
		for right := left + 1; right < len(sockets); right++ {
			if sockets[left] == sockets[right] {
				return Config{}, errors.New("Controller socket classes must use four distinct paths")
			}
		}
	}

	botID, err := requiredTelegramID(lookup, EnvTelegramBotID)
	if err != nil {
		return Config{}, err
	}
	ownerID, err := requiredTelegramID(lookup, EnvTelegramOwnerID)
	if err != nil {
		return Config{}, err
	}
	environmentText, err := requiredExact(lookup, EnvTelegramEnvironment)
	if err != nil {
		return Config{}, err
	}
	environment := mobileauth.Environment(environmentText)
	if environment != mobileauth.EnvironmentTest && environment != mobileauth.EnvironmentProduction {
		return Config{}, fmt.Errorf("%s must be exactly test or production", EnvTelegramEnvironment)
	}

	shutdownTimeout := defaultShutdownTimeout
	if raw, ok := lookup(EnvShutdownTimeoutMS); ok {
		if raw == "" || strings.TrimSpace(raw) != raw {
			return Config{}, fmt.Errorf("%s must be an exact positive integer", EnvShutdownTimeoutMS)
		}
		milliseconds, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || milliseconds < 1 || milliseconds > maximumShutdownTimeout.Milliseconds() {
			return Config{}, fmt.Errorf("%s must be an integer from 1 to %d", EnvShutdownTimeoutMS, maximumShutdownTimeout.Milliseconds())
		}
		shutdownTimeout = time.Duration(milliseconds) * time.Millisecond
	}

	dialogCreationEnabled := false
	if raw, ok := lookup(EnvDialogCreationEnabled); ok {
		if raw != "true" && raw != "false" {
			return Config{}, fmt.Errorf("%s must be true or false", EnvDialogCreationEnabled)
		}
		dialogCreationEnabled = raw == "true"
	}
	taskSubmissionEnabled := false
	if raw, ok := lookup(EnvTaskSubmissionEnabled); ok {
		if raw != "true" && raw != "false" {
			return Config{}, fmt.Errorf("%s must be true or false", EnvTaskSubmissionEnabled)
		}
		taskSubmissionEnabled = raw == "true"
	}
	if taskSubmissionEnabled && !dialogCreationEnabled {
		return Config{}, fmt.Errorf("submission requires dialog commands")
	}
	return Config{
		TaskSubmissionEnabled:    taskSubmissionEnabled,
		DialogCreationEnabled:    dialogCreationEnabled,
		ListenAddress:            listenAddress,
		PublicOrigin:             publicOrigin,
		ControllerBusinessSocket: businessSocket,
		ControllerHealthSocket:   healthSocket,
		ControllerControlSocket:  controlSocket,
		ControllerRecoverySocket: recoverySocket,
		TelegramBotID:            botID,
		TelegramOwnerID:          ownerID,
		TelegramEnvironment:      environment,
		ShutdownTimeout:          shutdownTimeout,
	}, nil
}

func requiredExact(lookup LookupEnv, key string) (string, error) {
	value, ok := lookup(key)
	if !ok || value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	if strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s must not contain surrounding whitespace", key)
	}
	return value, nil
}

func requiredTelegramID(lookup LookupEnv, key string) (int64, error) {
	raw, err := requiredExact(lookup, key)
	if err != nil {
		return 0, err
	}
	value, parseErr := strconv.ParseInt(raw, 10, 64)
	if parseErr != nil || value < 1 || value > maximumTelegramID || strconv.FormatInt(value, 10) != raw {
		return 0, fmt.Errorf("%s must be a canonical positive 52-bit integer", key)
	}
	return value, nil
}

func validateLoopbackAddress(address string) error {
	host, portText, err := net.SplitHostPort(address)
	if err != nil || host == "" || portText == "" {
		return errors.New("address must contain an explicit loopback IP and port")
	}
	ip := net.ParseIP(host)
	port, portErr := strconv.Atoi(portText)
	if ip == nil || !ip.IsLoopback() || portErr != nil || port < 1 || port > 65535 {
		return errors.New("address must contain an explicit loopback IP and port")
	}
	if net.JoinHostPort(ip.String(), strconv.Itoa(port)) != address {
		return errors.New("address must use canonical IP and port spelling")
	}
	return nil
}

func validatePublicOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.Hostname() == "" ||
		parsed.User != nil || parsed.Opaque != "" || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("origin must be one exact HTTPS origin without path, credentials, query, or fragment")
	}
	if parsed.String() != origin || origin != "https://"+parsed.Host {
		return errors.New("origin must use canonical HTTPS origin spelling")
	}
	portText := parsed.Port()
	if strings.HasSuffix(parsed.Host, ":") {
		return errors.New("origin port is invalid")
	}
	if portText != "" {
		port, portErr := strconv.Atoi(portText)
		if portErr != nil || port < 1 || port > 65535 || strconv.Itoa(port) != portText {
			return errors.New("origin port is invalid")
		}
	}
	return nil
}

func validateUnixSocketPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsRune(path, '\x00') || len(path) > maximumUnixSocketPath {
		return errors.New("path must be a clean absolute Unix socket path of at most 100 bytes")
	}
	return nil
}
