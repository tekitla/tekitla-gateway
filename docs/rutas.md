# Rutas, límites y errores — tekitla-gateway

## De dónde salen las rutas

```
.proto con google.api.http ──buf──▶ *.pb.gw.go + OpenAPI ──▶ app/upstream/registry.go ──▶ routes.yaml
   (repo de protos)                  (repo de protos)        (qué servicios expone)      (quién puede llamar)
```

1. **Rutas HTTP**: las anotaciones `google.api.http` de los `.proto`
   ([grpc-gateway](https://github.com/grpc-ecosystem/grpc-gateway)). El OpenAPI
   generado de esos mismos `.proto` es la única fuente de rutas (OWASP API9).
   Versión en el path (`/v1/...`).
2. **Servicios expuestos**: `app/upstream/registry.go` (nombre gRPC, función
   `Register...Handler` generada y nombre del upstream → `UPSTREAM_<NAME>_ADDR`).
3. **Autorización**: `routes.yaml`, **denegar por defecto**.

## Rutas de Tekitla

Las 17 de `issues.v1.IssueService` (tekitla-protos `v0.3.0`: rebanada 1 y 3a),
todas con `roles: [allowlist]` (docs/auth.md). Lecturas: bucket global
(600/min, ráfaga 100). Escrituras: body ≤ 64 KiB (`UpdateIssue`, 128 KiB por la
descripción), timeout 5 s (`DeleteStatus`, 10 s: mueve issues) y bucket propio
por usuario.

| Método | Ruta | Rate limit |
|---|---|---|
| `ListProjects` | `GET /v1/projects` | global |
| `CreateProject` | `POST /v1/projects` | 30/min, ráfaga 10 |
| `ListIssues` | `GET /v1/projects/{project_id}/issues` | global |
| `GetIssue` | `GET /v1/projects/{project_id}/issues/{number}` | global |
| `CreateIssue` | `POST /v1/projects/{project_id}/issues` | 60/min, ráfaga 20 |
| `MoveIssue` | `POST /v1/projects/{project_id}/issues/{number}:move` | 120/min, ráfaga 30 |
| `UpdateIssue` | `PATCH /v1/projects/{project_id}/issues/{number}` | 120/min, ráfaga 30 |
| `CreateComment` | `POST /v1/projects/{project_id}/issues/{number}/comments` | 60/min, ráfaga 20 |
| `ListIssueEvents` | `GET /v1/projects/{project_id}/issues/{number}/events` | global |
| `ListStatuses` | `GET /v1/projects/{project_id}/statuses` | global |
| `CreateStatus` | `POST /v1/projects/{project_id}/statuses` | 30/min, ráfaga 10 |
| `UpdateStatus` | `PATCH /v1/projects/{project_id}/statuses/{status_id}` | 30/min, ráfaga 10 |
| `DeleteStatus` | `DELETE /v1/projects/{project_id}/statuses/{status_id}` | 30/min, ráfaga 10 |
| `ListLabels` | `GET /v1/projects/{project_id}/labels` | global |
| `CreateLabel` | `POST /v1/projects/{project_id}/labels` | 30/min, ráfaga 10 |
| `UpdateLabel` | `PATCH /v1/projects/{project_id}/labels/{label_id}` | 30/min, ráfaga 10 |
| `DeleteLabel` | `DELETE /v1/projects/{project_id}/labels/{label_id}` | 30/min, ráfaga 10 |

## routes.yaml

Clave = método gRPC completo (`<paquete>.<Servicio>/<Método>`). Cada entrada
declara exactamente una de `public: true`, `roles: [...]` (alcanza con uno) o
`deny: true`, y opcionalmente `timeout`, `max_body_bytes`, `rate_limit` y
`sunset`. Formato completo en el encabezado de `routes.yaml`.

### Política (qué pasa si algo no coincide)

| Situación | Resultado |
|---|---|
| Método anotado **sin** entrada en `routes.yaml` | **404** (no expuesto). Warning al arrancar y **falla** `TestRutasDeLaTablaExisten` en CI |
| Entrada `deny: true` | 404, igual que una ruta inexistente |
| Entrada de un método que no existe o sin `google.api.http` | el gateway **no arranca** (y fallan los tests) |
| Campo desconocido o mal escrito en `routes.yaml` | el gateway no arranca |
| Ruta con `roles` y `auth = ninguno` | el gateway no arranca |
| Verbo equivocado sobre una ruta existente | 404 (no 405: no revela qué existe) |

Además, el interceptor gRPC verifica que el método que se llama sea el que
autorizó la tabla; si no coincide, la llamada no sale (500).

### Agregar una ruta

1. **Repo de protos**: el RPC con su anotación, p. ej.
   `option (google.api.http) = {post: "/v1/projects/{project_id}:archive" body: "*"};`.
   Rutas de administración bajo un prefijo propio (`/v1/admin/...`).
   `make generate` regenera `*.pb.gw.go` y `gen/openapi/api.swagger.json`;
   release del repo de protos.
2. **Este repo**: `go get <protos_module>@<versión>`. Si es un servicio nuevo,
   su entrada en `app/upstream/registry.go` y `UPSTREAM_<NAME>_ADDR` en
   `.env.example`, `docker-compose.yml` y `docker-compose.coolify.yml`.
3. **routes.yaml**: la entrada con su política (y `rate_limit` propio si es
   login, registro, recuperar clave, búsquedas o algo caro).
4. `go test ./...`: `TestAutorizacionPorRuta` genera los casos de la ruta
   nueva solo (sin token → 401, sin rol → 403, con cada rol → 200; pública →
   200; deny → 404).

## Límites

| Control | Dónde | Default |
|---|---|---|
| Body máximo | `max_body_bytes` (ruta o `defaults`) | 1 MiB → `413` (también si viene chunked) |
| Content-Type | rutas con body | `application/json` → si no, `415` |
| JSON estricto | todas | campo desconocido → `400` |
| Deadline gRPC | `timeout` (ruta o `defaults`) | 10 s → `504`; se propaga al servicio |
| Timeouts HTTP | `HTTP_READ_HEADER_TIMEOUT` / `READ` / `WRITE` / `IDLE` | 5 s / 15 s / 35 s / 60 s (slowloris) |
| Rate limit | `rate_limit` (ruta o `defaults`) | token bucket 600/min, ráfaga 100 → `429` |

**Rate limit**: clave = usuario (`sub` o `apikey:<id>`) si hay identidad; si
no, IP del cliente (`TRUSTED_PROXY_HOPS` decide qué IP de `X-Forwarded-For`
es confiable). Una ruta con `rate_limit` propio tiene su bucket; el resto
comparte el de `defaults`. Respuesta: `429` con `Retry-After` y las cabeceras
`RateLimit-Limit`, `RateLimit-Remaining`, `RateLimit-Reset` (también en las
respuestas exitosas).

Almacenamiento: **en memoria, por instancia** (fase 1). Para un límite global entre
instancias hace falta el store en Redis (fase 3 de la plantilla:
`app/ratelimit.Limiter` es la interfaz a implementar). La capa volumétrica
(DDoS) va delante: Cloud Armor o el middleware de Traefik.

## Errores

Formato único para todo lo que responde el gateway:

```json
{"error": {"code": "FAILED_PRECONDITION", "message": "el orden cambió; refrescá y reintentá", "reason": "STALE_ORDER", "request_id": "3f1c..."}}
```

- `code`: nombre del código gRPC (`INVALID_ARGUMENT`, `NOT_FOUND`,
  `UNAUTHENTICATED`, `RESOURCE_EXHAUSTED`...) o `PAYLOAD_TOO_LARGE` /
  `UNSUPPORTED_MEDIA_TYPE`.
- Errores de dominio de los servicios (4xx: validación, no encontrado,
  conflicto, precondición) pasan con su mensaje.
- `reason` (opcional): en esos 4xx, el `reason` del `google.rpc.ErrorInfo`
  que manda el servicio, para distinguir errores con el mismo `code`. El
  cliente decide con `code` + `reason`, nunca con `message` (es para
  humanos). Solo se reenvía si es `UPPER_SNAKE` de hasta 64 caracteres; sin
  `ErrorInfo`, el campo no viene. `domain` y `metadata` del `ErrorInfo` no
  salen: pueden llevar datos internos. Los de `tekitla-issues`:

  | `code` | `reason` | Cuándo |
  |---|---|---|
  | `FAILED_PRECONDITION` | `STALE_ORDER` | reordenar con vecinos que ya no están en la columna o en ese orden (refrescar y reintentar) |
  | `FAILED_PRECONDITION` | `LAST_STATUS_OF_CATEGORY` | borrar el último estado de una categoría |
  | `FAILED_PRECONDITION` | `STATUS_HAS_ISSUES` | borrar un estado con issues sin `move_to_status_id` |
  | `ALREADY_EXISTS` | `STATUS_NAME_TAKEN` | nombre de estado repetido en el proyecto |
  | `ALREADY_EXISTS` | `LABEL_NAME_TAKEN` | nombre de etiqueta repetido en el proyecto |
  | `ALREADY_EXISTS` | `PROJECT_KEY_TAKEN` | key de proyecto repetida en el espacio |

  Los demás errores de dominio traen como `reason` el mismo `code` (`NOT_FOUND`, `INVALID_ARGUMENT`...): no agrega información.
- Todo 5xx (`INTERNAL`, `UNKNOWN`, `UNAVAILABLE`, `DEADLINE_EXCEEDED`...)
  sale con un mensaje **genérico**; el detalle real queda en el log con el
  mismo `request_id`. Nunca stack traces ni mensajes internos.

## Endpoints propios del gateway

| Ruta | Para qué |
|---|---|
| `GET /healthz` | liveness (probes, `<binario> healthcheck`) |
| `GET /readyz` | `grpc.health.v1` de cada upstream (resultado cacheado 5 s) |
| `GET /openapi.json` | OpenAPI de los protos, si `OPENAPI_ENABLED=true` (default: todo salvo production) |

## Logs

Una línea JSON por request (`msg=http_request`): método, path (**sin** query
string), ruta gRPC, status, duración, bytes, `request_id`, IP, `user_id`. En
`LOG_LEVEL=debug` se agregan las cabeceras con `Authorization`, `Cookie`,
`X-API-Key` y todo lo que parezca token/clave tapado (`[REDACTED]`). Nunca
se loguean cuerpos.
