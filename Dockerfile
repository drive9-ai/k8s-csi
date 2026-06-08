FROM golang:1.25-bookworm AS csi-builder
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
WORKDIR /src/csi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/drive9-csi ./cmd/drive9-csi

FROM golang:1.25-bookworm AS drive9-builder
ARG DRIVE9_REF=9c464af94a9cab374c3841a297a0a1eefd047977
ARG GOPROXY=https://proxy.golang.org,direct
ARG GOSUMDB=sum.golang.org
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
WORKDIR /src
RUN git clone https://github.com/mem9-ai/drive9.git drive9 \
 && cd drive9 \
 && git checkout "${DRIVE9_REF}"
WORKDIR /src/drive9
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/drive9 ./cmd/drive9

FROM debian:bookworm-slim
LABEL org.opencontainers.image.source=https://github.com/drive9-ai/k8s-csi
LABEL org.opencontainers.image.description="Drive9 Kubernetes CSI driver"
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fuse3 tini util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=drive9-builder /out/drive9 /usr/local/bin/drive9
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
