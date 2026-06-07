FROM golang:1.24-bookworm AS csi-builder
WORKDIR /src/csi
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/drive9-csi ./cmd/drive9-csi

FROM golang:1.24-bookworm AS drive9-builder
ARG DRIVE9_REF=main
WORKDIR /src
RUN git clone --depth=1 --branch "${DRIVE9_REF}" https://github.com/mem9-ai/drive9.git drive9
WORKDIR /src/drive9
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/drive9 ./cmd/drive9

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates fuse3 tini util-linux \
 && rm -rf /var/lib/apt/lists/* \
 && sed -i 's/^#user_allow_other/user_allow_other/' /etc/fuse.conf
COPY --from=csi-builder /out/drive9-csi /usr/local/bin/drive9-csi
COPY --from=drive9-builder /out/drive9 /usr/local/bin/drive9
ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/drive9-csi"]
