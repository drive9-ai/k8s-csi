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
COPY build/drive9-cli.lock.json /tmp/drive9-cli.lock.json
COPY --from=csi-builder /out/drive9-csi-build /usr/local/bin/drive9-csi-build
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl jq \
 && rm -rf /var/lib/apt/lists/*
RUN target_arch="${TARGETARCH:-amd64}" \
 && platform="linux/${target_arch}" \
 && url="$(jq -er --arg platform "${platform}" '.current.artifacts[$platform].url' /tmp/drive9-cli.lock.json)" \
 && digest="$(jq -er --arg platform "${platform}" '.current.artifacts[$platform].sha256 | select(test("^[0-9a-f]{64}$"))' /tmp/drive9-cli.lock.json)" \
 && mkdir -p /out \
 && curl --proto '=https' --tlsv1.2 -fsSL -o /out/drive9 "${url}" \
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
