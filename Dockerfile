FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 在构建机原生架构上交叉编译,避免多架构构建走 QEMU。
ARG TARGETOS TARGETARCH
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/entente ./cmd/entente

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/entente /usr/local/bin/entente
ENTRYPOINT ["/usr/local/bin/entente"]
CMD ["--config", "/etc/entente/entente.yaml"]
