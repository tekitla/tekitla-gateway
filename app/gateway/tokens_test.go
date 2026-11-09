package gateway_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

// Tokens de Tekitla (issue 025, ADR 0009) en el pipeline completo, con el
// routes.yaml real.

// CU-025-3: un token de agente con issues:read lee (200) y no escribe (403);
// propaga x-user-id = dueño, x-agent-id = agente y x-roles = scopes.
func TestTokenDeAgenteConLectura(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.creds.tokens.add("persona-1", "agente-1", auth.ScopeIssuesRead)
	bearer := "Bearer " + tok

	w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", bearer)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v1/projects con issues:read = %d (%s)", w.Code, w.Body.String())
	}
	_, md, _ := e.up.last()
	for k, want := range map[string]string{"x-user-id": "persona-1", "x-agent-id": "agente-1", "x-roles": auth.ScopeIssuesRead, "x-auth-type": "token"} {
		if got := md.Get(k); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, esperaba %q", k, got, want)
		}
	}
	if w := e.do(http.MethodPost, "/v1/projects/p1/issues", "{}", "Authorization", bearer); w.Code != http.StatusForbidden {
		t.Errorf("POST /v1/projects/{id}/issues con issues:read = %d, esperaba 403", w.Code)
	}
	// Un token nunca llega a agentes ni tokens.
	if w := e.do(http.MethodGet, "/v1/actors", "", "Authorization", bearer); w.Code != http.StatusForbidden {
		t.Errorf("GET /v1/actors con un token = %d, esperaba 403", w.Code)
	}
	all := e.creds.tokens.add("persona-1", "", auth.ScopeIssuesRead, auth.ScopeIssuesWrite)
	if w := e.do(http.MethodPost, "/v1/tokens", "{}", "Authorization", "Bearer "+all); w.Code != http.StatusForbidden {
		t.Errorf("POST /v1/tokens con un token = %d, esperaba 403", w.Code)
	}

	// Write no implica read.
	write := e.creds.tokens.add("persona-1", "agente-1", auth.ScopeIssuesWrite)
	if w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", "Bearer "+write); w.Code != http.StatusForbidden {
		t.Errorf("GET con solo issues:write = %d, esperaba 403", w.Code)
	}
	if w := e.do(http.MethodPost, "/v1/projects/p1/issues/1:move", "{}", "Authorization", "Bearer "+write); w.Code != http.StatusOK {
		t.Errorf("POST :move con issues:write = %d, esperaba 200", w.Code)
	}
	// Un token de persona no manda x-agent-id.
	if w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", "Bearer "+all); w.Code != http.StatusOK {
		t.Fatalf("token de persona = %d", w.Code)
	}
	if _, md, _ := e.up.last(); len(md.Get("x-agent-id")) != 0 {
		t.Errorf("token de persona mandó x-agent-id %v", md.Get("x-agent-id"))
	}
}

// CU-025-5: con un ID token de Firebase, el x-agent-id que manda el cliente
// no llega al servicio (tampoco por Grpc-Metadata).
func TestXAgentIdDelClienteNoLlega(t *testing.T) {
	e := newEnv(t, nil)
	n, v := e.creds.header("duena", auth.AllowlistRole)
	w := e.do(http.MethodGet, "/v1/projects", "", n, v, "X-Agent-Id", "agente-falso", "Grpc-Metadata-X-Agent-Id", "agente-falso")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	_, md, _ := e.up.last()
	if got := md.Get("x-agent-id"); len(got) != 0 {
		t.Fatalf("x-agent-id del cliente llegó al servicio: %v", got)
	}
	if got := md.Get("x-auth-type"); len(got) != 1 || got[0] != "jwt" {
		t.Errorf("x-auth-type = %v", got)
	}
}

// Los scopes de token solo los da un token tka_: un ID token de Firebase
// que los trae como claim no lee con ellos.
func TestScopesDeTokenComoClaimSeDescartan(t *testing.T) {
	e := newEnv(t, nil)
	n, v := e.creds.withEmail("u-claim", "", false, auth.ScopeIssuesRead, auth.ScopeIssuesWrite)
	if w := e.do(http.MethodGet, "/v1/projects", "", n, v); w.Code != http.StatusForbidden {
		t.Fatalf("JWT con issues:read como claim = %d, esperaba 403", w.Code)
	}
}

// CU-025-7: un token revocado, vencido o desconocido responde 401 en
// cualquier ruta, y la respuesta no lo contiene. Un token mal formado ni se
// consulta.
func TestTokenInvalido401(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.creds.tokens.add("persona-1", "agente-1", auth.ScopeIssuesRead)
	e.creds.tokens.revoke(tok)
	unknown := auth.TokenPrefix + strings.Repeat("9", 43)
	for _, path := range []string{"/v1/projects", "/v1/actors", "/v1/tokens"} {
		for _, bad := range []string{tok, unknown} {
			w := e.do(http.MethodGet, path, "", "Authorization", "Bearer "+bad)
			if w.Code != http.StatusUnauthorized {
				t.Errorf("GET %s con token inválido = %d, esperaba 401", path, w.Code)
			}
			if strings.Contains(w.Body.String(), bad[4:]) {
				t.Errorf("la respuesta contiene el token")
			}
		}
	}
	before := e.creds.tokens.count()
	for _, bad := range []string{"tka_corto", auth.TokenPrefix + strings.Repeat("a", 44)} {
		if w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", "Bearer "+bad); w.Code != http.StatusUnauthorized {
			t.Errorf("token mal formado = %d", w.Code)
		}
	}
	if e.creds.tokens.count() != before {
		t.Errorf("un token mal formado no tiene que consultar a tekitla-issues")
	}
}

// Si tekitla-issues no responde, un token es 503 (no 401: no se sabe si es
// válido).
func TestTokenConServicioCaido503(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.creds.tokens.add("persona-1", "", auth.ScopeIssuesRead)
	e.creds.tokens.fail = fmt.Errorf("%w: caído", auth.ErrUnavailable)
	if w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", "Bearer "+tok); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("servicio caído = %d, esperaba 503", w.Code)
	}
	e.creds.tokens.fail = nil
	if w := e.do(http.MethodGet, "/v1/projects", "", "Authorization", "Bearer "+tok); w.Code != http.StatusOK {
		t.Fatalf("después de volver = %d (un 503 no se guarda en caché)", w.Code)
	}
}

// Auditoría: el access log de una escritura con token tiene persona, agente,
// token y ruta, y nunca el token.
func TestAuditoriaDeTokens(t *testing.T) {
	e := newEnv(t, nil)
	tok := e.creds.tokens.add("persona-1", "agente-1", auth.ScopeIssuesWrite)
	e.do(http.MethodPost, "/v1/projects/p1/issues/1:move", "{}", "Authorization", "Bearer "+tok)
	logs := e.logs.String()
	if strings.Contains(logs, tok[4:]) {
		t.Fatal("el log contiene el token")
	}
	for _, want := range []string{`"user_id":"persona-1"`, `"agent_id":"agente-1"`, `"token_id":"tok-1"`, `"auth":"token"`, `"route":"issues.v1.IssueService/MoveIssue"`, `"status":200`} {
		if !strings.Contains(logs, want) {
			t.Errorf("falta %s en el access log", want)
		}
	}
}
