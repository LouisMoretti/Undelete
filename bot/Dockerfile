# syntax=docker/dockerfile:1

# --- Étape de build ---
FROM golang:1.23-alpine AS build

WORKDIR /src

COPY go.mod ./
COPY go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 : binaire statique, nécessaire pour tourner sur l'image
# runtime alpine minimale sans toolchain C.
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/undelete-bot ./cmd/bot

# --- Étape runtime ---
FROM alpine:3.20

# ca-certificates est requis pour la validation TLS des appels sortants vers
# api.telegram.org : sans ce paquet, tous les appels HTTPS échouent avec une
# erreur de certificat, silencieusement au sens où le message d'erreur ne
# pointe pas vers ce paquet manquant.
RUN apk add --no-cache ca-certificates && \
    addgroup -g 10001 -S undelete && \
    adduser -u 10001 -S -G undelete undelete

WORKDIR /app
COPY --from=build /out/undelete-bot /app/undelete-bot

USER 10001:10001

ENTRYPOINT ["/app/undelete-bot"]
