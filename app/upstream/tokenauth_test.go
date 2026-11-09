package upstream_test

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/gateway"
	"github.com/tekitla/tekitla-gateway/app/reqctx"
	"github.com/tekitla/tekitla-gateway/app/upstream"
)

// fakeTokenAuth es tekitla-issues: responde lo que diga res/err y guarda la
// metadata recibida.
type fakeTokenAuth struct {
	issuesv1.UnimplementedTokenAuthServiceServer
	res *issuesv1.ResolveTokenResponse
	err error
	md  metadata.MD
}

func (f *fakeTokenAuth) ResolveToken(ctx context.Context, _ *issuesv1.ResolveTokenRequest) (*issuesv1.ResolveTokenResponse, error) {
	f.md, _ = metadata.FromIncomingContext(ctx)
	return f.res, f.err
}

func dial(t *testing.T, f *fakeTokenAuth) *upstream.TokenAuth {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	issuesv1.RegisterTokenAuthServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, gateway.DialOptions()...)
	conn, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return upstream.NewTokenAuth(conn)
}

// issuesStatus arma un error como los de tekitla-issues (con su ErrorInfo).
func issuesStatus(code codes.Code) error {
	st, _ := status.New(code, "invalid token").WithDetails(&errdetails.ErrorInfo{Domain: "issues-service", Reason: "UNAUTHENTICATED"})
	return st.Err()
}

func TestTokenAuthResuelve(t *testing.T) {
	exp := time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)
	f := &fakeTokenAuth{res: &issuesv1.ResolveTokenResponse{
		UserId: "u1", AgentId: "a1", TokenId: "t1", ExpiresAt: timestamppb.New(exp),
		Scopes: []issuesv1.TokenScope{issuesv1.TokenScope_TOKEN_SCOPE_ISSUES_READ, issuesv1.TokenScope_TOKEN_SCOPE_ISSUES_WRITE, issuesv1.TokenScope(99)},
	}}
	ta := dial(t, f)
	// Dentro de un request: va como llamada interna, sin identidad y con el
	// request id.
	ctx := reqctx.With(context.Background(), &reqctx.Info{RequestID: "req-1", Method: "issues.v1.IssueService/ListProjects",
		Identity: &auth.Identity{Subject: "otro"}})
	res, err := ta.ResolveToken(ctx, "tka_x")
	if err != nil {
		t.Fatal(err)
	}
	if res.UserID != "u1" || res.AgentID != "a1" || res.TokenID != "t1" || !res.ExpiresAt.Equal(exp) ||
		!slices.Equal(res.Scopes, []string{auth.ScopeIssuesRead, auth.ScopeIssuesWrite}) {
		t.Errorf("resolución: %+v (un scope desconocido se descarta)", res)
	}
	if got := f.md.Get("x-request-id"); len(got) != 1 || got[0] != "req-1" {
		t.Errorf("x-request-id = %v", got)
	}
	if len(f.md.Get("x-user-id")) != 0 {
		t.Errorf("la llamada interna no lleva identidad: %v", f.md)
	}
}

func TestTokenAuthErrores(t *testing.T) {
	f := &fakeTokenAuth{}
	ta := dial(t, f)
	for name, c := range map[string]struct {
		res  *issuesv1.ResolveTokenResponse
		err  error
		want error
	}{
		"rechazado por tekitla-issues": {err: issuesStatus(codes.Unauthenticated), want: auth.ErrInvalidCredentials},
		"UNAUTHENTICATED de otra capa": {err: status.Error(codes.Unauthenticated, "proxy"), want: auth.ErrUnavailable},
		"servicio caído":               {err: status.Error(codes.Unavailable, "down"), want: auth.ErrUnavailable},
		"error interno":                {err: issuesStatus(codes.Internal), want: auth.ErrUnavailable},
		"sin persona":                  {res: &issuesv1.ResolveTokenResponse{}, want: auth.ErrUnavailable},
	} {
		f.res, f.err = c.res, c.err
		if _, err := ta.ResolveToken(context.Background(), "tka_x"); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, esperaba %v", name, err, c.want)
		}
	}
}
