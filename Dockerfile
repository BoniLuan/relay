FROM golang:1.27.3-alpine@sha256:b8c9b82affbbb82a020bbcf998dad2ba9516944ac7e883524642a481a9505e9c AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/relay ./cmd/relay

FROM alpine:3.22@sha256:d406446bc05b82480756ce8f1287964d28768c23955fa9480d2a74a2eae7a654
COPY --from=build /out/relay /usr/local/bin/relay
USER 10001:10001
ENV RELAY_HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/relay"]
