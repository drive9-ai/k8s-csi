IMAGE ?= ghcr.io/drive9-ai/drive9-csi:local
GOPROXY ?= https://proxy.golang.org,direct
GOSUMDB ?= sum.golang.org
GOOS ?= linux
GOARCH ?= amd64
PLATFORM ?= $(GOOS)/$(GOARCH)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: fmt-check
fmt-check:
	@unformatted="$$(gofmt -l cmd internal)"; \
	if [ -n "$$unformatted" ]; then \
		echo "unformatted Go files:" >&2; \
		printf '%s\n' "$$unformatted" >&2; \
		exit 1; \
	fi

.PHONY: test
test: fmt-check
	go test ./...

.PHONY: test-race
test-race:
	go test -race ./...

.PHONY: test-linux-compile
test-linux-compile:
	@tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	for arch in amd64 arm64; do \
		CGO_ENABLED=0 GOOS=linux GOARCH="$$arch" go test -c -o "$$tmpdir/driver-$$arch.test" ./internal/driver; \
	done

.PHONY: vet
vet:
	go vet ./...

.PHONY: diff-check
diff-check:
	git diff --check

.PHONY: check
check: test test-race vet test-linux-compile diff-check

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/drive9-csi ./cmd/drive9-csi
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/drive9-csi-launcher ./cmd/drive9-csi-launcher

.PHONY: image
image:
	docker build --platform $(PLATFORM) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) .

.PHONY: image-multi
image-multi:
	docker buildx build --platform $(PLATFORMS) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) --push .

.PHONY: manifests
manifests:
	kubectl apply -k deploy/kubernetes/overlays/local
