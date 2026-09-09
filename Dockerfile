FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /out/relay ./cmd/relay

FROM alpine:3.22
COPY --from=build /out/relay /usr/local/bin/relay
USER 10001:10001
ENV RELAY_HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/relay"]
