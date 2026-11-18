package gateway_test

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

// TestAutorizacionPorRuta se GENERA desde routes.yaml + las anotaciones
// google.api.http: recorre cada ruta HTTP de los servicios registrados y
// verifica su política. Agregar una ruta a routes.yaml agrega sus casos acá
// sin escribir nada:
//
//	sin entrada          -> el test falla (declararla: public, roles o deny)
//	deny: true           -> 404 con y sin credencial
//	public: true         -> sin credencial NO es 401 (llega al servicio: 200)
//	roles: [...]         -> sin credencial 401; credencial inválida 401;
//	                        credencial válida sin rol 403; con cada rol 200
//	roles: [allowlist]   -> además 403 con email fuera de la lista, con email
//	                        permitido sin verificar y con el rol como claim
func TestAutorizacionPorRuta(t *testing.T) {
	e := newEnv(t, nil)
	if len(e.report.Bindings) == 0 {
		t.Fatal("no hay rutas HTTP: ¿falta google.api.http en los .proto o el servicio en app/upstream/registry.go?")
	}
	for _, b := range e.report.Bindings {
		pol := e.table.Policy(b.Method)
		method, path, body := request(b)
		t.Run(b.Verb+" "+b.Template, func(t *testing.T) {
			if pol == nil {
				t.Fatalf("%s no tiene entrada en routes.yaml (queda en 404). Declarala con public: true, roles: [...] o deny: true", b.Method)
			}
			// expect corre un subtest: "<caso> -> <código esperado>".
			expect := func(name string, want int, hdr ...string) bool {
				return t.Run(fmt.Sprintf("%s -> %d", name, want), func(t *testing.T) {
					w := e.do(method, path, body, hdr...)
					if w.Code != want {
						t.Errorf("%s %s -> %d, esperaba %d (body: %s)", method, path, w.Code, want, strings.TrimSpace(w.Body.String()))
					}
				})
			}
			switch {
			case pol.Deny:
				expect("deny sin credencial", http.StatusNotFound)
				if hasAuth {
					n, v := e.creds.header("u-deny", "items.admin")
					expect("deny con credencial", http.StatusNotFound, n, v)
				}
			case pol.Public:
				expect("publica sin credencial", http.StatusOK)
				if hasAuth {
					n, v := e.creds.invalid()
					expect("publica con credencial invalida (se ignora)", http.StatusOK, n, v)
				}
			default:
				expect("sin credencial", http.StatusUnauthorized)
				n, v := e.creds.invalid()
				expect("credencial invalida", http.StatusUnauthorized, n, v)
				n, v = e.creds.header("u-sin-rol")
				expect("credencial valida sin rol", http.StatusForbidden, n, v)
				if slices.Contains(pol.Roles, auth.AllowlistRole) {
					n, v = e.creds.withEmail("u-ajeno", "otra@tekitla.test", true)
					expect("email fuera de la allowlist", http.StatusForbidden, n, v)
					n, v = e.creds.withEmail("u-sin-verificar", testAllowedEmail, false)
					expect("email permitido sin verificar", http.StatusForbidden, n, v)
					n, v = e.creds.withEmail("u-claim", "", false, auth.AllowlistRole)
					expect("rol allowlist como claim del token", http.StatusForbidden, n, v)
				}
				for _, role := range pol.Roles {
					n, v = e.creds.header("u-"+role, role)
					if !expect("con rol "+role, http.StatusOK, n, v) {
						continue
					}
					gotMethod, md, _ := e.up.last()
					if gotMethod != "/"+b.Method {
						t.Errorf("con rol %s: el upstream recibió %s, esperaba /%s", role, gotMethod, b.Method)
					}
					if len(md.Get("x-user-id")) != 1 || md.Get("x-user-id")[0] == "" {
						t.Errorf("con rol %s: x-user-id no llegó al servicio (md=%v)", role, md)
					}
					if !strings.Contains(strings.Join(md.Get("x-roles"), ","), role) {
						t.Errorf("con rol %s: x-roles = %v", role, md.Get("x-roles"))
					}
				}
			}
		})
	}
}

// TestRutasDeLaTablaExisten: toda entrada de routes.yaml corresponde a un
// método con google.api.http (si no, gateway.New falla: lo cubre newEnv) y
// ningún método anotado queda sin declarar.
func TestRutasDeLaTablaExisten(t *testing.T) {
	e := newEnv(t, nil)
	if len(e.report.Undeclared) > 0 {
		t.Fatalf("métodos con google.api.http sin entrada en routes.yaml (quedan en 404): %v", e.report.Undeclared)
	}
}

// TestRutasDeTekitlaExigenAllowlist (rebanadas 1 y 3a, issues 011, 013, 025
// y 009): las rutas del contrato están expuestas y ninguna se abre sin el rol
// de la allowlist, salvo las tres de lectura por token del 009 (públicas, con
// bucket propio más estricto que las lecturas autenticadas). Las de
// IssueService aceptan además el scope de token que corresponde (lecturas
// issues:read, escrituras issues:write); las de ActorService y
// ShareLinkService, solo la allowlist. ImportIssue no se expone. Las
// escrituras tienen un rate limit más estricto que las lecturas.
func TestRutasDeTekitlaExigenAllowlist(t *testing.T) {
	e := newEnv(t, nil)
	want := []string{
		"GET /v1/projects", "POST /v1/projects",
		"GET /v1/projects/{project_id}/issues", "POST /v1/projects/{project_id}/issues",
		"GET /v1/projects/{project_id}/issues/{number}", "POST /v1/projects/{project_id}/issues/{number}:move",
		"POST /v1/projects/{project_id}/issues/{number}:discard", "POST /v1/projects/{project_id}/issues/{number}:restore",
		"PATCH /v1/projects/{project_id}/issues/{number}",
		"POST /v1/projects/{project_id}/issues/{number}/comments",
		"GET /v1/projects/{project_id}/issues/{number}/events",
		// Issue 036: métricas de flujo.
		"GET /v1/projects/{project_id}/flowMetrics",
		"GET /v1/projects/{project_id}/statuses", "POST /v1/projects/{project_id}/statuses",
		"PATCH /v1/projects/{project_id}/statuses/{status_id}", "DELETE /v1/projects/{project_id}/statuses/{status_id}",
		"POST /v1/projects/{project_id}/statuses:batchUpdate",
		"GET /v1/projects/{project_id}/labels", "POST /v1/projects/{project_id}/labels",
		"PATCH /v1/projects/{project_id}/labels/{label_id}", "DELETE /v1/projects/{project_id}/labels/{label_id}",
		// Issue 025: agentes y tokens.
		"GET /v1/actors", "POST /v1/agents", "POST /v1/actors/{actor_id}:revoke",
		"GET /v1/tokens", "POST /v1/tokens", "POST /v1/tokens/{token_id}:revoke",
		// Issue 009: gestión de enlaces de solo lectura.
		"GET /v1/projects/{project_id}/share-links", "POST /v1/projects/{project_id}/share-links",
		"POST /v1/projects/{project_id}/share-links/{share_link_id}:revoke",
	}
	// Issue 009: lo único público (lectura por el token de un enlace).
	wantPublic := []string{"POST /v1/shared/issues:get", "POST /v1/shared/issues:list", "POST /v1/shared/project:get"}
	listIssues := e.table.Policy("issues.v1.IssueService/ListIssues")
	denied := []string{"issues.v1.IssueService/ImportIssue", "issues.v1.IssueService/ImportIssueEvents"}
	var got, gotDenied, gotPublic []string
	for _, b := range e.report.Bindings {
		pol := e.table.Policy(b.Method)
		if pol != nil && pol.Deny {
			gotDenied = append(gotDenied, b.Method)
			continue
		}
		if pol != nil && pol.Public {
			gotPublic = append(gotPublic, b.Verb+" "+b.Template)
			// CU-009-5: bucket propio, más estricto que el global y que el de
			// ListIssues, con body chico.
			if !strings.HasPrefix(b.Method, "issues.v1.SharedProjectService/") || !pol.OwnRateLimit ||
				pol.RateLimit.PerMinute >= listIssues.RateLimit.PerMinute || pol.RateLimit.Burst >= listIssues.RateLimit.Burst ||
				pol.MaxBodyBytes > 1024 {
				t.Errorf("%s: pública con política %+v; esperaba solo SharedProjectService, con rate limit propio menor que %+v y body de 1 KiB", b.Method, pol, listIssues.RateLimit)
			}
			continue
		}
		got = append(got, b.Verb+" "+b.Template)
		wantRoles := []string{auth.AllowlistRole}
		switch {
		case strings.HasPrefix(b.Method, "issues.v1.ActorService/"), strings.HasPrefix(b.Method, "issues.v1.ShareLinkService/"):
		case b.Verb == http.MethodGet:
			wantRoles = append(wantRoles, auth.ScopeIssuesRead)
		default:
			wantRoles = append(wantRoles, auth.ScopeIssuesWrite)
		}
		if pol == nil || pol.Public || !slices.Equal(pol.Roles, wantRoles) {
			t.Errorf("%s: política %+v, esperaba roles: %v", b.Method, pol, wantRoles)
			continue
		}
		// Escrituras: bucket propio y más estricto que el de las lecturas.
		if b.Verb != http.MethodGet && (!pol.OwnRateLimit || pol.RateLimit.PerMinute >= e.table.Defaults.RateLimit.PerMinute) {
			t.Errorf("%s: rate limit %+v, esperaba uno propio menor que el default %+v", b.Method, pol.RateLimit, e.table.Defaults.RateLimit)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("rutas expuestas = %v, esperaba %v", got, want)
	}
	slices.Sort(gotPublic)
	if !slices.Equal(gotPublic, wantPublic) {
		t.Fatalf("rutas públicas = %v, esperaba %v", gotPublic, wantPublic)
	}
	if !slices.Equal(gotDenied, denied) {
		t.Fatalf("rutas cerradas = %v, esperaba %v", gotDenied, denied)
	}
}
