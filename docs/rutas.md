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

Las 21 de `issues.v1.IssueService` (tekitla-protos `v0.7.0`: rebanada 1, 3a e
issues 011, 013, 025 y 036; el 012 suma parámetros a `ListIssues`), con
`roles: [allowlist, issues:read]` las lecturas y `[allowlist, issues:write]`
las escrituras (sesión de Firebase en la allowlist **o** un token `tka_` con
ese scope; docs/auth.md), y las 6 de `issues.v1.ActorService` (issue 025),
solo con `allowlist`. `ImportIssue` e `ImportIssueEvents` (`/v1/admin/...`)
están declaradas con `deny: true`: no se exponen (el importador va directo
por gRPC). Lecturas: bucket global (600/min, ráfaga
100), salvo `ListIssues`. Escrituras: body ≤ 64 KiB (`UpdateIssue`,
128 KiB por la descripción), timeout 5 s (`DeleteStatus` y
`BatchUpdateStatuses`, 10 s: mueven issues) y bucket propio por usuario.

| Método | Ruta | Rate limit |
|---|---|---|
| `ListProjects` | `GET /v1/projects` | global |
| `CreateProject` | `POST /v1/projects` | 30/min, ráfaga 10 |
| `ListIssues` | `GET /v1/projects/{project_id}/issues` | 300/min, ráfaga 60 (también busca) |
| `GetIssue` | `GET /v1/projects/{project_id}/issues/{number}` | global |
| `CreateIssue` | `POST /v1/projects/{project_id}/issues` | 60/min, ráfaga 20 |
| `MoveIssue` | `POST /v1/projects/{project_id}/issues/{number}:move` | 120/min, ráfaga 30 |
| `DiscardIssue` | `POST /v1/projects/{project_id}/issues/{number}:discard` | 60/min, ráfaga 20 |
| `RestoreIssue` | `POST /v1/projects/{project_id}/issues/{number}:restore` | 60/min, ráfaga 20 |
| `UpdateIssue` | `PATCH /v1/projects/{project_id}/issues/{number}` | 120/min, ráfaga 30 |
| `CreateComment` | `POST /v1/projects/{project_id}/issues/{number}/comments` | 60/min, ráfaga 20 |
| `ListIssueEvents` | `GET /v1/projects/{project_id}/issues/{number}/events` | global |
| `GetFlowMetrics` | `GET /v1/projects/{project_id}/flowMetrics` | 60/min, ráfaga 10; timeout 20 s (issue 036: recalcula la historia del proyecto) |
| `ListStatuses` | `GET /v1/projects/{project_id}/statuses` | global |
| `CreateStatus` | `POST /v1/projects/{project_id}/statuses` | 30/min, ráfaga 10 |
| `UpdateStatus` | `PATCH /v1/projects/{project_id}/statuses/{status_id}` | 30/min, ráfaga 10 |
| `DeleteStatus` | `DELETE /v1/projects/{project_id}/statuses/{status_id}` | 30/min, ráfaga 10 |
| `BatchUpdateStatuses` | `POST /v1/projects/{project_id}/statuses:batchUpdate` | 30/min, ráfaga 10 |
| `ListLabels` | `GET /v1/projects/{project_id}/labels` | global |
| `CreateLabel` | `POST /v1/projects/{project_id}/labels` | 30/min, ráfaga 10 |
| `UpdateLabel` | `PATCH /v1/projects/{project_id}/labels/{label_id}` | 30/min, ráfaga 10 |
| `DeleteLabel` | `DELETE /v1/projects/{project_id}/labels/{label_id}` | 30/min, ráfaga 10 |
| `ListActors` | `GET /v1/actors` | global (solo `allowlist`) |
| `CreateAgent` | `POST /v1/agents` | 10/min, ráfaga 5 (solo `allowlist`) |
| `RevokeActor` | `POST /v1/actors/{actor_id}:revoke` | 10/min, ráfaga 5 (solo `allowlist`) |
| `ListTokens` | `GET /v1/tokens` | global (solo `allowlist`) |
| `CreateToken` | `POST /v1/tokens` | 10/min, ráfaga 5 (solo `allowlist`) |
| `RevokeToken` | `POST /v1/tokens/{token_id}:revoke` | 30/min, ráfaga 10 (solo `allowlist`) |

Búsqueda, filtros y orden de `ListIssues` (issue 012) van en la query string,
con los nombres del `.proto`. Las listas repiten el parámetro y los enums
aceptan el nombre o el número:

```
GET /v1/projects/{project_id}/issues?query=tk-12&states=ISSUE_STATE_TODO&states=ISSUE_STATE_IN_PROGRESS
    &priorities=PRIORITY_HIGH&label_ids=<id>&status_ids=<id>&order_by=ISSUE_ORDER_UPDATED_DESC
    &page.limit=50&page.cursor=<next_cursor>
```

`state` (una sola categoría, el de v0.3) sigue valiendo, pero no junto con
`states`: los dos a la vez son `INVALID_ARGUMENT`. El cursor sirve solo con
los mismos filtros y orden: con otros responde `INVALID_ARGUMENT` y el cliente
vuelve a la primera página.

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
  humanos). Solo viene si el `reason` tiene el formato de `ErrorInfo`
  (`[A-Z][A-Z0-9_]+[A-Z0-9]`, hasta 63 caracteres) y dice algo más que el
  `code`: sin `ErrorInfo`, o con `reason` igual al `code` (`NOT_FOUND`), el
  campo no viene. `domain` y `metadata` del `ErrorInfo` no salen, tampoco en
  los errores a mitad de un stream: pueden llevar datos internos. Los de
  `tekitla-issues`:

  | `code` | `reason` | Cuándo |
  |---|---|---|
  | `FAILED_PRECONDITION` | `STALE_ORDER` | reordenar con vecinos que ya no están en la columna o en ese orden (refrescar y reintentar) |
  | `FAILED_PRECONDITION` | `LAST_STATUS_OF_CATEGORY` | borrar el último estado de una categoría (también en `BatchUpdateStatuses`) |
  | `FAILED_PRECONDITION` | `STATUS_HAS_ISSUES` | borrar un estado con issues sin `move_to_status_id` (también en `BatchUpdateStatuses`) |
  | `FAILED_PRECONDITION` | `STALE_STATUSES` | `BatchUpdateStatuses` sin un estado que existe: otro lo creó, recargar |
  | `FAILED_PRECONDITION` | `ISSUE_ALREADY_DISCARDED` | `DiscardIssue` de un issue que ya está en un estado `CANCELED` |
  | `FAILED_PRECONDITION` | `ISSUE_NOT_DISCARDED` | `RestoreIssue` de un issue que no está en un estado `CANCELED` |
  | `ALREADY_EXISTS` | `STATUS_NAME_TAKEN` | nombre de estado repetido en el proyecto |
  | `ALREADY_EXISTS` | `LABEL_NAME_TAKEN` | nombre de etiqueta repetido en el proyecto |
  | `ALREADY_EXISTS` | `PROJECT_KEY_TAKEN` | key de proyecto repetida en el espacio |
- Todo 5xx (`INTERNAL`, `UNKNOWN`, `UNAVAILABLE`, `DEADLINE_EXCEEDED`...)
  sale con un mensaje **genérico**; el detalle real queda en el log con el
  mismo `request_id`. Nunca stack traces ni mensajes internos.

## Endpoints propios del gateway

| Ruta | Para qué |
|---|---|
| `GET /healthz` | liveness (probes, `<binario> healthcheck`) |
| `GET /readyz` | `grpc.health.v1` de cada upstream (resultado cacheado 5 s) |
| `GET /openapi.json` | OpenAPI de los protos, si `OPENAPI_ENABLED=true` (default: todo salvo production) |
| `POST /mcp` | Servidor MCP para agentes (ver abajo) |

## Herramientas MCP (`/mcp`)

Mecanismo de la plantilla: un método de `routes.yaml` con `mcp:` también es
una herramienta.

```yaml
issues.v1.IssueService/GetIssue:
  roles: [allowlist, issues:read]
  mcp:
    tool: get_issue              # verbo_objeto, inglés, snake_case, único
    description: "Un issue por clave (RND-236): ..."
```

- **Misma política que la ruta REST.** La llamada se traduce al request HTTP
  y pasa por el mismo mux: auth, roles, rate limit, límites y deadline. Un rol
  que falta vuelve como resultado con `isError: true` y el error de la API
  (`PERMISSION_DENIED`), para que el agente sepa qué pasó.
- **`/mcp` pide credencial**: la misma de la API (`Authorization: Bearer`,
  Firebase o `tka_`). Sin ella, o vencida o revocada, 401 (también para
  listar las herramientas).
- **Anotaciones** desde el verbo: GET = sólo lectura (el cliente no pide
  confirmación). Respuestas de más de 256 KiB se cortan con un error que pide
  paginar.

**En Tekitla (issues 030 y 036) las cinco herramientas son propias del repo**
(`app/mcptools`), porque la genérica no alcanza: el contrato pide el id del
proyecto (los agentes conocen la clave `RND`), `get_issue` junta `GetIssue`
con el historial y los nombres de estados y etiquetas, y `next_issue` fija
filtros de `ListIssues` (que ya es `list_issues`; un método, una herramienta).
El gancho (`app/mcpserver/custom.go`) mantiene la regla de la plantilla:

- Una herramienta propia declara las rutas que llama (`Methods`); el gateway
  no arranca si alguna no está en `routes.yaml`, está en `deny`, no tiene
  `google.api.http` o no es GET (las propias son de solo lectura).
- Cada llamada entra al mux como request REST con la credencial del cliente:
  la política es la de esa ruta. No hay un segundo camino al servicio.
- Si el nombre coincide con un `mcp.tool` de `routes.yaml`, reemplaza a la
  genérica y toma la descripción de ahí; si no (`next_issue`), trae la suya.

| Herramienta | Declarada en | RPC que usa (todas `allowlist` o `issues:read`) |
|---|---|---|
| `list_projects` | `routes.yaml` (`ListProjects`) | `ListProjects` + `ListIssues` (cuenta por categoría, de a 100) |
| `list_issues` | `routes.yaml` (`ListIssues`) | `ListProjects` (clave → id), `ListStatuses`, `ListLabels`, `ListIssues` |
| `get_issue` | `routes.yaml` (`GetIssue`) | `ListProjects`, `GetIssue`, `ListIssueEvents` (se recorre y quedan los últimos), `ListStatuses`, `ListLabels` |
| `next_issue` | `app/mcptools` | `ListProjects`, `ListIssues` con `state=TODO`, `unassigned`, `order_by=PRIORITY_DESC`, `page.limit=1`, y nombres |
| `get_flow_metrics` | `routes.yaml` (`GetFlowMetrics`) | `ListProjects`, `ListStatuses`, `ListLabels` (nombres de etiquetas → ids), `GetFlowMetrics` con `from`/`to` = las 00:00 de los días pedidos en `time_zone` (`to` incluido) |

Ninguna ruta de escritura lleva `mcp:` (`TestMCPSoloRutasDeLectura`). Cada
RPC interno cuenta para el rate limit de su ruta, así que hay topes por
llamada (`app/mcptools`): 10 páginas de `ListProjects` (`projects_partial`),
20 páginas de `ListIssues` entre todos los proyectos para los conteos de
`list_projects` (`counts_partial`: son un mínimo) y 10 de `ListIssueEvents`
en `get_issue` (`events_partial`; con `events: 0` no se lee el historial).
Los conteos exactos sin recorrer issues necesitan un RPC de conteo en el
contrato (pendiente, fuera de esta fase). Las páginas internas se leen con
un tope de 8 MiB; lo que llega al agente, con el de 256 KiB.

**Errores:** el de la API tal cual (`code`, `message`, `reason`,
`request_id`) más un `hint` con qué hacer (pedir un token con
`issues:read`, revisar la clave con `list_projects`, empezar sin cursor...).
Una clave de proyecto que el token no ve es `NOT_FOUND` + `PROJECT_NOT_FOUND`.

**Auditoría:** una línea `mcp tool` por llamada con herramienta, `user_id`,
`agent_id` (token de agente), status y duración; cada RPC interno deja además
su `http_request`.

Configuración de los clientes (Claude Code, `.mcp.json`, Codex): `README.md`,
sección MCP.

## Logs

Una línea JSON por request (`msg=http_request`): método, path (**sin** query
string), ruta gRPC, status, duración, bytes, `request_id`, IP, `user_id`. En
`LOG_LEVEL=debug` se agregan las cabeceras con `Authorization`, `Cookie`,
`X-API-Key` y todo lo que parezca token/clave tapado (`[REDACTED]`). Nunca
se loguean cuerpos.
