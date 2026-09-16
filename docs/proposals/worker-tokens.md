# Operator-managed worker authentication

Status: blocked on Cursor confirming a supported token-exchange API. This is a
proposal, not an implemented authentication mode.

## Current exposure and interim mitigation

The controller authenticates with a service-account API key. The Kubernetes
backend also injects that key into worker containers through `CURSOR_API_KEY`.
Code running inside a worker can read it. Moving that same long-lived key from
environment to a mounted file would not remove this exposure.

Use a repository-scoped key and the controller's `--repository` filter where
supported. The credential's server-side scope limits blast radius; the filter
alone does not restrict a broadly scoped key. This does not prevent a worker
from reading and using the credential it receives.

## Available worker-side capability

The reported Cursor Agent CLI version `2026.08.11-e8db854` accepts
`agent worker start --auth-token-file <path>`, described as intended for
operator-managed Secret mounts. Cursor's [public sample](https://github.com/anysphere/k8s-workers#compared-to-the-operator)
confirms operator token exchange plus `--auth-token-file`. The operator reportedly uses
`enableAuthManagement` to exchange a service-account key for rotating worker
tokens. These observations establish a possible consumer path, not a public
minting API contract. This repository has no supported mint endpoint, request
schema, scopes, expiry rules, or refresh contract to implement against.

## Questions for Cursor

Is the private-workers token exchange used by `worker-set-controller` a
supported public API for third-party controllers authenticated with a
service-account key? Please provide:

- Endpoint, method, API version, request/response schema, and required key scope.
- Worker, pool, repository and organization binding rules; whether tokens can be
  used outside their intended worker or exchanged for broader credentials.
- Lifetime, expiry field, refresh/rotation protocol, overlap period, revocation,
  rate limits, and retry/idempotency behavior.
- CLI versions supporting the token file; whether the running worker rereads
  the file and how refresh failure/expiry is handled during active sessions.
- Whether environment credentials override token-file authentication, and how
  to guarantee the long-lived service-account key stays out of worker Pods.

This section is a prepared question for Cursor support or the
[public k8s-workers repository](https://github.com/anysphere/k8s-workers).

## Proposed implementation after the contract is confirmed

1. Add an explicit opt-in managed-auth mode. Keep the service-account credential
   in the controller only; do not inject it into managed-auth worker Pods.
2. Mint a worker-bound, short-lived token before creating each worker Pod.
   Store only that worker's token in a dedicated Secret in the worker namespace.
3. Mount the Secret read-only as a directory (not a `subPath`, which would block
   mount updates) and pass `--auth-token-file` to the selected worker container.
   Verify the CLI rereads updated files before relying on Secret rotation.
4. Refresh before expiry with jitter and bounded retries. Account for kubelet
   Secret propagation delay. Recover expiry and refresh state after controller
   restarts without persisting tokens in annotations or logs.
5. Clean up credentials on failed spawn and disposal, define wake/re-mint rules,
   and reconcile orphaned Secrets. Scope Secret RBAC to the namespace and
   document its increased privileges. Do not put token values in metrics,
   errors, traces, CLI arguments, or PR/test fixtures.
6. Test mint/refresh failures, expiry during an active session, rotation while
   mounted, restart recovery, wake, revocation, and cleanup against Cursor's
   supported contract before enabling the mode.

Acceptance requires worker Pods to have no long-lived service-account key,
rotating worker tokens to survive long sessions and controller restarts, and
an end-to-end test against the supported Cursor API. Do not guess private
endpoints or advertise managed authentication before those checks pass.
