# clipstore — local developer loop. CI does NOT run this file; it runs
# `go vet` + `go test` with CGO off (.github/workflows/ci.yml), and release images
# are built and pushed to GHCR from a `v*` tag (.github/workflows/release.yml).
#
# 🔴 `make test` HERE IS THE -race RUN, AND CI'S IS NOT. CI runs CGO_ENABLED=0
# deliberately, because that is how the shipped binary is built. That means the
# race detector only ever runs from this file — if you touch anything concurrent
# (there is almost nothing: SetMaxOpenConns(1) serialises the store), run it.
VERSION ?= dev
IMAGE   ?= promptshipper-clip:$(VERSION)

.PHONY: build test vet race fmt image clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.buildVersion=$(VERSION)" -o clipstore .

vet:
	CGO_ENABLED=0 go vet ./...

test: vet
	go test -race -count=1 ./...

# What the gate runs, so a failure can be reproduced in the gate's own shape.
gate:
	CGO_ENABLED=0 go vet ./... && CGO_ENABLED=0 go test -count=1 ./...

fmt:
	gofmt -l -w .

# Local image build for inspection only. Release images are built by CI from a
# `v*` tag and pushed to GHCR.
image:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE) .

clean:
	rm -f clipstore
