# syntax=docker/dockerfile:1.7
###############################################################################
# tequio-gateway — MULTI-STAGE DOCKERFILE (vendor)
###############################################################################
# vendor/ lo genera el job de tests de la CI (`go mod vendor`, donde están las
# credenciales para módulos privados como el repo de protos) y viaja como
# artifact al build de la imagen. Este build NO toca la red para resolver
# dependencias.
#
# Build local: `make image` (= go mod vendor + docker build).

# ──────────────────────────────────────────────
# STAGE 1: BUILD
# ──────────────────────────────────────────────
FROM golang:1.26.9 AS builder

# GOWORK=off: -mod=vendor es incompatible con go.work (ver `make workspace`).
ENV CGO_ENABLED=0 \
    GOOS=linux \
    GOARCH=amd64 \
    GOWORK=off

WORKDIR /src

COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -mod=vendor -o /out/tequio-gateway ./cli

# ──────────────────────────────────────────────
# STAGE 2: RUNTIME
# ──────────────────────────────────────────────
# distroless/static (sin shell ni gestor de paquetes; trae certificados CA
# para TLS hacia los upstreams), usuario nonroot. Fijada por DIGEST: un tag
# puede cambiar de contenido; Renovate/Dependabot proponen el digest nuevo.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

WORKDIR /app

COPY --from=builder /out/tequio-gateway /app/tequio-gateway
# La tabla de rutas viaja en la imagen (versionada con el código).
COPY --from=builder /src/routes.yaml /app/routes.yaml

USER nonroot:nonroot

ENV APP_ENV=production \
    LOG_LEVEL=info \
    HTTP_PORT=8080 \
    ROUTES_FILE=/app/routes.yaml

EXPOSE 8080

ENTRYPOINT ["/app/tequio-gateway"]
