# HL-293 R10 PostgreSQL restart and mTLS enrollment evidence

Collected: 2026-09-18 (Europe/Samara). This is isolated local evidence for the implemented R10 admission/store/Router seams. It does not claim a native compatible Harness, a combined live Harness process restart plus mTLS redial, remote SSH execution, deployed runtime, production, or owner acceptance.

## Exact candidate and worktree state

- Worktree: /Users/kondor/.codex/worktrees/hl263-r10-r14-runtime-evidence/homelab-telegram-panel
- Branch: codex/hl-263-r10-r14-runtime-evidence-20260918
- Base/HEAD: 1e180fa497b67105cad0d0b34c97d5f79b89628e
- Fresh origin/main readback: 1e180fa497b67105cad0d0b34c97d5f79b89628e
- R10 implementation c657a652de02a48ca110c2581b913c326c396f9a is an ancestor of origin/main.
- Tracked candidate diff: only agentservice/internal/store/store_integration_test.go, 247 insertions, no runtime code changes.
- Exact tracked diff SHA-256: 66bc9bbd9b465e780fb06a9811ab5f843f7218b0aa7c0a343fd79d36cbb36843.
- git diff --check: PASS.

Full status at evidence readback:

~~~text
$ git status --short --branch
## codex/hl-263-r10-r14-runtime-evidence-20260918
 M agentservice/internal/store/store_integration_test.go
?? _bmad-output/
?? _bmad/
~~~

The untracked _bmad directory is 27 generated local workflow files (284 KiB), including machine-local render paths. It is tooling only and is explicitly excluded from the candidate. _bmad-output contains this spec/evidence/deferred-work set. The disposable Go cache /private/tmp/hl263-r10-go-cache was 373 MiB at final readback and is not product state.

Environment:

~~~text
$ go version
go version go1.26.5 darwin/arm64

$ sw_vers
ProductName:             macOS
ProductVersion:          27.0
BuildVersion:            26A428

$ docker context show
desktop-linux

$ docker version --format 'client={{.Client.Version}}/{{.Client.Os}}/{{.Client.Arch}} server={{.Server.Version}}/{{.Server.Os}}/{{.Server.Arch}}'
client=29.8.0/darwin/arm64 server=29.8.0/linux/arm64
~~~

## Real PostgreSQL persistence and server restart

The regression uses a deterministic test-only Ed25519 key through registry.Sign/Verify, not a fixture signature. Its SSH external binding includes CredentialRef, ExpectedHostKey and remote-loopback address. Before reconciliation it closes the application pool, reconstructs current/candidate/status solely from PostgreSQL, verifies the signed envelope and exact binding hash, replays the unknown operation, and completes from the restored values. It then closes/reopens again and checks terminal receipt, exact signed registry envelope, inventory, target and registration_binding_sha256.

Docker setup used a named task-local volume so a real PostgreSQL server restart did not discard state:

~~~text
$ docker volume create hl263-r10-pg-restart-data-20260918
hl263-r10-pg-restart-data-20260918

$ docker run --detach --name hl263-r10-pg-restart-20260918 --env POSTGRES_HOST_AUTH_METHOD=trust --env POSTGRES_DB=agent_service_test --mount source=hl263-r10-pg-restart-data-20260918,target=/var/lib/postgresql/data --publish 127.0.0.1::5432 postgres:17-alpine
1544810dd2069f0271fb9e1a352f3d54089cf218f84ce7171b050f00b73ae838

$ docker inspect --format 'id={{.Id}} image={{.Image}} running={{.State.Running}} started={{.State.StartedAt}} restartCount={{.RestartCount}} autoRemove={{.HostConfig.AutoRemove}} mounts={{json .Mounts}} ports={{json .NetworkSettings.Ports}}' hl263-r10-pg-restart-20260918
id=1544810dd2069f0271fb9e1a352f3d54089cf218f84ce7171b050f00b73ae838 image=sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73 running=true started=2026-09-18T11:11:27.702352637Z restartCount=0 autoRemove=false mounts=[{"Destination":"/var/lib/postgresql/data","Driver":"local","Mode":"z","Name":"hl263-r10-pg-restart-data-20260918","Propagation":"","RW":true,"Source":"/var/lib/docker/volumes/hl263-r10-pg-restart-data-20260918/_data","Type":"volume"}] ports={"5432/tcp":[{"HostIp":"127.0.0.1","HostPort":"62182"}]}

$ docker exec hl263-r10-pg-restart-20260918 psql -U postgres -d agent_service_test -Atc "SHOW server_version; SHOW server_version_num; SHOW max_connections;"
17.11
170011
100
~~~

Initial exact-owner run, from agentservice/:

~~~text
$ GOWORK=off GOCACHE=/private/tmp/hl263-r10-go-cache HL293_R10_OWNER_ID=hl293-r10-server-restart AGENT_SERVICE_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:62182/agent_service_test?sslmode=disable' go test -race ./internal/store -run '^TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles$' -count=1 -v
=== RUN   TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles
--- PASS: TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles (0.18s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store  1.402s
~~~

The first non-escalated attempt failed at database connection because the sandbox denied loopback access; the identical authorized command above passed.

Read-only SQL before server restart:

~~~text
$ docker exec hl263-r10-pg-restart-20260918 psql -U postgres -d agent_service_test -P pager=off -x -c "SELECT owner_id,operation_id,phase,effect_state,operation_version,candidate_registry_version,candidate_registry_sha256,new_node_host_id,intent->'externalBinding'->>'transport' AS transport,intent->'externalBinding'->>'credentialRef' AS credential_ref,intent->'externalBinding'->>'expectedHostKey' AS expected_host_key,intent->'externalBinding'->>'address' AS address FROM agent_service.registry_operations WHERE owner_id='hl293-r10-server-restart' AND operation_id='r10-pg-enrollment';"
-[ RECORD 1 ]--------------+-----------------------------------------------------------------
owner_id                   | hl293-r10-server-restart
operation_id               | r10-pg-enrollment
phase                      | succeeded
effect_state               | reconciled
operation_version          | 4
candidate_registry_version | 3
candidate_registry_sha256  | e9cb5e18d261f1419aebe3fdda99da9c2e94a188f8bc1aa052a25a37f73a8910
new_node_host_id           | 10000000-0000-4000-8000-000000000002
transport                  | ssh
credential_ref             | credential-r10
expected_host_key          | SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
address                    | 127.0.0.1:9443

$ docker exec hl263-r10-pg-restart-20260918 psql -U postgres -d agent_service_test -P pager=off -x -c "SELECT i.owner_id,i.node_id,i.host_id,i.registry_version,i.manifest_sha256,i.registration_revision,i.registration_epoch,i.registration_binding_sha256,i.registration_projected,s.registry_version AS state_registry_version,s.manifest_sha256 AS state_manifest_sha256,s.node_count,json_array_length((s.registry_envelope->'manifest'->'nodes')) AS envelope_nodes,length(s.registry_envelope->>'signature') AS signature_b64_bytes FROM agent_service.instances i JOIN agent_service.registry_state s USING(owner_id) WHERE i.owner_id='hl293-r10-server-restart' AND i.node_id='20000000-0000-4000-8000-000000000002';"
-[ RECORD 1 ]---------------+-----------------------------------------------------------------
owner_id                    | hl293-r10-server-restart
node_id                     | 20000000-0000-4000-8000-000000000002
host_id                     | 10000000-0000-4000-8000-000000000002
registry_version            | 3
manifest_sha256             | e9cb5e18d261f1419aebe3fdda99da9c2e94a188f8bc1aa052a25a37f73a8910
registration_revision       | 1
registration_epoch          | 1
registration_binding_sha256 | 02d18bcec4437009e836134da0998811986c8dd62f94ea5ea1496e4f16f441e6
registration_projected      | t
state_registry_version      | 3
state_manifest_sha256       | e9cb5e18d261f1419aebe3fdda99da9c2e94a188f8bc1aa052a25a37f73a8910
node_count                  | 2
envelope_nodes              | 2
signature_b64_bytes         | 88
~~~

Real server restart:

~~~text
$ docker restart --time 10 hl263-r10-pg-restart-20260918
Flag --time has been deprecated, use --timeout instead
hl263-r10-pg-restart-20260918

$ docker inspect --format 'id={{.Id}} running={{.State.Running}} started={{.State.StartedAt}} finished={{.State.FinishedAt}} restartCount={{.RestartCount}} volume={{(index .Mounts 0).Name}} port={{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' hl263-r10-pg-restart-20260918
id=1544810dd2069f0271fb9e1a352f3d54089cf218f84ce7171b050f00b73ae838 running=true started=2026-09-18T11:12:57.48165372Z finished=2026-09-18T11:12:57.37264747Z restartCount=0 volume=hl263-r10-pg-restart-data-20260918 port=62286

$ docker exec hl263-r10-pg-restart-20260918 pg_isready -U postgres -d agent_service_test
/var/run/postgresql:5432 - accepting connections
~~~

Docker Desktop reassigned the dynamic loopback port from 62182 to 62286 on restart. The container ID and persistent volume remained exact; StartedAt/FinishedAt changed. RestartCount is zero because this was an explicit manual restart, not a restart-policy recovery.

Read-only terminal verification after the server restart:

~~~text
$ GOWORK=off GOCACHE=/private/tmp/hl263-r10-go-cache HL293_R10_OWNER_ID=hl293-r10-server-restart HL293_R10_RECOVERY_PHASE=verify-server-restart AGENT_SERVICE_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:62286/agent_service_test?sslmode=disable' go test -race ./internal/store -run '^TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles$' -count=1 -v
=== RUN   TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles
--- PASS: TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles (0.05s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store  1.394s
~~~

This phase calls CheckSchema and only reads/re-verifies terminal operation, signed envelope, exact replay, projected inventory/target, manifest hash and registration binding. It does not run migrations or mutate state.

Final SQL after server restart:

~~~text
$ docker exec hl263-r10-pg-restart-20260918 psql -U postgres -d agent_service_test -P pager=off -x -c "SELECT pg_postmaster_start_time() AS postmaster_started_at,current_setting('server_version') AS server_version,(SELECT count(*) FROM agent_service.schema_migrations) AS migrations,(SELECT min(version) FROM agent_service.schema_migrations) AS min_version,(SELECT max(version) FROM agent_service.schema_migrations) AS max_version;"
-[ RECORD 1 ]---------+------------------------------
postmaster_started_at | 2026-09-18 11:12:57.552516+00
server_version        | 17.11
migrations            | 7
min_version           | 1
max_version           | 7

$ docker exec hl263-r10-pg-restart-20260918 psql -U postgres -d agent_service_test -P pager=off -x -c "SELECT o.owner_id,o.operation_id,o.phase,o.effect_state,o.operation_version,o.candidate_registry_version,o.candidate_registry_sha256,o.new_node_host_id,o.intent->'externalBinding'->>'transport' AS transport,o.intent->'externalBinding'->>'credentialRef' AS credential_ref,o.intent->'externalBinding'->>'expectedHostKey' AS expected_host_key,o.intent->'externalBinding'->>'address' AS address,i.node_id,i.host_id,i.registration_binding_sha256,i.registration_projected,s.registry_version,s.manifest_sha256,s.node_count,json_array_length(s.registry_envelope->'manifest'->'nodes') AS envelope_nodes,length(s.registry_envelope->>'signature') AS signature_b64_bytes FROM agent_service.registry_operations o JOIN agent_service.instances i ON i.owner_id=o.owner_id AND i.node_id='20000000-0000-4000-8000-000000000002' JOIN agent_service.registry_state s ON s.owner_id=o.owner_id WHERE o.owner_id='hl293-r10-server-restart' AND o.operation_id='r10-pg-enrollment';"
-[ RECORD 1 ]---------------+-----------------------------------------------------------------
owner_id                    | hl293-r10-server-restart
operation_id                | r10-pg-enrollment
phase                       | succeeded
effect_state                | reconciled
operation_version           | 4
candidate_registry_version  | 3
candidate_registry_sha256   | e9cb5e18d261f1419aebe3fdda99da9c2e94a188f8bc1aa052a25a37f73a8910
new_node_host_id            | 10000000-0000-4000-8000-000000000002
transport                   | ssh
credential_ref              | credential-r10
expected_host_key           | SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA
address                     | 127.0.0.1:9443
node_id                     | 20000000-0000-4000-8000-000000000002
host_id                     | 10000000-0000-4000-8000-000000000002
registration_binding_sha256 | 02d18bcec4437009e836134da0998811986c8dd62f94ea5ea1496e4f16f441e6
registration_projected      | t
registry_version            | 3
manifest_sha256             | e9cb5e18d261f1419aebe3fdda99da9c2e94a188f8bc1aa052a25a37f73a8910
node_count                  | 2
envelope_nodes              | 2
signature_b64_bytes         | 88
~~~

Focused real-Pg regression gate after restart:

~~~text
$ GOWORK=off GOCACHE=/private/tmp/hl263-r10-go-cache AGENT_SERVICE_TEST_DATABASE_URL='postgres://postgres@127.0.0.1:62286/agent_service_test?sslmode=disable' go test -race ./internal/store -run '^TestPostgres(ExternalEnrollmentIntentSurvivesRestartAndReconciles|RegistryOperationRecoveryIsolationAndGlobalIdempotency|OperationCASReceiptLeaseAndRestart|LegacyRegistryAdoptsSignedProjectionOnceAndNeverDowngrades)$' -count=1 -v
=== RUN   TestPostgresOperationCASReceiptLeaseAndRestart
=== RUN   TestPostgresOperationCASReceiptLeaseAndRestart/stale_target_host
=== RUN   TestPostgresOperationCASReceiptLeaseAndRestart/stale_target_epoch
--- PASS: TestPostgresOperationCASReceiptLeaseAndRestart (0.10s)
    --- PASS: TestPostgresOperationCASReceiptLeaseAndRestart/stale_target_host (0.00s)
    --- PASS: TestPostgresOperationCASReceiptLeaseAndRestart/stale_target_epoch (0.00s)
=== RUN   TestPostgresLegacyRegistryAdoptsSignedProjectionOnceAndNeverDowngrades
--- PASS: TestPostgresLegacyRegistryAdoptsSignedProjectionOnceAndNeverDowngrades (0.04s)
=== RUN   TestPostgresRegistryOperationRecoveryIsolationAndGlobalIdempotency
--- PASS: TestPostgresRegistryOperationRecoveryIsolationAndGlobalIdempotency (0.08s)
=== RUN   TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles
--- PASS: TestPostgresExternalEnrollmentIntentSurvivesRestartAndReconciles (0.06s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/store  1.582s
~~~

## mTLS, admission, replay and Router restart gates

All named tests and subtests are retained below so a zero-match package PASS cannot be mistaken for evidence.

~~~text
$ GOCACHE=/private/tmp/hl263-r10-go-cache go test -race ./internal/harnessclient ./internal/harnessrouter ./internal/panel -run '^Test(SignedRegistryAndMutualTLS|PrivateTunnelPreservesEndToEndMTLSAndCredentialRoles|AdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness|EnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle|EnrollmentIncompatibilityLeavesNodeSealed|EnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement|EnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady|EnrollmentHostProjectionDoesNotExposePrivateRefs)$' -count=1 -v
=== RUN   TestSignedRegistryAndMutualTLS
=== RUN   TestSignedRegistryAndMutualTLS/exact_key_manifest
=== RUN   TestSignedRegistryAndMutualTLS/exact_key_signature
=== RUN   TestSignedRegistryAndMutualTLS/exact_key_ownerId
=== RUN   TestSignedRegistryAndMutualTLS/exact_key_nodeId
=== RUN   TestSignedRegistryAndMutualTLS/exact_key_certificateSHA256
=== RUN   TestSignedRegistryAndMutualTLS/unsigned_URL_mutation
=== RUN   TestSignedRegistryAndMutualTLS/duplicate_node
=== RUN   TestSignedRegistryAndMutualTLS/query_URL
=== RUN   TestSignedRegistryAndMutualTLS/missing_owner
=== RUN   TestSignedRegistryAndMutualTLS/wrong_leaf
=== RUN   TestSignedRegistryAndMutualTLS/untrusted_CA
--- PASS: TestSignedRegistryAndMutualTLS (0.06s)
    --- PASS: TestSignedRegistryAndMutualTLS/exact_key_manifest (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/exact_key_signature (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/exact_key_ownerId (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/exact_key_nodeId (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/exact_key_certificateSHA256 (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/unsigned_URL_mutation (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/duplicate_node (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/query_URL (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/missing_owner (0.00s)
    --- PASS: TestSignedRegistryAndMutualTLS/wrong_leaf (0.01s)
    --- PASS: TestSignedRegistryAndMutualTLS/untrusted_CA (0.00s)
=== RUN   TestPrivateTunnelPreservesEndToEndMTLSAndCredentialRoles
--- PASS: TestPrivateTunnelPreservesEndToEndMTLSAndCredentialRoles (0.03s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnessclient  1.409s
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/missing
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/owner
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/identity
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/capability
=== RUN   TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/readiness
--- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness (0.00s)
    --- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/missing (0.00s)
    --- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/owner (0.00s)
    --- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/identity (0.00s)
    --- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/capability (0.00s)
    --- PASS: TestAdmissionProfileRejectsMissingOwnerIdentityCapabilityAndReadiness/readiness (0.00s)
=== RUN   TestEnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle
--- PASS: TestEnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle (0.06s)
=== RUN   TestEnrollmentIncompatibilityLeavesNodeSealed
--- PASS: TestEnrollmentIncompatibilityLeavesNodeSealed (0.03s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter  1.417s
=== RUN   TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement
=== RUN   TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement/accepted
=== RUN   TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement/reconciling
--- PASS: TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement (0.00s)
    --- PASS: TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement/accepted (0.00s)
    --- PASS: TestEnrollmentBFFReturnsOnlyReadyResultAndReconcilesLostAcknowledgement/reconciling (0.00s)
=== RUN   TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady
=== RUN   TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady/unknown_registry_effect
=== RUN   TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady/incompatible_profile
--- PASS: TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady (0.00s)
    --- PASS: TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady/unknown_registry_effect (0.00s)
    --- PASS: TestEnrollmentBFFNeverReportsPartialOrIncompatibleActivationAsReady/incompatible_profile (0.00s)
=== RUN   TestEnrollmentHostProjectionDoesNotExposePrivateRefs
--- PASS: TestEnrollmentHostProjectionDoesNotExposePrivateRefs (0.00s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/internal/panel  1.328s
~~~

These are cryptographic TLS 1.3/generated-certificate and private-tunnel fixtures, not a native Harness process.

~~~text
$ GOCACHE=/private/tmp/hl263-r10-go-cache go test -race ./internal/harnessrouter -run '^Test(RegistryInstallAddsOneNodeAndPreservesUntouchedFenceAcrossRestart|RegistryInstallUnknownCommitPoisonsUntilVerifiedRestart|EnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle|EnrollmentIncompatibilityLeavesNodeSealed)$' -count=1 -v
=== RUN   TestEnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle
--- PASS: TestEnrollmentDuplicateAndLostAcknowledgementProjectOnceWithoutLifecycle (0.06s)
=== RUN   TestEnrollmentIncompatibilityLeavesNodeSealed
--- PASS: TestEnrollmentIncompatibilityLeavesNodeSealed (0.03s)
=== RUN   TestRegistryInstallAddsOneNodeAndPreservesUntouchedFenceAcrossRestart
--- PASS: TestRegistryInstallAddsOneNodeAndPreservesUntouchedFenceAcrossRestart (0.05s)
=== RUN   TestRegistryInstallUnknownCommitPoisonsUntilVerifiedRestart
--- PASS: TestRegistryInstallUnknownCommitPoisonsUntilVerifiedRestart (0.04s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/internal/harnessrouter  1.386s

$ GOWORK=off GOCACHE=/private/tmp/hl263-r10-go-cache go test -race ./internal/httpapi ./internal/model -run '^Test(ExternalEnrollmentSignsExactCandidateAndReplaysOneDurableOperation|ExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI|SSHExternalEndpointIsRemoteLoopbackOnly|ExternalEnrollmentRequestAndBindingHashesFenceIdentity)$' -count=1 -v
=== RUN   TestExternalEnrollmentSignsExactCandidateAndReplaysOneDurableOperation
--- PASS: TestExternalEnrollmentSignsExactCandidateAndReplaysOneDurableOperation (0.02s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/httpapi  1.329s
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://harness.internal:9443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://8.8.8.8:9443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://169.254.1.2:9443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://0.0.0.0:9443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://user@10.20.30.40:9443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:9443/
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:9443?next=1
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:09443
=== RUN   TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://[::ffff:10.20.30.40]:9443
--- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://harness.internal:9443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://8.8.8.8:9443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://169.254.1.2:9443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://0.0.0.0:9443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://user@10.20.30.40:9443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:9443/ (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:9443?next=1 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://10.20.30.40:09443 (0.00s)
    --- PASS: TestExternalEndpointRejectsDNSRebindingPublicAndAmbiguousURI/https://[::ffff:10.20.30.40]:9443 (0.00s)
=== RUN   TestSSHExternalEndpointIsRemoteLoopbackOnly
--- PASS: TestSSHExternalEndpointIsRemoteLoopbackOnly (0.00s)
=== RUN   TestExternalEnrollmentRequestAndBindingHashesFenceIdentity
--- PASS: TestExternalEnrollmentRequestAndBindingHashesFenceIdentity (0.00s)
PASS
ok      github.com/boxvtk621/homelab-telegram-panel/agentservice/internal/model  1.287s
~~~

## Cleanup

~~~text
$ docker rm --force hl263-r10-pg-restart-20260918
hl263-r10-pg-restart-20260918

$ docker volume rm hl263-r10-pg-restart-data-20260918
hl263-r10-pg-restart-data-20260918

$ docker ps -a --filter name=^/hl263-r10-pg-restart-20260918$ --format '{{.ID}} {{.Names}} {{.Status}}'
<no output>

$ docker volume ls --filter name=^hl263-r10-pg-restart-data-20260918$ --format '{{.Name}}'
<no output>
~~~

The task-local container and persistent volume were removed after evidence collection.

## Exact boundary and result

- PASS: real PostgreSQL 17 persistence, unknown-effect recovery across an application-pool reconnect, real postmaster restart over a persistent task-local volume, read-only post-restart terminal verification, signed registry re-verification, exact terminal replay, SSH binding/credential/host-key persistence and binding/hash readback.
- PASS: R10 protocol admission negatives, exact request hashing, duplicate/lost-ACK recovery, Router restart/readback, zero lifecycle calls, TLS 1.3/private-tunnel fixture and credential-role separation.
- The store regression proves unknown-effect recovery across application reconnect and terminal durability across a later real PostgreSQL restart. It does not claim an in-flight unknown-effect postmaster restart. Router/Panel/mTLS tests prove their own seams; they are not presented as one combined native-process E2E.
- NOT_RUN: a single compatible external Harness process enrollment followed by process restart and live mTLS redial; remote SSH server/AllowTcpForwarding; deployed/production runtime; owner acceptance. No authorized implemented compatible target exists.
- The current concrete Harness intentionally lacks future profile/MCP, replica, assets and target-reservation capabilities and therefore remains fenced as incompatible.
- DEPENDENCIES: R15-R19 profile/MCP producers, R20-R22 input/assets, R25 compatible managed build/endpoint, and later transfer/retirement producers must supply their owned capabilities. This package does not start R15.
- No commit, push, merge or deploy was performed.
