FROM golang:1.24.4-alpine3.22 as builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux \
    go build \
    -trimpath \
    -ldflags="-s -w" \
    -o api

FROM alpine:3.22
RUN apk add --no-cache ffmpeg ca-certificates
RUN addgroup -S nonroot && adduser -S nonroot -G nonroot
WORKDIR /app
COPY --from=builder --chown=nonroot:nonroot /app/api .
USER nonroot:nonroot
ENTRYPOINT ["./api"]
