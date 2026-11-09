# Autenticación — tekitla-gateway (auth: `firebase`)

El gateway **verifica** credenciales; no las emite ni las renueva (eso es del
proveedor o de un servicio de auth propio). Con una credencial válida arma la
identidad y se la pasa a los servicios como metadata gRPC:

| Metadata | Valor |
|---|---|
| `x-user-id` | `sub` del token, `apikey:<id>`, o el uid de la persona dueña de un token `tka_` |
| `x-agent-id` | id del agente, solo con un token `tka_` de agente (issue 025) |
| `x-roles` | roles del token o scopes de la key / del token `tka_`, separados por coma |
| `x-auth-type` | `jwt` \| `apikey` \| `token` |
| `x-request-id` | id del request (también en la respuesta, `X-Request-Id`) |
| `x-client-ip` | IP del cliente según `TRUSTED_PROXY_HOPS` |

Los servicios **solo** reciben eso: ninguna cabecera del cliente pasa tal cual
(tampoco `Authorization`), y si el cliente manda `X-User-Id`, `X-Agent-Id`,
`X-Roles` o `Grpc-Metadata-*`, el gateway las borra antes de armar la
metadata. Los
servicios confían en esta metadata **porque solo el gateway puede llamarlos**
(red interna de Docker, sin puertos publicados).

Respuestas: sin credencial o credencial inválida en una ruta protegida → `401`;
credencial válida sin el rol (en Tekitla: email fuera de la allowlist) → `403`; proveedor de claves caído → `503`. En
rutas `public: true` una credencial válida se usa (identidad y rate limit por
usuario) y una inválida se ignora.

## Firebase Auth

| Qué | Valor |
|---|---|
| `iss` | `https://securetoken.google.com/<FIREBASE_PROJECT_ID>` |
| `aud` | `<FIREBASE_PROJECT_ID>` |
| Algoritmo | RS256 (lista cerrada: RS256/ES256; nunca `none` ni HS*) |
| Claves | `https://www.googleapis.com/service_accounts/v1/jwk/securetoken@system.gserviceaccount.com` (JWKS en caché según `Cache-Control`; refresco ante `kid` desconocido, máx. uno cada 30 s) |
| Además | `exp`, `nbf`, `iat` con tolerancia `AUTH_CLOCK_SKEW` (≤ 60 s), `sub` no vacío, `auth_time` en el pasado |

Variables: `FIREBASE_PROJECT_ID` (default `tekitla-app`; QA y prod
suelen ser proyectos distintos: cada entorno pone el suyo) y
`AUTH_ROLES_CLAIM` (default `roles`).

## Allowlist de emails (Tekitla, rebanada 1)

Un solo dueño, sin roles propios: **todas** las rutas de `routes.yaml` piden
`roles: [allowlist]`. Ese rol no lo pone Firebase: lo agrega
`auth.EmailAllowlist` (`app/auth/allowlist.go`), que envuelve al verificador,
solo si el ID token (ya validado) trae:

- `email_verified: true` (booleano), y
- `email` en `AUTH_ALLOWED_EMAILS` (separados por coma, sin distinguir
  mayúsculas ni espacios).

| Caso | Resultado |
|---|---|
| Sin token / token inválido o vencido | `401` |
| Token válido, email en la lista y verificado | pasa (`x-roles` incluye `allowlist`) |
| Email fuera de la lista, sin verificar o sin claim `email` | `403` |
| Un custom claim `roles: [allowlist]` | se **descarta**: el rol solo sale de la lista |
| `AUTH_ALLOWED_EMAILS` vacía | `403` para todos (fail closed) + warning al arrancar |
| Entrada mal formada (sin `@`, con espacios...) | el gateway no arranca |

Con login de Google el email viene verificado. El email no viaja a los
servicios (solo `x-user-id` = `uid` de Firebase) ni se escribe en los logs.
Para sumar a alguien: agregar su email a la variable y redeploy. Cuando haya
más de un usuario con permisos distintos, pasar a roles por custom claims
(abajo) y dejar la allowlist como filtro de acceso o retirarla.

## Tokens de Tekitla (`tka_`, issue 025)

Decisión en el ADR 0009 del repo docs. Los emite `tekitla-issues`
(`ActorService.CreateToken`, solo con sesión de Firebase) a nombre de una
persona o de uno de sus agentes; el servicio guarda solo el SHA-256. El
cliente los manda como `Authorization: Bearer tka_…` (un header fijo, como
piden los clientes MCP).

- **Elección del verificador** (`auth.TokenOrJWT`): un bearer que empieza con
  `tka_` va a `auth.TekitlaTokens`; cualquier otro, a Firebase + allowlist.
- **Validación:** largo exacto (`tka_` + 43) antes de consultar nada; después
  `issues.v1.TokenAuthService/ResolveToken` (interno, sin ruta REST) por la
  conexión a `UPSTREAM_ISSUES_ADDR`, como llamada interna (sin metadata del
  cliente) y con 3 s de timeout.
- **Caché** (`app/auth/tekitla_token.go`): por SHA-256 del token, **60 s
  como máximo** y nunca más allá de su vencimiento. Es la demora de una
  revocación. Los rechazos también se guardan 60 s (un token inexistente no
  puede pasar a existir), así un intento repetido no consulta al servicio.
  Válidos y rechazados van en cachés separadas de hasta 10 000 entradas cada
  una (una lluvia de tokens falsos no desaloja a los buenos), y varios
  requests con el mismo token sin caché hacen una sola consulta.
- **Tokens desconocidos:** la autenticación corre antes del rate limit de la
  ruta, así que las consultas por tokens que no están en caché tienen su
  propio límite: 30/min por IP (ráfaga 10). Pasado, `401` sin consultar.
- **Errores:** `UNAUTHENTICATED` con el `ErrorInfo` de `tekitla-issues`
  (domain `issues-service`) → `401`; servicio caído, un `UNAUTHENTICATED`
  de otra capa (proxy, auth del upstream) o cualquier otra falla → `503`
  (no se guarda: el próximo request reintenta). Una resolución ya vencida se
  rechaza.
- **Identidad:** `x-user-id` = la persona dueña, `x-agent-id` = el agente
  (si es de un agente), `x-roles` = los scopes (`issues:read`,
  `issues:write`) y `x-auth-type: token`. Un token nunca recibe `allowlist`.
- **Los scopes solo salen de un token:** si un ID token de Firebase trae
  `issues:read` o `issues:write` como custom claim, se descartan.
- **Rate limit:** con un token (de persona o de agente) el bucket es el de
  la persona dueña (`sub:<uid>`): crear agentes no multiplica la cuota.
- **Request id:** `ResolveToken` va sin identidad pero con `x-request-id`,
  para cruzar el log del servicio con el del gateway.
- **Auditoría:** el access log de cada request con token lleva `user_id`,
  `agent_id`, `token_id`, `auth`, `route` y `status`. Nunca el token ni su
  hash.
- **Sacar a alguien de la allowlist no revoca sus tokens** (el gateway no
  conoce el email del dueño de un token): hay que revocar también su actor
  (runbook de la allowlist en el repo docs).

### Revisión contra el OWASP API Top 10 (2023)

| Riesgo | Cómo se cubre |
|---|---|
| API1 Autorización por objeto | La sigue haciendo `tekitla-issues` con `x-user-id` (= la persona, también con un token de agente): un proyecto, actor o token ajeno es `NOT_FOUND`. |
| API2 Autenticación rota | Tokens de 256 bits aleatorios guardados como SHA-256, vencimiento obligatorio (≤ 90 días), revocación en cascada, sin el token en logs ni respuestas; formato validado antes de consultar; `401` sin detalle. |
| API3 Autorización por propiedad | Las respuestas de tokens nunca traen el secreto ni el hash (solo `CreateToken` devuelve el secreto, una vez). `x-agent-id` lo pone solo el gateway. |
| API4 Consumo de recursos | Caché positiva y negativa de 60 s (acotadas y separadas), una consulta por token a la vez, 30 consultas/min por IP para tokens desconocidos, timeout de 3 s a `ResolveToken`, rate limit por persona (no por agente) y buckets estrictos en `CreateToken`/`CreateAgent` (10/min). |
| API5 Autorización por función | `routes.yaml`: lecturas con `issues:read`, escrituras con `issues:write` (write no implica read); agentes y tokens solo con `allowlist` (un token no crea agentes ni tokens; el servicio también lo rechaza). `ImportIssue` con `deny: true`. |
| API6 Flujos de negocio sensibles | Emitir tokens pide sesión de Firebase de la persona; rate limit propio. |
| API7 SSRF | No aplica: el gateway no hace requests a URLs del cliente. |
| API8 Configuración | Denegar por defecto; sin variables nuevas; los scopes de token no se pueden obtener por claims. |
| API9 Inventario | Las rutas nuevas salen de las anotaciones del `.proto` y `TestRutasDeTekitlaExigenAllowlist` fija la lista; `ResolveToken` no tiene ruta REST. |
| API10 Consumo inseguro de APIs | La respuesta de `ResolveToken` se filtra: solo scopes conocidos, persona obligatoria. |

## Pruebas locales sin tokens reales (`FIREBASE_JWKS_URL`)

Firebase no emite ID tokens para pruebas automatizadas (y el emulador de Auth
los firma con `alg: none`, que el gateway rechaza siempre). Para un e2e local:
un emisor de prueba firma tokens RS256 con `iss`
`https://securetoken.google.com/<FIREBASE_PROJECT_ID>`, `aud`
`<FIREBASE_PROJECT_ID>`, `auth_time`, `email` y `email_verified`, y publica su
JWKS; el gateway se levanta con `FIREBASE_JWKS_URL=<url del JWKS>`. Todas las
demás validaciones son las mismas. Solo se admite con `APP_ENV=development`
(con cualquier otro valor el gateway no arranca) y loguea un warning. Nunca
en QA ni prod: `docker-compose.coolify.yml` ni siquiera la pasa.

Los tests de Go usan lo mismo (`app/auth/authtest`).

## Roles por custom claims (no usados en la rebanada 1)

**Roles** = custom claims. Se setean desde un backend con el Admin SDK (nunca
desde el cliente), y el usuario los ve en el próximo ID token (≤ 1 h, o
forzando `getIdToken(true)`):

```js
await admin.auth().setCustomUserClaims(uid, { roles: ['items.read', 'items.write'] });
```

El front manda el ID token (`await user.getIdToken()`) en
`Authorization: Bearer <token>`.

**Revocación**: el gateway no llama a `checkRevoked` en cada request (cuesta
una llamada extra). Para operaciones sensibles, que el servicio de auth la
haga o acortá la vida de las sesiones. (docs/05 §2.)

## Cambiar de proveedor

`copier update --data auth=<firebase|oidc|api-key|ninguno>` regenera el
wiring (`cli/main.go`), los tests y la documentación. El código de todos los
verificadores vive siempre en `app/auth` (y tiene tests), así que el cambio no
toca la lógica.
