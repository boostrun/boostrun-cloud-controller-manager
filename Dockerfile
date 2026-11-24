# Cross-compiles on the build host's architecture for each target platform (no emulation).
FROM --platform=$BUILDPLATFORM golang:1.27@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X github.com/boostrun/boostrun-cloud-controller-manager/internal/version.Version=${VERSION}" \
      -o /out/boostrun-cloud-controller-manager .

FROM gcr.io/distroless/static:nonroot@sha256:8ce6dd87e9823c5b48dcfb2c8604714ccb351ce59607327bcc4957448da279b5
COPY --from=build /out/boostrun-cloud-controller-manager /boostrun-cloud-controller-manager
USER 65532:65532
ENTRYPOINT ["/boostrun-cloud-controller-manager"]
