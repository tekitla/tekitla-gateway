package upstream

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/reqctx"
)

// TokenAuth resuelve los tokens `tka_` con el RPC interno
// issues.v1.TokenAuthService/ResolveToken de tekitla-issues (issue 025). No
// tiene ruta REST: lo llama solo el gateway, como llamada interna (sin la
// metadata del cliente).
type TokenAuth struct {
	client  issuesv1.TokenAuthServiceClient
	timeout time.Duration
}

var _ auth.TokenResolver = (*TokenAuth)(nil)

// issuesErrorDomain es el Domain del google.rpc.ErrorInfo que adjunta
// tekitla-issues: un UNAUTHENTICATED sin él no lo dijo el servicio (un proxy,
// la auth de Cloud Run) y no prueba que el token sea inválido.
const issuesErrorDomain = "issues-service"

// tokenAuthTimeout acota la espera: con el servicio colgado, el request
// responde 503 en vez de esperar el timeout de la ruta.
const tokenAuthTimeout = 3 * time.Second

// NewTokenAuth usa conn, la conexión al upstream "issues".
func NewTokenAuth(conn grpc.ClientConnInterface) *TokenAuth {
	return &TokenAuth{client: issuesv1.NewTokenAuthServiceClient(conn), timeout: tokenAuthTimeout}
}

// ResolveToken implementa auth.TokenResolver: UNAUTHENTICATED de
// tekitla-issues -> token inválido (401); cualquier otra falla -> no
// disponible (503), nunca 401.
func (t *TokenAuth) ResolveToken(ctx context.Context, token string) (*auth.TokenResolution, error) {
	ctx, cancel := context.WithTimeout(reqctx.Internal(ctx), t.timeout)
	defer cancel()
	res, err := t.client.ResolveToken(ctx, &issuesv1.ResolveTokenRequest{Token: token})
	if err != nil {
		if st := status.Convert(err); st.Code() == codes.Unauthenticated && fromIssues(st) {
			return nil, fmt.Errorf("%w: token de Tekitla rechazado", auth.ErrInvalidCredentials)
		}
		// El detalle va al log del gateway (sin el token); al cliente, 503.
		return nil, fmt.Errorf("%w: ResolveToken: %s", auth.ErrUnavailable, status.Code(err))
	}
	out := &auth.TokenResolution{
		UserID:  res.GetUserId(),
		AgentID: res.GetAgentId(),
		TokenID: res.GetTokenId(),
	}
	if res.GetExpiresAt() != nil {
		out.ExpiresAt = res.GetExpiresAt().AsTime()
	}
	if out.UserID == "" {
		return nil, fmt.Errorf("%w: ResolveToken sin persona", auth.ErrUnavailable)
	}
	for _, s := range res.GetScopes() {
		switch s {
		case issuesv1.TokenScope_TOKEN_SCOPE_ISSUES_READ:
			out.Scopes = append(out.Scopes, auth.ScopeIssuesRead)
		case issuesv1.TokenScope_TOKEN_SCOPE_ISSUES_WRITE:
			out.Scopes = append(out.Scopes, auth.ScopeIssuesWrite)
		}
	}
	return out, nil
}

// fromIssues informa si el status trae el ErrorInfo de tekitla-issues.
func fromIssues(st *status.Status) bool {
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok && info.GetDomain() == issuesErrorDomain {
			return true
		}
	}
	return false
}
