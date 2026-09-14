# syntax=docker/dockerfile:1

# --- Build stage ---
# --platform=$BUILDPLATFORM: the Go toolchain runs natively on the builder
# (fast, no emulation) and cross-compiles for $TARGETARCH below. Without
# this, a `buildx build --platform linux/arm64` on an amd64 runner would
# either emulate the whole Go toolchain (slow, needs QEMU) or -- without
# emulation -- silently embed an amd64 binary into an arm64 image.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build

# TARGETARCH is provided by BuildKit (amd64, arm64, ...). CGO_ENABLED=0 +
# pure-Go dependencies make the cross-compile safe.
ARG TARGETARCH

WORKDIR /src

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: static binary, required to run on the minimal alpine
# runtime image without a C toolchain. GOARCH pins the output to the target
# platform (see TARGETARCH above): without it the binary would silently
# match the builder instead.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -o /out/undelete-bot ./cmd/bot

# --- Runtime stage ---
FROM alpine:3.22

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
