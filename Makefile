IMAGE ?= ghcr.io/drive9-ai/drive9-csi:drive9-6e2820e-csi-126ff81
DRIVE9_REF ?= cbf73aa4e62bf23cf869e69d3cd060c7a044cbde
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
