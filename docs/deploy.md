# Deploy en Coolify v4 — tekitla-gateway

Setup **de una sola vez**. La CI (GitHub Actions → GHCR)
corre tests, govulncheck y Trivy, publica **la imagen escaneada** y dispara el
deploy por la API de Coolify.

```bash
git tag qa-v0.1.0   && git push origin qa-v0.1.0
git tag prod-v0.1.0 && git push origin prod-v0.1.0
```

```
Internet ─https─▶ Traefik (Coolify) ─▶ tekitla-gateway ──gRPC plaintext, red interna──▶ tekitla-issues
                  TLS + dominio          TRUSTED_PROXY_HOPS=1                             (sin puertos publicados)
```

> Los nombres de menús de Coolify cambian entre versiones de v4: los pasos
> dicen **qué** configurar; buscá el equivalente en tu versión.

## 1. Acceso del VPS al registry

```bash
# Token con read:packages (de una cuenta de servicio, no personal)
docker login ghcr.io -u <usuario> --password-stdin
```

## 2. Recurso (QA; repetir para prod)

1. **New resource** → repositorio git → build pack **Docker Compose**,
   archivo `/docker-compose.coolify.yml`. Sin auto-deploy por push: lo dispara
   la CI con el tag.
2. **Dominio** del servicio `gateway` (prod: `https://api.tekitla.victorrb1015.com`,
   ADR 0004; QA: `https://api-qa.tekitla.victorrb1015.com`):
   Coolify configura Traefik y el certificado. El gateway es lo único con dominio.
3. **Red**: activá *Connect to Predefined Network* en este recurso **y** en
   el de cada servicio gRPC; así el gateway los resuelve por nombre de
   contenedor. Los servicios no necesitan dominio ni puertos publicados.
4. Anotá el UUID del recurso (`COOLIFY_RESOURCE_UUID_QA` / `_PROD`).

## 3. Variables del recurso

| Variable | Ejemplo | Notas |
|---|---|---|
| `IMAGE_TAG` | `qa` / `prod` | Tag móvil. Rollback: un tag inmutable (`prod-v1.2.2`) + Redeploy |
| `UPSTREAM_ISSUES_ADDR` | `app-<UUID-del-recurso-de-tekitla-issues>:5000` | Con la red predefinida, Coolify nombra los contenedores `<servicio>-<UUID del recurso>` (el servicio de tekitla-issues es `app`). Confirmalo con `docker ps` en el VPS. No uses `app` a secas: en la red compartida puede haber más de uno |
| `CORS_ALLOWED_ORIGINS` | prod: `https://tekitla.victorrb1015.com,https://tekitla-app.web.app,https://tekitla-app.firebaseapp.com` | Orígenes exactos del front de ESE entorno, separados por coma, sin `/` final. Nunca `*`. El panel de prod se sirve desde los tres (dominio propio y los dos de Firebase Hosting). QA: el sitio de Firebase Hosting de QA (a definir, p. ej. `https://<sitio-qa>.web.app`) |
| `FIREBASE_PROJECT_ID` | `tekitla-app` | El proyecto de Firebase de ESE entorno |
| `AUTH_ALLOWED_EMAILS` | `victor@ejemplo.com` | **Obligatoria.** Emails verificados que pueden usar la API, separados por coma. Fuera de la lista → 403; vacía → nadie entra (docs/auth.md) |
| `APP_ENV` / `SERVICE_ENV` / `LOG_LEVEL` | `production` / `qa` / `info` | Opcionales (defaults de prod) |
| `OPENAPI_ENABLED` | `true` en QA | Default `false` |

`FIREBASE_JWKS_URL` **no** se define en Coolify: es solo para pruebas locales
y con `APP_ENV` distinto de `development` el gateway no arranca.

## 3b. Firebase (consola, proyecto `tekitla-app`)

- **Authentication → Sign-in method**: Google habilitado.
- **Authentication → Settings → Authorized domains**: los orígenes del
  **front** (no los de la API): `tekitla.victorrb1015.com`, el dominio del
  sitio de QA y `localhost` (viene por defecto). Los `*.web.app` /
  `*.firebaseapp.com` del proyecto ya están.
- El gateway no necesita credenciales de Firebase: solo baja las claves
  públicas de securetoken (salida HTTPS a `www.googleapis.com`).

## 4. Variables de la CI

Settings > Secrets and variables > Actions: variables `COOLIFY_URL`,
`COOLIFY_RESOURCE_UUID_QA`, `COOLIFY_RESOURCE_UUID_PROD`; secret
`COOLIFY_TOKEN`. Environments `qa`/`prod` (prod: solo tags `prod-v*` y
reviewers).
Secret `PROTOS_TOKEN` (lectura del repo de protos): hoy no hace falta,
porque `tekitla/tekitla-protos` es público; si pasa a privado, es obligatorio.

## 5. Verificar

```bash
API=https://api.tekitla.victorrb1015.com             # QA: https://api-qa.tekitla.victorrb1015.com
curl -sI $API/healthz          # 200 + cabeceras de seguridad
curl -s  $API/readyz           # {"status":"ok"} si llega a los upstreams
curl -s  $API/v1/projects      # 401 sin token (todas las rutas son privadas)
curl -s  -X OPTIONS $API/v1/projects -H 'Origin: https://tekitla-app.web.app' -H 'Access-Control-Request-Method: GET' -D - -o /dev/null
                                                      # Access-Control-Allow-Origin solo si el origen está en la lista
```

`/readyz` en `unavailable`: revisar `UPSTREAM_*_ADDR` y que los dos recursos
estén en la red predefinida (`docker network inspect coolify`).

DNS: un registro `A` (o `CNAME`) de `api.tekitla.victorrb1015.com` (y
`api-qa…` cuando exista QA) hacia la IP del VPS, **antes** de asignar el
dominio: Traefik pide el certificado de Let's Encrypt en cuanto lo ve y falla
si el nombre todavía no resuelve.

## Mudar a Cloud Run

`copier update --data deploy_target=cloud-run --data gcp_project=<id>` en una
rama: aparecen `cloudrun.*.yaml`, `cloudbuild.yaml` y `docs/ci-cd-setup.md`;
desaparecen este archivo y `docker-compose.coolify.yml`. El código no cambia:
en Cloud Run los upstreams pasan a `UPSTREAM_TLS=true` + `UPSTREAM_AUTH=google-idtoken`.
