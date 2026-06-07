IMAGE ?= ghcr.io/drive9-ai/drive9-csi:dev
DRIVE9_REF ?= main

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
	docker build --build-arg DRIVE9_REF=$(DRIVE9_REF) -t $(IMAGE) .

.PHONY: manifests
manifests:
	kubectl apply -f deploy/kubernetes/
