IMAGE ?= ghcr.io/drive9-ai/drive9-csi:drive9-6e2820e-csi-1f8e5cd
DRIVE9_REF ?= 6e2820e22d0592ece7644da1135ba9ef7ba95839
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
