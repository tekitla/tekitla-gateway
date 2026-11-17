package gateway_test

// Alta en una columna (issue 033, CU-033-3): el REST de CreateIssue acepta
// status_id y lo pasa al servicio; un campo que no está en el contrato sigue
// siendo 400.

import (
	"net/http"
	"testing"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
	"google.golang.org/protobuf/proto"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

func TestCreateIssueConStatusID(t *testing.T) {
	e := newEnv(t, nil)
	bearer := "Bearer " + e.creds.tokens.add("persona-1", "", auth.ScopeIssuesWrite)

	body := `{"title":"desde el tablero","status_id":"22222222-2222-4222-8222-222222222222"}`
	if w := e.do(http.MethodPost, "/v1/projects/p1/issues", body, "Authorization", bearer); w.Code != http.StatusOK {
		t.Fatalf("POST con status_id = %d (%s)", w.Code, w.Body.String())
	}
	var req issuesv1.CreateIssueRequest
	if err := proto.Unmarshal(lastRequest(t, e, "/issues.v1.IssueService/CreateIssue"), &req); err != nil {
		t.Fatal(err)
	}
	if req.GetProjectId() != "p1" || req.GetTitle() != "desde el tablero" || req.GetStatusId() != "22222222-2222-4222-8222-222222222222" {
		t.Errorf("request a CreateIssue = %v", &req)
	}

	if w := e.do(http.MethodPost, "/v1/projects/p1/issues", `{"title":"x","columna":"hecho"}`, "Authorization", bearer); w.Code != http.StatusBadRequest {
		t.Errorf("POST con un campo desconocido = %d, esperaba 400", w.Code)
	}
}
