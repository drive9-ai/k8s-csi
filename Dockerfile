FROM golang:1.26-bookworm AS csi-builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
WORKDIR /src/csi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /out/drive9-csi ./cmd/drive9-csi

FROM debian:bookworm-slim AS drive9-downloader
ARG TARGETARCH=amd64
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl \
 && rm -rf /var/lib/apt/lists/*
RUN mkdir -p /out \
 && curl -fsSL -o /out/drive9 "https://drive9.ai/releases/drive9-linux-${TARGETARCH}" \
 && chmod +x /out/drive9 \
 && echo "Downloaded drive9 CLI binary version:" \
 && /out/drive9 --version

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source=https://github.com/drive9-ai/k8s-csi
LABEL org.opencontainers.image.description="Drive9 Kubernetes CSI driver"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fuse3 tini util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=drive9-downloader /out/drive9 /usr/local/bin/drive9
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
