FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS csi-builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ARG BUILDOS
ARG BUILDARCH
ARG TARGETOS
ARG TARGETARCH
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
WORKDIR /src/csi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN target_os="${TARGETOS:-linux}" \
 && target_arch="${TARGETARCH:-amd64}" \
 && CGO_ENABLED=0 GOOS="${target_os}" GOARCH="${target_arch}" go build -o /out/drive9-csi ./cmd/drive9-csi \
 && CGO_ENABLED=0 GOOS="${target_os}" GOARCH="${target_arch}" go build -o /out/drive9-csi-launcher ./cmd/drive9-csi-launcher \
 && CGO_ENABLED=0 GOOS="${BUILDOS:-linux}" GOARCH="${BUILDARCH:-amd64}" go build -o /out/drive9-csi-build ./cmd/drive9-csi \
 && /out/drive9-csi-build verify-host-binary --path=/out/drive9-csi --target-arch="${target_arch}" \
 && /out/drive9-csi-build verify-host-binary --path=/out/drive9-csi-launcher --target-arch="${target_arch}"

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS drive9-downloader
ARG TARGETARCH
ARG DRIVE9_CLI_VERSION
ARG DRIVE9_CLI_LINUX_AMD64_SHA256
ARG DRIVE9_CLI_LINUX_ARM64_SHA256
COPY --from=csi-builder /out/drive9-csi-build /usr/local/bin/drive9-csi-build
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN expected_version="${DRIVE9_CLI_VERSION}" \
 && target_arch="${TARGETARCH:-amd64}" \
 && case "${target_arch}" in amd64|arm64) ;; *) echo "unsupported Drive9 CLI architecture: ${target_arch}" >&2; exit 1 ;; esac \
 && expected_digest="" \
 && case "${target_arch}" in amd64) expected_digest="${DRIVE9_CLI_LINUX_AMD64_SHA256}" ;; arm64) expected_digest="${DRIVE9_CLI_LINUX_ARM64_SHA256}" ;; esac \
 && artifact="drive9-linux-${target_arch}" \
 && release_base="https://drive9.ai/releases" \
 && case "${expected_version}:${expected_digest}" in \
      :) curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/version "${release_base}/version" \
       && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/checksums.txt "${release_base}/checksums.txt" \
       && published_version="$(tr -d '[:space:]' < /tmp/version)" \
       && digest="$(awk -v artifact="${artifact}" '$2 == artifact { count += 1; value = $1 } END { if (count == 1) print value }' /tmp/checksums.txt)" ;; \
      :*|*:) echo "DRIVE9_CLI_VERSION and the platform checksum must be provided together" >&2; exit 1 ;; \
      *) published_version="${expected_version}"; digest="${expected_digest}" ;; \
    esac \
 && if ! printf '%s' "${published_version}" | grep -Eq '^[0-9a-f]{7}$'; then echo "published Drive9 CLI version must be a seven-character commit prefix" >&2; exit 1; fi \
 && if ! printf '%s' "${digest}" | grep -Eq '^[0-9a-f]{64}$'; then echo "release must contain exactly one valid checksum for ${artifact}" >&2; exit 1; fi \
 && mkdir -p /out \
 && curl --proto '=https' --tlsv1.2 -fsSL -o /out/drive9 "${release_base}/${artifact}" \
 && printf '%s  %s\n' "${digest}" /out/drive9 | sha256sum -c - \
 && chmod +x /out/drive9 \
 && /usr/local/bin/drive9-csi-build verify-host-binary --path=/out/drive9 --target-arch="${target_arch}" \
 && echo "Downloaded published drive9 CLI ${published_version} for linux/${target_arch}"

FROM --platform=$TARGETPLATFORM debian:bookworm-slim AS runtime
LABEL org.opencontainers.image.source=https://github.com/drive9-ai/k8s-csi
LABEL org.opencontainers.image.description="Drive9 Kubernetes CSI driver"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tar tini util-linux \
 && rm -rf /var/lib/apt/lists/*
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=csi-builder /out/drive9-csi-launcher /usr/local/bin/drive9-csi-launcher
COPY --from=drive9-downloader /out/drive9 /usr/local/bin/drive9
COPY hack/drive9-csi-upload-perf.sh /usr/local/bin/drive9-csi-upload-perf
RUN chmod +x /usr/local/bin/drive9-csi-upload-perf \
 && mount_help="$(/usr/local/bin/drive9 mount --supervise-foreground --direct-mount-strict --help 2>&1)" \
 && printf '%s\n' "${mount_help}" | grep -F -- '-supervise-foreground' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- '-gvisor-compat' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- '-local-only ' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- '-remote-only ' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- 'DRIVE9_MOUNT_GVISOR_COMPAT' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- 'DRIVE9_MOUNT_LOCAL_ONLY_PATTERNS' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- 'DRIVE9_MOUNT_REMOTE_ONLY_PATTERNS' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- '-profile ' >/dev/null \
 && printf '%s\n' "${mount_help}" | grep -F -- '-durability ' >/dev/null
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
