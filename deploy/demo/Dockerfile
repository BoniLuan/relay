FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal ./internal
COPY cmd/relay-demo ./cmd/relay-demo
RUN CGO_ENABLED=0 go build -trimpath -o /out/relay-demo ./cmd/relay-demo
FROM alpine:3.22
COPY --from=build /out/relay-demo /usr/local/bin/relay-demo
USER 10001:10001
ENTRYPOINT ["/usr/local/bin/relay-demo"]
