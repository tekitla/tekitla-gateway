## Qué cambia

<!-- Una o dos líneas. El título va con el issue: feat(issue-<n>): … (chore(deps) sin issue). -->

Issue: issue-N  <!-- carpeta contenido/issues/N-…/ del repo de docs; obligatorio salvo chore(deps) -->

## Cómo se probó

- [ ] `go build ./... && go vet ./...`
- [ ] `go test -count=1 ./...` (incluye los tests de autorización generados desde `routes.yaml`)
- [ ] `make vuln` (govulncheck) y, si cambian dependencias o el Dockerfile, `make scan` (Trivy)

## Rutas y seguridad

- [ ] No toca rutas ni `routes.yaml`
- [ ] Ruta nueva o cambiada: la anotación `google.api.http` está en el repo de protos (link al MR), la entrada de `routes.yaml` declara `public`, `roles` o `deny` a propósito, y el `rate_limit`/`max_body_bytes` es el adecuado
- [ ] Una ruta pública nueva está justificada abajo
- [ ] Cambia auth, CORS, cabeceras o límites: explicado abajo

## Checklist

- [ ] Sin secretos ni datos personales en el diff (gitleaks corre en el CI igual)
- [ ] Migraciones: tienen `down` / no aplica
- [ ] Docs del repo actualizadas (README, `docs/`, CLAUDE.md) / no aplica
- [ ] Ficha o ADR en el repo de docs actualizados / no aplica

## Notas para el review

<!-- Decisiones, deuda que queda, PR relacionados en otros repos. -->
