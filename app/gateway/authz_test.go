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

// TestRutasDeTekitlaExigenAllowlist (rebanadas 1 y 3a): las 17 rutas del
// contrato están expuestas, ninguna es pública ni se abre sin el rol de la
// allowlist, y las escrituras tienen un rate limit más estricto que las
// lecturas.
func TestRutasDeTekitlaExigenAllowlist(t *testing.T) {
	e := newEnv(t, nil)
	want := []string{
		"GET /v1/projects", "POST /v1/projects",
		"GET /v1/projects/{project_id}/issues", "POST /v1/projects/{project_id}/issues",
		"GET /v1/projects/{project_id}/issues/{number}", "POST /v1/projects/{project_id}/issues/{number}:move",
		"PATCH /v1/projects/{project_id}/issues/{number}",
		"POST /v1/projects/{project_id}/issues/{number}/comments",
		"GET /v1/projects/{project_id}/issues/{number}/events",
		"GET /v1/projects/{project_id}/statuses", "POST /v1/projects/{project_id}/statuses",
		"PATCH /v1/projects/{project_id}/statuses/{status_id}", "DELETE /v1/projects/{project_id}/statuses/{status_id}",
		"GET /v1/projects/{project_id}/labels", "POST /v1/projects/{project_id}/labels",
		"PATCH /v1/projects/{project_id}/labels/{label_id}", "DELETE /v1/projects/{project_id}/labels/{label_id}",
	}
	var got []string
	for _, b := range e.report.Bindings {
		got = append(got, b.Verb+" "+b.Template)
		pol := e.table.Policy(b.Method)
		if pol == nil || pol.Public || pol.Deny || !slices.Equal(pol.Roles, []string{auth.AllowlistRole}) {
			t.Errorf("%s: política %+v, esperaba roles: [%s]", b.Method, pol, auth.AllowlistRole)
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
}
