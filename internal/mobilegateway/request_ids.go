package mobilegateway

import (
	"crypto/rand"
	"errors"
	"fmt"
)

// UUIDGenerator emits the canonical request identity required by the private
// Controller protocol. Prefix-hex execution IDs are not valid protocol IDs.
type UUIDGenerator struct{}

func (UUIDGenerator) New(kind string) (string, error) {
	if kind != "request" && kind != "trace" {
		return "", errors.New("unsupported mobile identity kind")
	}
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("mobile identity entropy unavailable")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", value[0:4], value[4:6], value[6:8], value[8:10], value[10:16]), nil
}
