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
ARG DRIVE9_CLI_RELEASE_COMMIT
COPY --from=csi-builder /out/drive9-csi-build /usr/local/bin/drive9-csi-build
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN release_commit="${DRIVE9_CLI_RELEASE_COMMIT}" \
 && if ! printf '%s' "${release_commit}" | grep -Eq '^[0-9a-f]{40}$'; then echo "DRIVE9_CLI_RELEASE_COMMIT must be a full commit SHA" >&2; exit 1; fi \
 && target_arch="${TARGETARCH:-amd64}" \
 && case "${target_arch}" in amd64|arm64) ;; *) echo "unsupported Drive9 CLI architecture: ${target_arch}" >&2; exit 1 ;; esac \
 && artifact="drive9-linux-${target_arch}" \
 && release_base="https://raw.githubusercontent.com/mem9-ai/drive9-fe/${release_commit}/site/releases" \
 && curl --proto '=https' --tlsv1.2 -fsSL -o /tmp/checksums.txt "${release_base}/checksums.txt" \
 && digest="$(awk -v artifact="${artifact}" '$2 == artifact { count += 1; value = $1 } END { if (count == 1) print value }' /tmp/checksums.txt)" \
 && if ! printf '%s' "${digest}" | grep -Eq '^[0-9a-f]{64}$'; then echo "release must contain exactly one valid checksum for ${artifact}" >&2; exit 1; fi \
 && mkdir -p /out \
 && curl --proto '=https' --tlsv1.2 -fsSL -o /out/drive9 "${release_base}/${artifact}" \
 && printf '%s  %s\n' "${digest}" /out/drive9 | sha256sum -c - \
 && chmod +x /out/drive9 \
 && /usr/local/bin/drive9-csi-build verify-host-binary --path=/out/drive9 --target-arch="${target_arch}" \
 && echo "Downloaded drive9 CLI for linux/${target_arch}"

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source=https://github.com/drive9-ai/k8s-csi
LABEL org.opencontainers.image.description="Drive9 Kubernetes CSI driver"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fuse3 tar tini util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=csi-builder /out/drive9-csi-launcher /usr/local/bin/drive9-csi-launcher
COPY --from=drive9-downloader /out/drive9 /usr/local/bin/drive9
COPY hack/drive9-csi-upload-perf.sh /usr/local/bin/drive9-csi-upload-perf
RUN chmod +x /usr/local/bin/drive9-csi-upload-perf
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
