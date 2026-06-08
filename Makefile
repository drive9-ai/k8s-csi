IMAGE ?= ghcr.io/drive9-ai/drive9-csi:0.1.0
DRIVE9_REF ?= 7fb7c58d0c99a27d75d0f7a78c904cef2018f799
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
	docker build --build-arg DRIVE9_REF=$(DRIVE9_REF) --build-arg GOPROXY=$(GOPROXY) --build-arg GOSUMDB=$(GOSUMDB) -t $(IMAGE) .

.PHONY: manifests
manifests:
	kubectl apply -k deploy/kubernetes
