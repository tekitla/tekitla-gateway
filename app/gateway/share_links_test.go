package gateway_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

// Enlaces de solo lectura (issue 009) en el pipeline completo, con el
// routes.yaml real.

// shareToken tiene la forma de un token de enlace (inventado).
const shareToken = "tks_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// La lectura pública llega al servicio sin credenciales y sin identidad.
func TestEnlace_LecturaPublicaSinCredencial(t *testing.T) {
	e := newEnv(t, nil)
	body := `{"token":"` + shareToken + `"}`
	for _, path := range []string{"/v1/shared/project:get", "/v1/shared/issues:list", "/v1/shared/issues:get"} {
		w := e.do(http.MethodPost, path, body)
		if w.Code != http.StatusOK {
			t.Fatalf("POST %s sin credencial = %d (%s)", path, w.Code, w.Body.String())
		}
		_, md, _ := e.up.last()
		for _, k := range []string{"x-user-id", "x-agent-id", "x-roles"} {
			if v := md.Get(k); len(v) != 0 {
				t.Errorf("POST %s: %s = %v, esperaba vacío", path, k, v)
			}
		}
	}
	// El token llega al servicio en el cuerpo.
	reqs := e.up.requests()
	if last := reqs[len(reqs)-1]; !strings.Contains(string(last.raw), shareToken) {
		t.Errorf("el token no llegó al servicio: %q", last.raw)
	}
	// Una lectura pública no acepta GET (no hay ruta: 404 o 405, nunca 200).
	if w := e.do(http.MethodGet, "/v1/shared/project:get", ""); w.Code == http.StatusOK {
		t.Errorf("GET /v1/shared/project:get = 200, esperaba que no exista")
	}
}

// CU-009-2: un 404 del servicio sale como 404 genérico, sin el detalle.
func TestEnlace_NoEncontradoEs404(t *testing.T) {
	e := newEnv(t, nil)
	e.up.set(status.Error(codes.NotFound, "share link not found"), 0)
	w := e.do(http.MethodPost, "/v1/shared/project:get", `{"token":"`+shareToken+`"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("enlace inexistente = %d, esperaba 404", w.Code)
	}
	if strings.Contains(w.Body.String(), shareToken) {
		t.Errorf("la respuesta repite el token: %s", w.Body.String())
	}
}

// CU-009-3: el token de un enlace nunca es una credencial. Como Bearer, en
// cualquier ruta no pública, 401.
func TestEnlace_TokenNoAutoriza(t *testing.T) {
	e := newEnv(t, nil)
	bearer := "Bearer " + shareToken
	for _, r := range []struct{ method, path, body string }{
		{http.MethodPost, "/v1/projects/p1/issues", "{}"},
		{http.MethodPost, "/v1/projects/p1/issues/1:move", "{}"},
		{http.MethodPatch, "/v1/projects/p1/issues/1", "{}"},
		{http.MethodPost, "/v1/projects/p1/share-links", "{}"},
		{http.MethodPost, "/v1/projects/p1/share-links/s1:revoke", "{}"},
		{http.MethodGet, "/v1/projects/p1/share-links", ""},
		{http.MethodGet, "/v1/projects/p1/issues", ""},
		{http.MethodGet, "/v1/projects", ""},
	} {
		if w := e.do(r.method, r.path, r.body, "Authorization", bearer); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s con Bearer tks_ = %d, esperaba 401", r.method, r.path, w.Code)
		}
	}
	if n := e.creds.tokens.count(); n != 0 {
		t.Errorf("un tks_ se mandó a resolver como token de API (%d veces)", n)
	}
}

// La gestión pide la sesión del panel: un token tka_ (aunque tenga todos los
// scopes) no crea, lista ni revoca enlaces.
func TestEnlace_GestionSoloConSesion(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.creds.tokens.add("persona-1", "", auth.ScopeIssuesRead, auth.ScopeIssuesWrite)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/projects/p1/share-links", ""},
		{http.MethodPost, "/v1/projects/p1/share-links", "{}"},
		{http.MethodPost, "/v1/projects/p1/share-links/s1:revoke", "{}"},
	} {
		if w := e.do(r.method, r.path, r.body, "Authorization", "Bearer "+tok); w.Code != http.StatusForbidden {
			t.Errorf("%s %s con tka_ = %d, esperaba 403", r.method, r.path, w.Code)
		}
	}
	n, v := e.creds.header("duena", auth.AllowlistRole)
	if w := e.do(http.MethodGet, "/v1/projects/p1/share-links", "", n, v); w.Code != http.StatusOK {
		t.Errorf("GET share-links con la sesión = %d, esperaba 200", w.Code)
	}
}

// CU-009-5: la ruta pública se corta por IP antes que las lecturas
// autenticadas.
func TestEnlace_RateLimitPropioYMasEstricto(t *testing.T) {
	e := newEnv(t, nil)
	body := `{"token":"` + shareToken + `"}`
	pol := e.table.Policy("issues.v1.SharedProjectService/GetSharedProject")
	limited := 0
	for i := 0; i < pol.RateLimit.Burst+1; i++ {
		w := e.do(http.MethodPost, "/v1/shared/project:get", body)
		if w.Code == http.StatusTooManyRequests {
			limited = i
			if w.Header().Get("Retry-After") == "" {
				t.Error("429 sin Retry-After")
			}
			break
		}
	}
	if limited != pol.RateLimit.Burst {
		t.Fatalf("el 429 llegó en el request %d, esperaba en el %d", limited+1, pol.RateLimit.Burst+1)
	}
	// Con la misma cantidad, una lectura autenticada sigue pasando.
	n, v := e.creds.header("duena", auth.AllowlistRole)
	for i := 0; i < pol.RateLimit.Burst+1; i++ {
		if w := e.do(http.MethodGet, "/v1/projects", "", n, v); w.Code != http.StatusOK {
			t.Fatalf("GET /v1/projects %d con sesión = %d", i+1, w.Code)
		}
	}
	// Otra ruta pública tiene su propio bucket.
	if w := e.do(http.MethodPost, "/v1/shared/issues:list", body); w.Code != http.StatusOK {
		t.Errorf("ListSharedIssues después del 429 de GetSharedProject = %d", w.Code)
	}
}

// El token nunca queda en los logs del gateway (va en el cuerpo).
func TestEnlace_TokenFueraDeLosLogs(t *testing.T) {
	e := newEnv(t, nil)
	e.do(http.MethodPost, "/v1/shared/project:get", `{"token":"`+shareToken+`"}`)
	e.up.set(status.Error(codes.NotFound, "share link not found"), 0)
	e.do(http.MethodPost, "/v1/shared/issues:list", `{"token":"`+shareToken+`"}`)
	if logs := e.logs.String(); strings.Contains(logs, shareToken) || strings.Contains(logs, "tks_") {
		t.Errorf("el token quedó en los logs:\n%s", logs)
	}
}

// CU-009-4: el JSON público lleva solo los campos de los mensajes Shared*
// (el gateway no agrega nada), sin emails ni uids.
func TestEnlace_JSONSinDatosPrivados(t *testing.T) {
	e := newEnv(t, nil)
	e.up.respond = func(method string, _ []byte) (proto.Message, error) {
		if method != "/issues.v1.SharedProjectService/ListSharedIssues" {
			return nil, nil
		}
		return &issuesv1.ListSharedIssuesResponse{
			Issues: []*issuesv1.SharedIssue{{
				Number: 7, Title: "Revisar el aviso", WaitingNote: "que apruebe el texto",
				State: issuesv1.IssueState_ISSUE_STATE_TODO, Priority: issuesv1.Priority_PRIORITY_HIGH,
				StatusId: "st-1", LabelIds: []string{"lb-1"},
				Assignee:  &issuesv1.SharedActor{Kind: issuesv1.ActorKind_ACTOR_KIND_PERSON, DisplayName: "Victor"},
				CreatedAt: timestamppb.Now(), UpdatedAt: timestamppb.Now(),
			}},
			Page: &issuesv1.PageResponse{},
		}, nil
	}
	w := e.do(http.MethodPost, "/v1/shared/issues:list", `{"token":"`+shareToken+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("ListSharedIssues = %d (%s)", w.Code, w.Body.String())
	}
	var got struct {
		Issues []map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Issues) != 1 {
		t.Fatalf("JSON: %v %s", err, w.Body.String())
	}
	allowed := []string{"number", "title", "description", "waitingNote", "state", "priority", "statusId", "labelIds", "assignee", "createdAt", "updatedAt"}
	for k := range got.Issues[0] {
		if !slices.Contains(allowed, k) {
			t.Errorf("campo inesperado en el JSON público: %q", k)
		}
	}
	for _, bad := range []string{"@", "userId", "actorId", "projectId", "workspaceId", "authorId", "\"id\""} {
		if strings.Contains(w.Body.String(), bad) {
			t.Errorf("el JSON público lleva %q: %s", bad, w.Body.String())
		}
	}
}
