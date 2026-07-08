FROM --platform=$BUILDPLATFORM golang:1.26-bookworm AS csi-builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
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
 && case "${target_arch}" in \
      amd64) expected_machine="3e 00" ;; \
      arm64) expected_machine="b7 00" ;; \
      *) echo "unsupported TARGETARCH=${target_arch}" >&2; exit 1 ;; \
    esac \
 && actual_machine="$(od -An -tx1 -j 18 -N 2 /out/drive9-csi | tr -s ' ' | sed 's/^ //; s/ $//')" \
 && if [ "${actual_machine}" != "${expected_machine}" ]; then \
      echo "drive9-csi ELF machine ${actual_machine}, expected ${expected_machine} for ${target_arch}" >&2; \
      exit 1; \
    fi

FROM --platform=$BUILDPLATFORM debian:bookworm-slim AS drive9-downloader
ARG TARGETARCH
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN target_arch="${TARGETARCH:-amd64}" \
 && mkdir -p /out \
 && curl -fsSL -o /out/drive9 "https://drive9.ai/releases/drive9-linux-${target_arch}" \
 && chmod +x /out/drive9 \
 && case "${target_arch}" in \
      amd64) expected_machine="3e 00" ;; \
      arm64) expected_machine="b7 00" ;; \
      *) echo "unsupported TARGETARCH=${target_arch}" >&2; exit 1 ;; \
    esac \
 && actual_machine="$(od -An -tx1 -j 18 -N 2 /out/drive9 | tr -s ' ' | sed 's/^ //; s/ $//')" \
 && if [ "${actual_machine}" != "${expected_machine}" ]; then \
      echo "drive9 ELF machine ${actual_machine}, expected ${expected_machine} for ${target_arch}" >&2; \
      exit 1; \
    fi \
 && echo "Downloaded drive9 CLI for linux/${target_arch}"

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source=https://github.com/drive9-ai/k8s-csi
LABEL org.opencontainers.image.description="Drive9 Kubernetes CSI driver"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fuse3 tar tini util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=drive9-downloader /out/drive9 /usr/local/bin/drive9
COPY hack/drive9-csi-upload-perf.sh /usr/local/bin/drive9-csi-upload-perf
RUN chmod +x /usr/local/bin/drive9-csi-upload-perf
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
