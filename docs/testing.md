# Testing

[README](../README.md)

The project provides three levels of automated testing:

| Layer | Command | Needs | What it proves |
| --- | --- | --- | --- |
| Unit | `make test` | go | Parsers, claim/wake/GC logic, Pod/PVC construction (fake clientset), hook env contract, plus a component test that runs the real HTTP client and controller against the in-memory fake API. |
| Laptop e2e | `make local-e2e` | go, curl | Real processes: fake fleet API, controller (hook backend), fake worker. Claim, idle exit, follow-up wake with the workspace intact, missing workspace → release → re-claim, archive → dispose hook. About 40 seconds. |
| kind e2e | `make e2e-setup && make e2e` | docker with buildx, kind, kubectl, helm | Real cluster with the CSI hostpath driver and snapshot controller: PVC cloned from a `VolumeSnapshot`, Pod lifecycle, wake on the same PVC with the marker file present, PVC deletion on archive, controller restart without duplicate claims. `make e2e-teardown` removes the cluster. |

On macOS without Docker Desktop: `brew install colima docker docker-buildx kind helm`,
link the buildx plugin (`mkdir -p ~/.docker/cli-plugins && ln -sfn $(brew --prefix)/opt/docker-buildx/bin/docker-buildx ~/.docker/cli-plugins/docker-buildx`),
then `colima start --cpu 4 --memory 8`. The setup script is idempotent; rerun it after a failure.

The fakes live in [internal/fakeapi](../internal/fakeapi) (fleet API with
`/fake/*` admin endpoints: inject requests, connect/disconnect workers,
follow-up, archive, inspect state) and [cmd/fake-worker](../cmd/fake-worker)
(accepts `agent worker ... start` arguments, registers with the fake, keeps a
marker file in the workspace, exits after the idle timeout).

To run the controller by hand against the fake: `make fake-api` in one
terminal, then start the controller with `--api-url http://localhost:8081
--api-key dev`, and inject work with
`curl -X POST localhost:8081/fake/requests -d '{"pool":"default"}'`.

All commands below run from the repository root.

## Real Cursor validation

The client expects the following event shapes:
`claimed_offline` carries the request and worker metadata, while `claimed`
carries only the request id. The latter is mapped back to a warm worker using
the connected-worker inventory's `activeBcId`. Run
`CURSOR_API_KEY=... POOL=<dev-pool> ./hack/capture-fixtures.sh` once against
a dev pool while starting an agent and sending a follow-up; it stores the
responses under `internal/cursorapi/testdata`, and the fixture tests then
validate the decoders against captured data. These tests skip when fixtures are
absent. Review and redact captured user and repository metadata before committing
fixtures. For a full run, point the kind
setup or a dev namespace at the real API with a dedicated pool and a short
`idleReleaseTimeout`, trigger an agent with `POST /v1/agents`
(`env.type: pool`), follow up with `POST /v1/agents/{id}/followup`, and archive
with `POST /v1/agents/{id}/archive`.
