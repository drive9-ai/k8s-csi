DRIVE9_REF ?= $(shell git ls-remote https://github.com/mem9-ai/drive9.git refs/heads/main | awk '{print $$1}')
DRIVE9_REF_SHORT = $(shell printf '%s' '$(DRIVE9_REF)' | cut -c1-7)
CSI_REF ?= $(shell git rev-parse --short=7 HEAD)
IMAGE ?= ghcr.io/drive9-ai/drive9-csi:drive9-$(DRIVE9_REF_SHORT)-csi-$(CSI_REF)
GOPROXY ?= https://proxy.golang.org,direct
GOSUMDB ?= sum.golang.org

.PHONY: test
test:
	gofmt -w cmd internal
	go test ./...

.PHONY: build
build:
	mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o bin/drive9-csi ./cmd/drive9-csi

.PHONY: image
image:
	@printf '%s' "$(DRIVE9_REF)" | grep -Eq '^[0-9a-f]{7,40}$$' || { echo "DRIVE9_REF must be a 7-40 char hex commit SHA" >&2; exit 1; }
	docker build --build-arg DRIVE9_REF=$(DRIVE9_REF) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) .

.PHONY: manifests
manifests:
	kubectl apply -k deploy/kubernetes
