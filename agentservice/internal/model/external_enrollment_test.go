package model

import (
	"strings"
	"testing"
)

func externalEnrollmentFixture() ExternalEnrollmentRequest {
	return ExternalEnrollmentRequest{
		SchemaID: ExternalEnrollmentSchemaID, OperationID: "r10-enrollment",
		HostID: "10000000-0000-4000-8000-000000000001", ExpectedHostVersion: 3,
		NodeID: "20000000-0000-4000-8000-000000000001", Name: "External Codex",
		Adapter: "codex", EndpointURI: "https://10.20.30.40:9443", CertificateSHA256: strings.Repeat("a", 64),
	}
}

func TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI(t *testing.T) {
	for _, value := range []string{
		"https://harness.internal:9443",
		"https://8.8.8.8:9443",
		"https://169.254.1.2:9443",
		"https://0.0.0.0:9443",
		"https://user@10.20.30.40:9443",
		"https://10.20.30.40:9443/",
		"https://10.20.30.40:9443?next=1",
		"https://10.20.30.40:09443",
		"https://[::ffff:10.20.30.40]:9443",
	} {
		t.Run(value, func(t *testing.T) {
			if _, _, err := ExternalEndpoint(value, "local"); err == nil {
				t.Fatal("unsafe or non-canonical endpoint accepted")
			}
		})
	}
	if canonical, address, err := ExternalEndpoint("https://10.20.30.40:9443", "local"); err != nil || canonical != "https://10.20.30.40:9443" || address != "10.20.30.40:9443" {
		t.Fatalf("private endpoint rejected: canonical=%q address=%q err=%v", canonical, address, err)
	}
}

func TestSSHExternalEndpointIsRemoteLoopbackOnly(t *testing.T) {
	if _, _, err := ExternalEndpoint("https://10.20.30.40:9443", "ssh"); err == nil {
		t.Fatal("SSH endpoint escaped remote loopback")
	}
	if canonical, address, err := ExternalEndpoint("https://127.0.0.1:9443", "ssh"); err != nil || canonical != "https://127.0.0.1:9443" || address != "127.0.0.1:9443" {
		t.Fatalf("remote loopback rejected: canonical=%q address=%q err=%v", canonical, address, err)
	}
}

func TestExternalEnrollmentRequestAndBindingHashesFenceIdentity(t *testing.T) {
	request := externalEnrollmentFixture()
	if err := ValidateExternalEnrollmentRequest(request); err != nil {
		t.Fatal(err)
	}
	first, err := ExternalEnrollmentRequestHash(request)
	if err != nil {
		t.Fatal(err)
	}
	request.CertificateSHA256 = strings.Repeat("b", 64)
	second, err := ExternalEnrollmentRequestHash(request)
	if err != nil || first == second {
		t.Fatal("request hash did not bind certificate identity")
	}
	binding := ExternalEndpointBinding{
		Kind: "external", NodeID: request.NodeID, RegistrationRevision: 1, RegistrationEpoch: 1, EndpointRevision: 1,
		HostID: request.HostID, HostVersion: request.ExpectedHostVersion, Transport: "local", TargetRef: "local-host",
		Address: "10.20.30.40:9443",
	}
	digest, err := ExternalBindingSHA256(binding)
	if err != nil {
		t.Fatal(err)
	}
	binding.HostVersion++
	changed, err := ExternalBindingSHA256(binding)
	if err != nil || digest == changed {
		t.Fatal("binding hash did not bind exact host version")
	}
}
