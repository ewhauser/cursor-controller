# Contributing

Thanks for helping improve cursor-controller. Bug reports, documentation fixes,
tests, and code changes are welcome.

## Development setup

Install Go at the version declared in `go.mod` and Helm 3. Clone the repository,
then use the Make targets that CI runs:

```bash
make check
```

`make check` builds every Go package and the controller binary, runs the test
suite with the race detector, runs the pinned golangci-lint suite, and lints the
Helm chart with both standard and persistence-enabled values.

For a quicker edit loop, use the individual targets:

```bash
make build
make test
make lint
make lint-new
make helm-lint
make local-e2e
```

The optional pre-commit hook runs `make lint-new`:

```bash
pre-commit install
```

## Changes and tests

Add a failing regression test before fixing a bug. Keep changes focused, update
documentation when behavior or configuration changes, and run `make check`
before opening a pull request. Changes to the controller lifecycle or backend
behavior should also pass `make local-e2e`.

Kubernetes lifecycle changes should pass the kind suite:

```bash
make e2e-setup
make e2e
make e2e-teardown
```

## Pull requests

Describe the problem, the approach, and the validation performed. CI separates
build, race-test, lint, Helm, local end-to-end, and kind end-to-end gates. Images
are published only from pushes after every gate succeeds.
