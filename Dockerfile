# Build the manager binary.
#
# The two base images are build args. The defaults are public (Docker Hub /
# gcr.io), so a clone builds with no extra flags; point them at a mirror where
# those registries are unreachable.
ARG GO_IMAGE=golang:1.26
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot
# Multi-arch: Go cross-compiles, so the build stage runs on the builder's own
# platform and only the runtime stage is per-target -- no emulation needed.
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS builder
# TARGETOS/TARGETARCH are filled in by BuildKit. The classic builder does not
# set them, and an unset one aborts the RUN below under `set -u` -- hence the
# `:-` defaults there. An empty GOARCH is the intended fallback, not a bug: go
# then builds for the host, which is what a non-buildx build wants anyway.
ARG TARGETOS TARGETARCH
# Module proxy. The default is the public one; pass a mirror where
# proxy.golang.org is unreachable. GOSUMDB is off so a build that cannot reach
# sum.golang.org still works (go.sum is committed), and GOTOOLCHAIN=local keeps
# go from trying to fetch a toolchain of its own when go.mod names a newer one
# than the base image carries.
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY} GOSUMDB=off GOTOOLCHAIN=local
# Module resolution mode: "auto" builds from vendor/ when it is present in the
# build context and falls back to downloading modules otherwise. Set to "vendor"
# or "mod" to require one of them (i.e. --build-arg GO_MOD_MODE=mod).
ARG GO_MOD_MODE=auto

WORKDIR /workspace
# Copy the Go source, including go.mod/go.sum and vendor/ if it exists
# (relies on .dockerignore to filter)
COPY . .

# Build
# GOOS/GOARCH come from the target platform, so the binary matches the runtime
# stage even though this stage runs natively on the build platform.
RUN set -eu; \
    mod="${GO_MOD_MODE}"; \
    if [ "${mod}" = "auto" ]; then \
        if [ -f vendor/modules.txt ]; then mod=vendor; else mod=mod; fi; \
    fi; \
    echo "building with -mod=${mod}"; \
    if [ "${mod}" != "vendor" ]; then go mod download; fi; \
    CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-}" \
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
