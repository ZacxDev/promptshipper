# syntax=docker/dockerfile:1

# --- Stage 1: static Go binary -----------------------------------------------
#
# 🔴 THIS MUST MATCH THE GO THAT GRADES THE MODULE. CI
# (.github/workflows/ci.yml) runs `go vet` + `go test` with the Go version named
# in go.mod. A split between the compiler CI grades with and the compiler that
# builds the shipped artifact makes every green check a claim about a binary
# that was never built.
#
# 🔴 CGO_ENABLED=0 IS NOT AN OPTIMISATION, IT IS WHAT MAKES THE RUNTIME STAGE
# POSSIBLE. gcr.io/distroless/static-debian12 has no libc, so a cgo-linked
# binary would not start. It is also why the SQLite driver is modernc.org/sqlite
# (pure Go) and not github.com/mattn/go-sqlite3 — see go.mod.
#
# go.sum IS copied and `go mod download` IS run: this module has one real
# dependency and a build that silently proceeded over an absent go.sum is not the
# failure you want to find in CI.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.buildVersion=${VERSION}" \
    -o /clipstore . \
    && mkdir -p /out/data

# --- Stage 2: minimal runtime ------------------------------------------------
# distroless nonroot runs as uid/gid 65532. No shell, no package manager, no
# write path except the mounted volume at /data (and /tmp if you mount one).
#
# 🔴 /data IS CREATED IN THE IMAGE, OWNED BY 65532, ON PURPOSE. Docker seeds a
# NEW named volume from the image's directory at the mount point, ownership
# included. Without this the compose volume is created root-owned and the
# nonroot process cannot create clipstore.db — a crash on first start that only
# a fresh install ever hits. (Kubernetes solves the same thing with fsGroup.)
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /clipstore /clipstore
COPY --from=build --chown=65532:65532 /out/data /data
EXPOSE 8139
USER nonroot:nonroot
ENTRYPOINT ["/clipstore"]
