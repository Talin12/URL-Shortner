# Build stage. CGO is off so the binary runs on a scratch-adjacent base.
FROM golang:1.26-alpine AS build

WORKDIR /src

# Copy manifests first so dependency download is cached independently of source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/linkflow ./cmd/linkflow

FROM alpine:3.20

# curl is here only so compose can health-check the container.
RUN apk add --no-cache ca-certificates curl \
    && adduser -D -u 10001 linkflow

COPY --from=build /out/linkflow /usr/local/bin/linkflow

USER linkflow
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/linkflow"]
