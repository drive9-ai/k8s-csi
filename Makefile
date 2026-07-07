IMAGE ?= ghcr.io/drive9-ai/drive9-csi:local
GOPROXY ?= https://proxy.golang.org,direct
GOSUMDB ?= sum.golang.org
GOOS ?= linux
GOARCH ?= amd64
PLATFORM ?= $(GOOS)/$(GOARCH)
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: test
test:
	gofmt -w cmd internal
	go test ./...

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -o bin/drive9-csi ./cmd/drive9-csi

.PHONY: image
image:
	docker build --platform $(PLATFORM) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) .

.PHONY: image-multi
image-multi:
	docker buildx build --platform $(PLATFORMS) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) --push .

.PHONY: manifests
manifests:
	kubectl apply -k deploy/kubernetes
