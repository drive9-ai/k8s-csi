IMAGE ?= ghcr.io/drive9-ai/drive9-csi:0.1.0
DRIVE9_REF ?= 68ce029f889a1a6ac17b07fb9d6b5849ce39631b

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
