package gateway_test

import (
	"net/http"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/gateway"
)

// Webhooks de GitHub y GitLab (issue 016): handler propio que pasa el cuerpo
// crudo al RPC interno WebhookService/ReceiveWebhook. La firma la verifica
// tekitla-issues; acá se prueba el transporte. Payloads inventados.

func TestWebhook_PassesRawBodyAndOnlyItsHeaders(t *testing.T) {
	e := newEnv(t, nil, withWebhooks)
	body := `{"ref":"refs/heads/feat/tk-9-login","campo_que_no_esta_en_ningun_proto":{"x":[1,2]}}`
	e.up.respond = func(string, []byte) (proto.Message, error) {
		return &issuesv1.ReceiveWebhookResponse{Outcome: issuesv1.WebhookOutcome_WEBHOOK_OUTCOME_PROCESSED}, nil
	}
	w := e.do(http.MethodPost, gateway.WebhookPathGitHub, body,
		"X-GitHub-Event", "push", "X-GitHub-Delivery", "d-1", "X-Hub-Signature-256", "sha256=abc",
		"Authorization", "Bearer algo", "X-Gitlab-Token", "no-debe-pasar", "Cookie", "c=1")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"processed"`) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	method, md, _ := e.up.last()
	if method != "/issues.v1.WebhookService/ReceiveWebhook" {
		t.Fatalf("método: %s", method)
	}
	// Llamada interna: sin identidad ni roles.
	if len(md.Get("x-user-id")) != 0 || len(md.Get("x-roles")) != 0 || len(md.Get("authorization")) != 0 {
		t.Errorf("metadata con identidad: %v", md)
	}
	reqs := e.up.requests()
	var got issuesv1.ReceiveWebhookRequest
	if err := proto.Unmarshal(reqs[len(reqs)-1].raw, &got); err != nil {
		t.Fatal(err)
	}
	if string(got.GetBody()) != body || got.GetEvent() != "push" || got.GetDeliveryId() != "d-1" ||
		got.GetSignature() != "sha256=abc" || got.GetToken() != "" || got.GetProvider() != issuesv1.RepoProvider_REPO_PROVIDER_GITHUB {
		t.Errorf("request: %+v", &got)
	}
	// La firma no queda en los logs.
	if strings.Contains(e.logs.String(), "sha256=abc") {
		t.Error("la firma quedó en los logs")
	}
}

func TestWebhook_GitLabHeaders(t *testing.T) {
	e := newEnv(t, nil, withWebhooks)
	e.up.respond = func(string, []byte) (proto.Message, error) {
		return &issuesv1.ReceiveWebhookResponse{Outcome: issuesv1.WebhookOutcome_WEBHOOK_OUTCOME_IGNORED}, nil
	}
	w := e.do(http.MethodPost, gateway.WebhookPathGitLab, `{}`,
		"X-Gitlab-Event", "Push Hook", "X-Gitlab-Event-UUID", "u-1", "X-Gitlab-Token", "tkw_prueba", "X-Hub-Signature-256", "sha256=no")
	if w.Code != http.StatusAccepted {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}
	reqs := e.up.requests()
	var got issuesv1.ReceiveWebhookRequest
	_ = proto.Unmarshal(reqs[len(reqs)-1].raw, &got)
	if got.GetProvider() != issuesv1.RepoProvider_REPO_PROVIDER_GITLAB || got.GetToken() != "tkw_prueba" || got.GetSignature() != "" ||
		got.GetDeliveryId() != "u-1" || got.GetEvent() != "Push Hook" {
		t.Errorf("request: %+v", &got)
	}
	if strings.Contains(e.logs.String(), "tkw_prueba") {
		t.Error("el secreto quedó en los logs")
	}
}

// CU-016-2: firma inválida → 401 sin detalle.
func TestWebhook_Errors(t *testing.T) {
	e := newEnv(t, nil, withWebhooks)
	cases := []struct {
		err  error
		want int
	}{
		{status.Error(codes.Unauthenticated, "invalid webhook signature: detalle interno"), http.StatusUnauthorized},
		{status.Error(codes.InvalidArgument, "x"), http.StatusBadRequest},
		{status.Error(codes.Unavailable, "x"), http.StatusServiceUnavailable},
		{status.Error(codes.Internal, "secreto interno"), http.StatusInternalServerError},
	}
	for _, c := range cases {
		e.up.set(c.err, 0)
		w := e.do(http.MethodPost, gateway.WebhookPathGitHub, `{}`, "X-GitHub-Event", "push", "X-GitHub-Delivery", "d")
		if w.Code != c.want {
			t.Errorf("%v: got %d, want %d", status.Code(c.err), w.Code, c.want)
		}
		if strings.Contains(w.Body.String(), "detalle interno") || strings.Contains(w.Body.String(), "secreto interno") {
			t.Errorf("%v: el cuerpo filtra el error del servicio: %s", status.Code(c.err), w.Body.String())
		}
	}
	e.up.set(nil, 0)

	// Transporte: verbo, Content-Type y tamaño.
	if w := e.do(http.MethodGet, gateway.WebhookPathGitHub, ""); w.Code != http.StatusNotFound {
		t.Errorf("GET: %d", w.Code)
	}
	if w := e.do(http.MethodPost, gateway.WebhookPathGitHub, `{}`, "Content-Type", "application/x-www-form-urlencoded"); w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("form: %d", w.Code)
	}
	big := `{"x":"` + strings.Repeat("a", 1<<20) + `"}`
	if w := e.do(http.MethodPost, gateway.WebhookPathGitHub, big); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("1 MiB: %d", w.Code)
	}
}

func TestWebhook_RateLimitPerIP(t *testing.T) {
	e := newEnv(t, nil, withWebhooks)
	got429 := false
	for i := 0; i < 70; i++ {
		w := e.do(http.MethodPost, gateway.WebhookPathGitHub, `{}`)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			if w.Header().Get("Retry-After") == "" {
				t.Error("429 sin Retry-After")
			}
			break
		}
	}
	if !got429 {
		t.Error("sin 429 tras la ráfaga de 60")
	}
	// GitLab tiene su propio bucket.
	if w := e.do(http.MethodPost, gateway.WebhookPathGitLab, `{}`); w.Code == http.StatusTooManyRequests {
		t.Error("el bucket de GitLab se gastó con GitHub")
	}
}

func TestWebhook_NotMountedWithoutClient(t *testing.T) {
	e := newEnv(t, nil)
	if w := e.do(http.MethodPost, gateway.WebhookPathGitHub, `{}`); w.Code != http.StatusNotFound {
		t.Errorf("sin cliente: %d", w.Code)
	}
}

// ReceiveWebhook no tiene ruta REST: no se puede llamar por el mux.
func TestWebhook_InternalRPCHasNoRoute(t *testing.T) {
	e := newEnv(t, nil, withWebhooks)
	for _, b := range e.report.Bindings {
		if strings.HasPrefix(b.Method, "issues.v1.WebhookService/") {
			t.Errorf("WebhookService con ruta HTTP: %s %s", b.Verb, b.Template)
		}
	}
}

func withWebhooks(o *gateway.Options) {
	o.Webhooks = issuesv1.NewWebhookServiceClient(o.Upstreams[0].Conn)
}
