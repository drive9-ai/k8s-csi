IMAGE ?= ghcr.io/drive9-ai/drive9-csi:local
GOPROXY ?= https://proxy.golang.org,direct
GOSUMDB ?= sum.golang.org
GOOS ?= linux
GOARCH ?= amd64
PLATFORM ?= $(GOOS)/$(GOARCH)
PLATFORMS ?= linux/amd64,linux/arm64
E2E_PREPARE := e2e/prepare.sh
E2E_CASES := e2e/basic-lifecycle.sh e2e/mount-survival.sh
E2E_SCRIPTS := $(E2E_PREPARE) $(E2E_CASES)

.PHONY: fmt-check
fmt-check:
	@unformatted="$$(gofmt -l cmd internal hack/check-manifests.go)"; \
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

.PHONY: build-check
build-check:
	bash -n hack/check-build-artifacts.sh
	hack/check-build-artifacts.sh

.PHONY: manifest-check
manifest-check:
	go run hack/check-manifests.go

.PHONY: script-check
script-check:
	bash -n hack/check-upload-perf.sh
	hack/check-upload-perf.sh

.PHONY: e2e-check
e2e-check:
	bash -n hack/check-e2e-common.sh
	bash -n e2e/prepare.sh
	bash -n e2e/basic-lifecycle.sh
	bash -n e2e/mount-survival.sh
	bash -n e2e/lib/common.sh
	bash -n e2e/lib/manifests.sh
	hack/check-e2e-common.sh
	@awk 'length($$0) > 80 { print FILENAME ":" FNR ": line exceeds 80 columns"; bad=1 } END { exit bad }' $(E2E_SCRIPTS) e2e/lib/*.sh
	@test -f e2e/AGENTS.md
	@test -f e2e/README.md
	@test ! -x e2e/lib/common.sh
	@test ! -x e2e/lib/manifests.sh
	@for script in $(E2E_SCRIPTS); do \
		test -x "$$script" || { echo "E2E script is not executable: $$script" >&2; exit 1; }; \
		rg -Fq 'source "$$script_dir/lib/common.sh"' "$$script" || { echo "E2E script does not source common.sh: $$script" >&2; exit 1; }; \
		rg -Fq 'source "$$script_dir/lib/manifests.sh"' "$$script" || { echo "E2E script does not source manifests.sh: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_init' "$$script" || { echo "E2E script does not call e2e_init: $$script" >&2; exit 1; }; \
	done
	@for script in $(E2E_CASES); do \
		rg -Fq 'e2e_require_prepared_driver' "$$script" || { echo "E2E case does not require a prepared Driver: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_configure_case' "$$script" || { echo "E2E case does not configure its namespace and Secret: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_require_case_environment' "$$script" || { echo "E2E case does not require its pre-provisioned environment: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_render_case_manifests' "$$script" || { echo "E2E case does not render case manifests: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_validate_case_manifests' "$$script" || { echo "E2E case does not validate case manifests: $$script" >&2; exit 1; }; \
		rg -Fq 'case_resources_registered=1' "$$script" || { echo "E2E case does not register namespaced cleanup: $$script" >&2; exit 1; }; \
		rg -Fq 'storage_class_created=1' "$$script" || { echo "E2E case does not track StorageClass ownership: $$script" >&2; exit 1; }; \
		rg -Fq 'volume_attributes_class_created=1' "$$script" || { echo "E2E case does not track VAC ownership: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_cleanup_owned_resource' "$$script" || { echo "E2E case cleanup is not ownership-aware: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_delete_owned_namespaced_resource' "$$script" || { echo "E2E case does not clean owned namespaced resources: $$script" >&2; exit 1; }; \
		rg -Fq 'e2e_delete_owned_pvc' "$$script" || { echo "E2E case does not clean owned PVCs: $$script" >&2; exit 1; }; \
		owned_creates="$$(rg -c '^e2e_create_owned_resource' "$$script")"; \
		test "$$owned_creates" = 2 || { echo "E2E case must create only its SC and VAC as cluster resources: $$script" >&2; exit 1; }; \
		namespaced_creates="$$(rg -c '^e2e_create_owned_namespaced_resource' "$$script")"; \
		test "$$namespaced_creates" -ge 2 || { echo "E2E case does not create namespaced resources safely: $$script" >&2; exit 1; }; \
		rg -Fq '"$$manifest_dir/storageclass.yaml"' "$$script" || { echo "E2E case does not create its StorageClass safely: $$script" >&2; exit 1; }; \
		rg -Fq '"$$manifest_dir/volumeattributesclass.yaml"' "$$script" || { echo "E2E case does not create its VAC safely: $$script" >&2; exit 1; }; \
		if rg -n '^[[:space:]]*kube(_retry)?[[:space:]]+(apply|create)' "$$script"; then \
			echo "E2E case must create resources through ownership helpers: $$script" >&2; \
			exit 1; \
		fi; \
		if rg -n 'DRIVE9_CSI_IMAGE|e2e_render_driver_manifests' "$$script"; then \
			echo "E2E case must not prepare the Driver: $$script" >&2; \
			exit 1; \
		fi; \
	done
	@rg -Fq 'e2e_render_driver_manifests' $(E2E_PREPARE)
	@rg -Fq 'e2e_validate_driver_manifests' $(E2E_PREPARE)
	@rg -Fq 'e2e_require_prepared_driver "$$DRIVE9_CSI_IMAGE"' $(E2E_PREPARE)
	@rg -Fq 'DRIVE9_CSI_IMAGE' $(E2E_PREPARE)
	@if rg -n 'e2e_render_case_manifests|DRIVE9_(SERVER|API_KEY)|kube delete' $(E2E_PREPARE); then \
		echo "E2E prepare must not own case resources or delete the Driver" >&2; \
		exit 1; \
	fi
	@if rg -n '^[[:space:]]*kubectl([[:space:]]|$$)' $(E2E_SCRIPTS) e2e/lib/manifests.sh; then \
		echo "E2E cases and manifest helpers must use the kube wrapper" >&2; \
		exit 1; \
	fi
	@rg -Fq 'kubectl --context "$$DRIVE9_CSI_E2E_CONTEXT"' e2e/lib/common.sh
	@rg -Fq 'prod|production' e2e/lib/common.sh
	@rg -Fq 'DRIVE9_CSI_E2E_CONFIRM' e2e/lib/common.sh
	@rg -Fq 'DRIVE9_CSI_E2E_DRIVER_NAMESPACE' e2e/lib/common.sh
	@rg -Fq 'DRIVE9_CSI_E2E_NAMESPACE:-$$driver_namespace' e2e/lib/common.sh
	@rg -Fq 'DRIVE9_CSI_E2E_SECRET_NAME' e2e/lib/common.sh
	@rg -Fq 'e2e_require_validation_image()' e2e/lib/common.sh
	@rg -Fq 'e2e_kube_error_is_transient' e2e/lib/common.sh
	@rg -Fq 'kube_retry()' e2e/lib/common.sh
	@rg -Fq 'e2e_create_owned_resource()' e2e/lib/common.sh
	@rg -Fq 'e2e_create_owned_namespaced_resource()' e2e/lib/common.sh
	@rg -Fq 'e2e_delete_owned_namespaced_resource()' e2e/lib/common.sh
	@rg -Fq 'e2e_delete_owned_pvc()' e2e/lib/common.sh
	@rg -Fq 'kube_retry delete "$$@"' e2e/lib/common.sh
	@if rg -n 'config (current-context|use-context)' $(E2E_SCRIPTS) e2e/lib/*.sh; then \
		echo "E2E scripts must not read or change the current kubectl context" >&2; \
		exit 1; \
	fi
	@if rg -n -- '--(context|server|cluster)(=|[[:space:]])' $(E2E_SCRIPTS) e2e/lib/manifests.sh; then \
		echo "E2E scripts must not override the kube wrapper target" >&2; \
		exit 1; \
	fi
	@rg -Fq 'volumeAttributesClassName: $$volume_attributes_class' e2e/lib/manifests.sh
	@rg -Fq 'drive9.ai/secret-name: $$secret_name' e2e/lib/manifests.sh
	@if rg -n 'kind: Secret|DRIVE9_(SERVER|API_KEY)|e2e_write_case_namespace' $(E2E_CASES) e2e/lib/manifests.sh; then \
		echo "E2E cases must reuse a pre-provisioned Secret and namespace" >&2; \
		exit 1; \
	fi
	@rg -Fq 'registry\.invalid\/drive9-csi:unpublished' e2e/lib/manifests.sh
	@rg -Fq 'using Drive9 workspace root mode' e2e/basic-lifecycle.sh
	@rg -Fq 'read after PVC recreation' e2e/basic-lifecycle.sh
	@rg -Fq '/tmp/drive9-survival-stop' e2e/mount-survival.sh
	@rg -Fq '/tmp/drive9-survival-stopped' e2e/mount-survival.sh
	@if rg -n 'drive9-survival-pid|kill -9' e2e/mount-survival.sh; then \
		echo "mount-survival I/O loop must stop cooperatively" >&2; \
		exit 1; \
	fi
	@test ! -e hack/e2e-k8s.sh

.PHONY: check
check: test test-race vet test-linux-compile build-check manifest-check \
	script-check e2e-check diff-check

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
	kubectl apply -k deploy/overlays/local
