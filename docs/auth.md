# Autenticación — tequio-gateway (auth: `firebase`)

El gateway **verifica** credenciales; no las emite ni las renueva (eso es del
proveedor o de un servicio de auth propio). Con una credencial válida arma la
identidad y se la pasa a los servicios como metadata gRPC:

| Metadata | Valor |
|---|---|
| `x-user-id` | `sub` del token, o `apikey:<id>` |
| `x-roles` | roles del token o scopes de la key, separados por coma |
| `x-auth-type` | `jwt` \| `apikey` |
| `x-request-id` | id del request (también en la respuesta, `X-Request-Id`) |
| `x-client-ip` | IP del cliente según `TRUSTED_PROXY_HOPS` |

Los servicios **solo** reciben eso: ninguna cabecera del cliente pasa tal cual
(tampoco `Authorization`), y si el cliente manda `X-User-Id`, `X-Roles` o
`Grpc-Metadata-*`, el gateway las borra antes de armar la metadata. Los
servicios confían en esta metadata **porque solo el gateway puede llamarlos**
(red interna de Docker, sin puertos publicados).

Respuestas: sin credencial o credencial inválida en una ruta protegida → `401`;
credencial válida sin el rol → `403`; proveedor de claves caído → `503`. En
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

Variables: `FIREBASE_PROJECT_ID` (default `tequio-app-435b4`; QA y prod
suelen ser proyectos distintos: cada entorno pone el suyo) y
`AUTH_ROLES_CLAIM` (default `roles`).

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
