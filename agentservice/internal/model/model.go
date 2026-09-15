// Package model defines the R01 agent-management contract. It deliberately
// contains no execution, Router, Docker, or secret-management interfaces.
package model

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ImportSchemaID    = "agent-registry-import-v1"
	InventorySchemaID = "agent-management-v1"
	BindingsSchemaID  = "agent-dialog-bindings-v1"
	RetirementSchema  = "agent-retirement-v1"
	StaleAfter        = 15 * time.Second
	MaximumSafeInt    = int64(1<<53 - 1)
)

var (
	uuidPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	actorPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@-]{0,127}$`)
	sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type RegistryNode struct {
	NodeID               string `json:"nodeId"`
	Name                 string `json:"name"`
	Adapter              string `json:"adapter"`
	URL                  string `json:"url"`
	CertificateSHA256    string `json:"certificateSHA256"`
	RegistrationRevision int64  `json:"registrationRevision,omitempty"`
	RegistrationEpoch    int64  `json:"registrationEpoch,omitempty"`
	Compatibility        string `json:"compatibility,omitempty"`
}

type RegistryManifest struct {
	SchemaID         string         `json:"schemaId,omitempty"`
	RegistryVersion  int64          `json:"registryVersion"`
	OwnerID          string         `json:"ownerId"`
	Mode             string         `json:"mode"`
	WireSchemaSHA256 string         `json:"wireSchemaSHA256,omitempty"`
	Nodes            []RegistryNode `json:"nodes"`
}

type HostSeed struct {
	HostID string `json:"hostId"`
	Name   string `json:"name"`
}

type DialogSeed struct {
	NodeDialogID string `json:"nodeDialogId"`
}

type ObservationSeed struct {
	Process      string `json:"process"`
	Connection   string `json:"connection"`
	Readiness    string `json:"readiness"`
	Occupancy    string `json:"occupancy"`
	ObservedAt   string `json:"observedAt"`
	Source       string `json:"source"`
	PendingCount *int64 `json:"pendingCount"`
}

type NodeSeed struct {
	NodeID           string           `json:"nodeId"`
	HostID           string           `json:"hostId"`
	RegistrationMode string           `json:"registrationMode"`
	Observation      *ObservationSeed `json:"observation"`
	Dialogs          []DialogSeed     `json:"dialogs"`
}

type ImportSnapshot struct {
	SchemaID string     `json:"schemaId"`
	Complete bool       `json:"complete"`
	Hosts    []HostSeed `json:"hosts"`
	Nodes    []NodeSeed `json:"nodes"`
}

func ValidUUID(value string) bool {
	return uuidPattern.MatchString(value)
}

func ValidActor(value string) bool {
	return actorPattern.MatchString(value)
}

func validText(value string, maximum int) bool {
	return value != "" && utf8.ValidString(value) && len(value) <= maximum &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func ValidateSnapshot(manifest RegistryManifest, snapshot ImportSnapshot) error {
	if snapshot.SchemaID != ImportSchemaID || !snapshot.Complete ||
		snapshot.Hosts == nil || snapshot.Nodes == nil ||
		len(snapshot.Hosts) == 0 || len(snapshot.Nodes) != len(manifest.Nodes) {
		return errors.New("invalid complete import snapshot")
	}

	hosts := make(map[string]bool, len(snapshot.Hosts))
	for _, host := range snapshot.Hosts {
		if !ValidUUID(host.HostID) || hosts[host.HostID] || !validText(host.Name, 200) {
			return errors.New("invalid import host")
		}
		hosts[host.HostID] = true
	}

	manifestNodes := make(map[string]bool, len(manifest.Nodes))
	for _, node := range manifest.Nodes {
		manifestNodes[node.NodeID] = true
	}
	seenNodes := make(map[string]bool, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		if !manifestNodes[node.NodeID] || seenNodes[node.NodeID] || !hosts[node.HostID] ||
			(node.RegistrationMode != "legacy_readonly" && node.RegistrationMode != "compatible") ||
			node.Dialogs == nil {
			return errors.New("invalid import node")
		}
		for _, registered := range manifest.Nodes {
			if registered.NodeID == node.NodeID && registered.Compatibility != "" && registered.Compatibility != node.RegistrationMode {
				return errors.New("import compatibility does not match signed registration")
			}
		}
		seenNodes[node.NodeID] = true
		dialogs := map[string]bool{}
		for _, dialog := range node.Dialogs {
			if !ValidUUID(dialog.NodeDialogID) || dialogs[dialog.NodeDialogID] {
				return errors.New("invalid import dialog")
			}
			dialogs[dialog.NodeDialogID] = true
		}
		if node.Observation != nil {
			if err := ValidateObservation(*node.Observation); err != nil {
				return err
			}
		}
	}
	return nil
}

func ValidateObservation(observation ObservationSeed) error {
	if !slices.Contains([]string{"running", "stopped", "unknown"}, observation.Process) ||
		!slices.Contains([]string{"online", "offline", "unknown"}, observation.Connection) ||
		!slices.Contains([]string{"ready", "unready", "unknown"}, observation.Readiness) ||
		!slices.Contains([]string{"idle", "busy", "unknown"}, observation.Occupancy) ||
		!validText(observation.Source, 100) {
		return errors.New("invalid import observation")
	}
	if !consistentAxes(observation.Process, observation.Connection, observation.Readiness, observation.Occupancy) {
		return errors.New("conflicting import observation")
	}
	if _, err := time.Parse(time.RFC3339Nano, observation.ObservedAt); err != nil {
		return errors.New("invalid import observation time")
	}
	if observation.PendingCount != nil && (*observation.PendingCount < 0 || *observation.PendingCount > MaximumSafeInt) {
		return errors.New("invalid import pending count")
	}
	return nil
}

type StoredObservation struct {
	Process      string
	Connection   string
	Readiness    string
	Occupancy    string
	ObservedAt   *time.Time
	Source       *string
	PendingCount *int64
}

func DeriveStatus(now time.Time, registrationMode string, observation StoredObservation) string {
	if registrationMode == "legacy_readonly" {
		return "readonly"
	}
	if observation.ObservedAt == nil || observation.Source == nil ||
		observation.ObservedAt.After(now.Add(time.Minute)) {
		return "unknown"
	}
	if !consistentAxes(observation.Process, observation.Connection, observation.Readiness, observation.Occupancy) {
		return "unknown"
	}
	if now.Sub(*observation.ObservedAt) > StaleAfter {
		return "stale"
	}
	if observation.Process == "stopped" {
		return "stopped"
	}
	if observation.Process != "running" || observation.Connection != "online" {
		return "unknown"
	}
	if observation.Readiness == "unready" {
		return "unready"
	}
	if observation.Readiness == "ready" && observation.Occupancy == "busy" {
		return "busy"
	}
	if observation.Readiness == "ready" && observation.Occupancy == "idle" {
		return "online"
	}
	return "unknown"
}

func consistentAxes(process, connection, readiness, occupancy string) bool {
	switch process {
	case "stopped":
		return connection == "offline" && readiness == "unknown" && occupancy == "unknown"
	case "unknown":
		return connection == "unknown" && readiness == "unknown" && occupancy == "unknown"
	case "running":
	default:
		return false
	}
	if connection != "online" {
		return (connection == "offline" || connection == "unknown") &&
			readiness == "unknown" && occupancy == "unknown"
	}
	switch readiness {
	case "ready":
		return occupancy == "idle" || occupancy == "busy"
	case "unready":
		return occupancy == "idle" || occupancy == "unknown"
	case "unknown":
		return occupancy == "unknown"
	default:
		return false
	}
}

type Host struct {
	HostID string `json:"hostId"`
	Name   string `json:"name"`
}

type StateAxes struct {
	Process    string `json:"process"`
	Connection string `json:"connection"`
	Readiness  string `json:"readiness"`
	Occupancy  string `json:"occupancy"`
}

type MetricInt64 struct {
	Value      int64  `json:"value"`
	ObservedAt string `json:"observedAt"`
	Source     string `json:"source"`
}

type Action struct {
	Allowed    bool   `json:"allowed"`
	Reason     string `json:"reason,omitempty"`
	NextAction string `json:"nextAction,omitempty"`
}

type Actions struct {
	OpenWorkspace Action `json:"openWorkspace"`
	SendMessage   Action `json:"sendMessage"`
	Lifecycle     Action `json:"lifecycle"`
}

func ActionsFor(status string) Actions {
	actions := Actions{
		OpenWorkspace: Action{Allowed: true},
		Lifecycle:     Action{Allowed: false, Reason: "r01_read_only", NextAction: "Управление lifecycle появится в следующих этапах."},
	}
	switch status {
	case "online", "busy":
		actions.SendMessage = Action{Allowed: true}
	case "readonly":
		actions.SendMessage = Action{Allowed: false, Reason: "readonly_registration", NextAction: "Обновите регистрацию Harness до совместимой версии."}
	case "unready":
		actions.SendMessage = Action{Allowed: false, Reason: "node_unready", NextAction: "Устраните причину неготовности и обновите состояние."}
	case "stale":
		actions.SendMessage = Action{Allowed: false, Reason: "observation_stale", NextAction: "Обновите состояние перед отправкой сообщения."}
	case "stopped":
		actions.SendMessage = Action{Allowed: false, Reason: "node_stopped", NextAction: "Запуск Harness относится к следующему lifecycle-этапу."}
	default:
		actions.SendMessage = Action{Allowed: false, Reason: "state_unknown", NextAction: "Получите подтверждённое состояние Harness."}
	}
	return actions
}

type DialogMapping struct {
	NodeDialogID    string `json:"nodeDialogId"`
	LogicalDialogID string `json:"logicalDialogId"`
	BindingVersion  int64  `json:"bindingVersion"`
}

type InventoryItem struct {
	NodeID           string       `json:"nodeId"`
	Name             string       `json:"name"`
	Engine           string       `json:"engine"`
	SourceMode       string       `json:"sourceMode"`
	Host             Host         `json:"host"`
	RegistrationMode string       `json:"registrationMode"`
	Status           string       `json:"status"`
	State            StateAxes    `json:"state"`
	ObservedAt       *string      `json:"observedAt"`
	Source           *string      `json:"source"`
	PendingCount     *MetricInt64 `json:"pendingCount"`
	Actions          Actions      `json:"actions"`
	DialogCount      int64        `json:"dialogCount"`
}

type InventoryPage struct {
	SchemaID   string          `json:"schemaId"`
	Items      []InventoryItem `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

type DialogBindingPage struct {
	SchemaID   string          `json:"schemaId"`
	NodeID     string          `json:"nodeId"`
	Items      []DialogMapping `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

type RetirementDescriptor struct {
	SchemaID              string  `json:"schemaId"`
	DescriptorVersion     int64   `json:"descriptorVersion"`
	Kind                  string  `json:"kind"`
	State                 string  `json:"state"`
	ObservedAt            string  `json:"observedAt"`
	ProcessExitObserved   *bool   `json:"processExitObserved"`
	OwnershipReleaseProof *string `json:"ownershipReleaseProof"`
}

func ValidateRetirementDescriptor(value RetirementDescriptor) error {
	if value.SchemaID != RetirementSchema || value.DescriptorVersion < 1 ||
		!slices.Contains([]string{"managed_stopped", "external_detached"}, value.Kind) ||
		!slices.Contains([]string{"pending", "verified"}, value.State) {
		return errors.New("invalid retirement descriptor")
	}
	if _, err := time.Parse(time.RFC3339Nano, value.ObservedAt); err != nil {
		return errors.New("invalid retirement observation")
	}
	if value.State != "verified" {
		return nil
	}
	switch value.Kind {
	case "managed_stopped":
		if value.ProcessExitObserved == nil || !*value.ProcessExitObserved || value.OwnershipReleaseProof != nil {
			return errors.New("managed retirement requires observed exit")
		}
	case "external_detached":
		if value.ProcessExitObserved != nil || value.OwnershipReleaseProof == nil || !validText(*value.OwnershipReleaseProof, 500) {
			return errors.New("external retirement requires ownership release proof")
		}
	default:
		return fmt.Errorf("unsupported retirement kind %q", value.Kind)
	}
	return nil
}
