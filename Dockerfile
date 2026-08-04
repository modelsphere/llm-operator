# Build the manager binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
# Module resolution mode: "auto" builds from vendor/ when it is present in the
# build context and falls back to downloading modules otherwise. Set to "vendor"
# or "mod" to require one of them (i.e. --build-arg GO_MOD_MODE=mod).
ARG GO_MOD_MODE=auto

WORKDIR /workspace
# Copy the Go source, including go.mod/go.sum and vendor/ if it exists
# (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN set -eu; \
    mod="${GO_MOD_MODE}"; \
    if [ "${mod}" = "auto" ]; then \
        if [ -f vendor/modules.txt ]; then mod=vendor; else mod=mod; fi; \
    fi; \
    echo "building with -mod=${mod}"; \
    if [ "${mod}" != "vendor" ]; then go mod download; fi; \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
        go build -mod="${mod}" -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
