FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/entente ./cmd/entente

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/entente /usr/local/bin/entente
ENTRYPOINT ["/usr/local/bin/entente"]
CMD ["-config", "/etc/entente/entente.yaml"]
