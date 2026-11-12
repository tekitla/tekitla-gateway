# tekitla-gateway

Entrada pública de Tekitla: autentica (Firebase o token `tka_` de agente),
autoriza por ruta (`routes.yaml`) y traduce REST a gRPC hacia
`tekitla-issues`. También sirve `/mcp` para agentes.

- Ambientes: `https://api-qa.tekitla.victorrb1015.com` (QA) y
  `https://api.tekitla.victorrb1015.com` (prod).
- Contexto para agentes y reglas de seguridad: [`AGENTS.md`](AGENTS.md).
- Rutas, errores y pipeline: [`docs/rutas.md`](docs/rutas.md); auth:
  [`docs/auth.md`](docs/auth.md); deploy: [`docs/deploy.md`](docs/deploy.md).

```bash
cp .env.example .env.dev && make run   # local
make lint test vuln                    # lo mismo que el CI
```

## MCP

`POST /mcp` (transporte HTTP del protocolo, sin sesión) expone el tracker a
agentes (issue 030, fase 1). **Solo lectura**: ninguna herramienta escribe.

| Herramienta | Qué hace |
|---|---|
| `list_projects` | Proyectos visibles: clave, nombre y conteo de issues por categoría |
| `list_issues` | Issues de un proyecto (`project: "RND"`) con filtros `category` (`todo`, `in_progress`, `done`, `canceled`), `status` (nombre de la columna), `priority`, `assignee` (id del actor o `"none"`) y `query`; 20 por página, `next_cursor` para seguir |
| `get_issue` | Un issue por clave (`RND-236`): descripción, estado, prioridad, etiquetas, asignado y últimos eventos (`events`, 10 por defecto) |
| `next_issue` | El siguiente issue libre de un proyecto: «por hacer», sin asignar, de mayor prioridad y después por rank. **No lo toma** |

**Credencial:** un token de agente `tka_` con `issues:read` (o la sesión de
Firebase de la dueña). Se saca con la CLI (`tekitla-cli` v0.2):

```bash
tk agente nuevo "Claude Code PC-01" --herramienta claude-code
tk token nuevo --agente "Claude Code PC-01" --permiso issues:read --vence 30d
```

El token se muestra una sola vez. Guardalo como **variable de entorno de
usuario** `TRACKER_TOKEN` (en Windows: *Variables de entorno › Variables de
usuario*, o `[Environment]::SetEnvironmentVariable('TRACKER_TOKEN', '<token>', 'User')`
y abrir una terminal nueva). Nunca en un archivo del repo.

Sin token, o con uno vencido o revocado, `/mcp` responde 401. Con un token
sin `issues:read`, cada herramienta devuelve un error `PERMISSION_DENIED` con
un `hint` de qué hacer.

### Claude Code (alcance de usuario)

```bash
claude mcp add --transport http tracker https://api.tekitla.victorrb1015.com/mcp \
  --header 'Authorization: Bearer ${TRACKER_TOKEN}' -s user
```

Con **comillas simples** (bash y PowerShell): así queda `${TRACKER_TOKEN}`
literal en `~/.claude.json` y Claude Code lo expande del entorno al arrancar.
Con comillas dobles la shell escribe el token en el archivo. Para QA, la misma
URL con `api-qa` (p. ej. un segundo servidor `tracker-qa`).

#### 401 «Server rejected the configured Authorization header»

Si `/mcp` responde 401 con el header configurado, lo más probable es que
`TRACKER_TOKEN` no exista **en el proceso de Claude Code**: entonces manda
`Authorization: Bearer` sin token. Revisalo en la terminal desde la que abrís
`claude`:

```powershell
if ($env:TRACKER_TOKEN) { 'TRACKER_TOKEN cargada' } else { 'TRACKER_TOKEN ausente' }
```

(sin imprimir el valor). Una variable de usuario nueva solo llega a los
procesos que arrancan después de crearla: en Windows con **herdr**, o con una
terminal abierta antes, no la ven ni herdr ni sus pestañas. Dos salidas:

- Reiniciar herdr (o abrir una terminal nueva).
- Cargarla en la pestaña actual antes de abrir `claude`:

  ```powershell
  $env:TRACKER_TOKEN = [Environment]::GetEnvironmentVariable('TRACKER_TOKEN','User')
  ```

Si la variable está y sigue el 401, el token venció o lo revocaron
(`tk tokens`, columna ESTADO): creá otro con `tk token nuevo`, en el mismo
ambiente que la URL del servidor (`--ambiente qa` para `api-qa`).

### Repos generados con dev-plantillas (`.mcp.json`)

La base común (`tracker_mcp: true` en Copier) trae el servidor `tracker` en el
`.mcp.json` del repo y los permisos de lectura en `.claude/settings.json`:

```json
{
  "mcpServers": {
    "tracker": {
      "type": "http",
      "url": "${TRACKER_MCP_URL}",
      "headers": { "Authorization": "Bearer ${TRACKER_TOKEN}" }
    }
  }
}
```

Cada persona define en su entorno de usuario `TRACKER_MCP_URL`
(`https://api.tekitla.victorrb1015.com/mcp`, o `api-qa` para QA) y
`TRACKER_TOKEN`.

### Codex

En `~/.codex/config.toml`:

```toml
[mcp_servers.tracker]
url = "https://api.tekitla.victorrb1015.com/mcp"
bearer_token_env_var = "TRACKER_TOKEN"
```

### Cómo está hecho

Las cuatro son herramientas propias del repo (`app/mcptools`, gancho en
`app/mcpserver/custom.go`): toman la clave del proyecto en vez de su id,
componen varias lecturas y `next_issue` fija los filtros de `ListIssues`.
Cada RPC que llaman vuelve a entrar al mux como request REST con la credencial
del cliente, así que la política es la de su ruta en `routes.yaml`
(`allowlist` o `issues:read`, rate limit, deadline). Detalle en
[`docs/rutas.md`](docs/rutas.md#herramientas-mcp-mcp).

Lo que devuelven es dato, no instrucciones: el contenido de un issue lo
escribe cualquiera con acceso al proyecto.
