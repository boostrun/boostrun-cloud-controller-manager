# Contributing

## Development

Requires Go (version in `go.mod`), Docker and Helm.

```bash
go test -race ./...
golangci-lint run
helm lint charts/boostrun-cloud-controller-manager
```

`test/kind/install-test.sh` installs the chart on a [kind](https://kind.sigs.k8s.io) cluster
against `test/fakeplatform`, an in-memory stand-in for the platform API:

```bash
kind create cluster --config test/kind/cluster.yaml
docker build -t boostrun-cloud-controller-manager:dev .
kind load docker-image boostrun-cloud-controller-manager:dev
test/kind/install-test.sh boostrun-cloud-controller-manager:dev
```

CI runs all of these on every pull request, with kind on each supported Kubernetes version.

## Pull requests

Keep changes focused, add tests for behaviour changes, and update the README when user-facing
behaviour changes. Add an entry to `CHANGELOG.md` under **Unreleased**.

## Releases

Maintainers run the **Release** workflow on `main` with a version such as `v1.2.0`. It publishes
the signed image, the Helm chart pinned to the image digest, and a GitHub Release with
`install.yaml`.

## Verifying a release

Images are signed by the release workflow with [cosign](https://github.com/sigstore/cosign):

```bash
cosign verify public.ecr.aws/boostrun/boostrun-cloud-controller-manager:<version> \
  --certificate-identity-regexp '^https://github.com/boostrun/boostrun-cloud-controller-manager/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```
