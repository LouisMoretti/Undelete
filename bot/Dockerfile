# syntax=docker/dockerfile:1

# --- Build stage ---
FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: static binary, required to run on the minimal alpine
# runtime image without a C toolchain.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/undelete-bot ./cmd/bot

# --- Runtime stage ---
FROM alpine:3.20

# ca-certificates is required for TLS validation of outbound calls to
# api.telegram.org: without this package, all HTTPS calls fail with a
# certificate error, silently in the sense that the error message does not
# point at this missing package.
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 -S undelete && \
    adduser -u 10001 -S -G undelete undelete

WORKDIR /app
COPY --from=build /out/undelete-bot /app/undelete-bot

USER 10001:10001

ENTRYPOINT ["/app/undelete-bot"]
