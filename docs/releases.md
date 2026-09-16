# Releases

Push a new semantic version tag, such as `v0.2.0`, on the reviewed commit.
The CI workflow runs build, race tests, lint, Helm validation and both E2E
suites before publishing the multi-platform image. The release job then
packages the chart with version and appVersion set to the tag without `v`,
pushes it to `ghcr.io/ewhauser/charts/cursor-controller`, verifies a pull of the
published archive, and creates a GitHub Release with generated notes and the
chart archive attached. Prerelease tags must use SemVer, e.g. `v0.2.0-rc.1`.

A release-job rerun can update the archive attachment on an existing release.
Do not move published tags or reuse versions for different content. Verify
image and chart package visibility in GHCR; first publication may require the
owner to make packages public. Release publishing does not add signing yet.

Install a published version (replace the version with one actually released):

```sh
helm upgrade --install cursor-controller \
  oci://ghcr.io/ewhauser/charts/cursor-controller \
  --version 0.2.0 -f values.yaml
```

For Flux, use an `OCIRepository` URL of
`oci://ghcr.io/ewhauser/charts/cursor-controller` and pin `spec.ref.digest` to the
published chart manifest digest. Reference it from `HelmRelease.spec.chartRef`.
The chart appVersion matches the corresponding image tag; deployments can
separately pin `image.digest` for immutable images. See
[Helm OCI registries](https://helm.sh/docs/topics/registries/).

Existing tags do not run this new workflow retroactively. Publish a new tag
containing the workflow rather than assuming `v0.1.0` has an OCI chart or release.
