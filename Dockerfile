# Build the manager binary.
#
# The two base images are build args: the defaults are public (Docker Hub /
# gcr.io), and a build where those are unreachable can point them at a mirror.
ARG GO_IMAGE=golang:1.26
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot
FROM ${GO_IMAGE} AS builder
# TARGETOS/TARGETARCH are filled in by BuildKit. The classic builder does not
# set them, and an unset one aborts the RUN below under `set -u` -- hence the
# `:-` defaults there. An empty GOARCH is the intended fallback, not a bug: go
# then builds for the host, which is what a non-buildx build wants anyway.
ARG TARGETOS
ARG TARGETARCH
# Module proxy and checksum database. Empty means Go's own defaults
# (proxy.golang.org, sum.golang.org); set them with --build-arg where those are
# unreachable. GOTOOLCHAIN=local keeps go from trying to fetch a toolchain of
# its own when go.mod names a newer one than the base image carries.
ARG GOPROXY=
ARG GOSUMDB=
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB} GOTOOLCHAIN=local
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
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-} \
        go build -mod="${mod}" -a -o manager cmd/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
# ARG is re-declared: an ARG before the first FROM is out of scope in a stage.
ARG RUNTIME_IMAGE
FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
