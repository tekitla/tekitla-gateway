# tekitla-gateway — contexto para Claude

## Qué es

Entrada pública REST de Tekitla: autentica con Firebase y traduce a gRPC.

Tekitla (issue 002, rebanadas 1 y 3a): expone `issues.v1.IssueService`
(`github.com/tekitla/tekitla-protos@v0.4.0`) del servicio `tekitla-issues`
(`UPSTREAM_ISSUES_ADDR`, gRPC :5000). Las 20 rutas (`docs/rutas.md`) exigen el rol `allowlist`,
que **no** sale del token: lo otorga `auth.EmailAllowlist` solo a los emails
verificados de `AUTH_ALLOWED_EMAILS` (vacía = nadie entra). Nada público
salvo `/healthz` y `/readyz`.

API gateway Go: **la única puerta pública** de la plataforma. Traduce REST/JSON
a gRPC con grpc-gateway (rutas desde las anotaciones `google.api.http` de los
`.proto`) y concentra los controles comunes. Auth: **firebase**. Deploy:
Coolify (Traefik delante; servicios en la red interna).
Generado desde la plantilla Copier `api-gateway` (`.copier-answers.yml`).

## Layout

| Path | Qué |
|---|---|
| `cli/main.go` | Wiring + ciclo de vida (`healthcheck` como subcomandos) |
| `routes.yaml` | Autorización por ruta: **denegar por defecto** |
| `app/upstream/registry.go` | Servicios gRPC expuestos (`UPSTREAM_<NAME>_ADDR`) |
| `app/gateway/` | Mux de grpc-gateway + middleware de ruta, errores, interceptor de metadata |
| `app/middleware/` | request-id, logs redactados, recover, cabeceras de seguridad, CORS, limpieza de cabeceras |
| `app/auth/` | JWT (Firebase/OIDC, JWKS en caché), API keys (SHA-256) y allowlist de emails (`allowlist.go`) |
| `app/routes/`, `app/ratelimit/`, `app/config/` | routes.yaml, token bucket en memoria, variables de entorno |

Pipeline (en orden): request-id → access log → recover → cabeceras → CORS →
borrar `X-User-Id`/`X-Roles`/`Grpc-*` del cliente → mux → ruta declarada (404)
→ body (413) → auth (401/503) → roles/allowlist (403) → rate limit (429) →
Content-Type (415) → deadline gRPC → interceptor (metadata desde cero).
Detalle: `docs/rutas.md`; auth: `docs/auth.md`.

## Agregar una ruta

1. **Repo de protos**: RPC con `option (google.api.http) = {...}`; `make generate`
   (genera `*.pb.gw.go` y el OpenAPI); MR y release.
2. Acá: `go get <protos>@<versión>`; si el servicio es nuevo, entrada en
   `app/upstream/registry.go` + `UPSTREAM_<NAME>_ADDR` en `.env.example`,
   `docker-compose.yml` y `docker-compose.coolify.yml`.
3. **routes.yaml**: `<paquete>.<Servicio>/<Método>` con `public: true`,
   `roles: [...]` o `deny: true` (+ `rate_limit` estricto en login, registro,
   recuperar clave, búsquedas). En Tekitla (rebanada 1): `roles: [allowlist]`.
4. `go test ./...`: `TestAutorizacionPorRuta` genera los casos solo
   (401/403/200; pública → 200; deny → 404; con `allowlist`, además 403 por
   email ajeno, sin verificar o rol puesto como claim). Si falla por "sin
   entrada", la ruta no está declarada. `TestRutasDeTekitlaExigenAllowlist`
   fija la lista de rutas expuestas: actualizala si el contrato cambia.

## Repo de protos

`github.com/tekitla/tekitla-protos` (hoy `v0.4.0`): el handler sale de
`<módulo>/gen/go/issues/v1` (`app/upstream/registry.go`) y el OpenAPI de
`<módulo>/gen/openapi` (`cli/main.go`). Ya no hay `proto/`, `gen/` ni `buf.*`
locales. Versión nueva: `go get github.com/tekitla/tekitla-protos@<tag>`. Sin
tag (co-desarrollo): `go mod edit -replace` o `make workspace`.

## Reglas de seguridad (no negociables)

1. **Denegar por defecto**: ninguna ruta se expone sin entrada en `routes.yaml`.
   Nunca agregar un "catch-all" ni un modo que exponga todo el mux.
2. Algoritmos JWT en lista cerrada (RS256/ES256). Validar siempre iss, aud,
   exp, nbf; tolerancia ≤ 60 s. Nunca `none` ni HS*.
3. Los servicios reciben identidad **solo** por la metadata que arma el
   interceptor (`x-user-id`, `x-roles`, `x-request-id`). Nunca reenviar
   cabeceras del cliente (ni `Authorization`) ni metadata de vuelta.
4. Errores 5xx: mensaje genérico + `request_id`. Nunca devolver `err.Error()`
   de un upstream ni stack traces.
5. Logs: nunca query strings, cuerpos ni cabeceras sin `RedactHeaders`.
6. CORS con lista exacta por entorno; nunca `*`.
7. Secretos (API keys, tokens): solo en Secret Manager / panel de Coolify /
   `.env.dev`. Las API keys se guardan como SHA-256. gitleaks los busca en
   cada commit (`pre-commit install`) y en el CI; uno filtrado se rota
   primero (`SECURITY.md`). Título del MR: `feat(issue-<n>): …`.
8. Autorización **por objeto** (dueño/organización) es de los servicios: el
   gateway no conoce los datos.
9. Cambios en auth, CORS, cabeceras, límites o en `routes.yaml` con
   `public: true` se justifican en el MR.

## Comandos

```bash
cp .env.example .env.dev      # una vez
make run                      # gateway local (lee .env.dev)
make up / make down           # con docker compose
make test                     # incluye los tests de autorización por ruta
make lint                     # gofmt + go vet
make vuln                     # govulncheck
make scan                     # imagen + Trivy (HIGH/CRITICAL con fix -> falla)
make workspace / workspace-off     # co-desarrollar con un checkout local de los protos
# e2e local sin tokens reales: FIREBASE_JWKS_URL (solo APP_ENV=development), ver docs/auth.md
curl -s localhost:8080/openapi.json   # rutas (OPENAPI_ENABLED=true)
```

## Tests

Solo stdlib `testing` (nada de testify ni librerías de mocks). El upstream es
un servidor gRPC falso con `bufconn` que atiende cualquier método
(`app/gateway/helpers_test.go`); los JWT de prueba los firma
`app/auth/authtest` (discovery + JWKS con `httptest`).
