package gateway_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/auth/authtest"
	"github.com/tekitla/tekitla-gateway/app/gateway"
	"github.com/tekitla/tekitla-gateway/app/middleware"
	"github.com/tekitla/tekitla-gateway/app/ratelimit"
	"github.com/tekitla/tekitla-gateway/app/routes"
	"github.com/tekitla/tekitla-gateway/app/upstream"
)

// ---------------------------------------------------------------------------
// Upstream gRPC falso (bufconn): atiende CUALQUIER método de CUALQUIER
// servicio con una respuesta vacía, y guarda la metadata recibida. Sirve para
// todas las rutas de routes.yaml sin escribir un fake por servicio.
// ---------------------------------------------------------------------------

type fakeUpstream struct {
	lis *bufconn.Listener
	srv *grpc.Server

	mu     sync.Mutex
	calls  int
	method string
	md     metadata.MD
	fail   error         // si no es nil, se devuelve en vez de la respuesta
	delay  time.Duration // demora antes de responder (respeta el deadline)
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{lis: bufconn.Listen(1 << 20)}
	f.srv = grpc.NewServer(grpc.UnknownServiceHandler(f.handle))
	go func() { _ = f.srv.Serve(f.lis) }()
	t.Cleanup(f.srv.Stop)
	return f
}

func (f *fakeUpstream) handle(_ any, stream grpc.ServerStream) error {
	method, _ := grpc.MethodFromServerStream(stream)
	md, _ := metadata.FromIncomingContext(stream.Context())
	f.mu.Lock()
	f.calls++
	f.method, f.md = method, md.Copy()
	fail, delay := f.fail, f.delay
	f.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-stream.Context().Done():
			return status.FromContextError(stream.Context().Err()).Err()
		}
	}
	var in emptypb.Empty
	if err := stream.RecvMsg(&in); err != nil {
		return err
	}
	if fail != nil {
		return fail
	}
	return stream.SendMsg(&emptypb.Empty{})
}

func (f *fakeUpstream) last() (string, metadata.MD, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.method, f.md, f.calls
}

func (f *fakeUpstream) set(fail error, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail, f.delay = fail, delay
}

func (f *fakeUpstream) conn(t *testing.T) *grpc.ClientConn {
	t.Helper()
	opts := append([]grpc.DialOption{
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return f.lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	}, gateway.DialOptions()...)
	c, err := grpc.NewClient("passthrough:///bufnet", opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------------------------------------------------------------------------
// Credenciales de prueba según el modo de auth de la plantilla (firebase).
// ---------------------------------------------------------------------------

const hasAuth = true

type creds struct {
	authn auth.Authenticator

	issuer *authtest.Issuer
	// tokens: tokens tka_ de prueba (resolver falso de tekitla-issues).
	tokens *fakeTokens
}

// fakeTokens es un auth.TokenResolver en memoria: token en claro ->
// resolución (nil = inválido). Cuenta las llamadas para probar la caché.
type fakeTokens struct {
	mu    sync.Mutex
	byTok map[string]*auth.TokenResolution
	calls int
	fail  error
}

func (f *fakeTokens) ResolveToken(_ context.Context, tok string) (*auth.TokenResolution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail != nil {
		return nil, f.fail
	}
	if res, ok := f.byTok[tok]; ok && res != nil {
		return res, nil
	}
	return nil, auth.ErrInvalidCredentials
}

func (f *fakeTokens) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// add registra un token tka_ nuevo de la persona sub (agente opcional) con
// esos scopes y lo devuelve.
func (f *fakeTokens) add(sub, agent string, scopes ...string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := auth.TokenPrefix + fmt.Sprintf("%043d", len(f.byTok)+1)
	f.byTok[tok] = &auth.TokenResolution{UserID: sub, AgentID: agent, TokenID: fmt.Sprintf("tok-%d", len(f.byTok)+1),
		Scopes: scopes, ExpiresAt: time.Now().Add(24 * time.Hour)}
	return tok
}

func (f *fakeTokens) revoke(tok string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byTok[tok] = nil
}

const testProject = "proyecto-test"

// testAllowedEmail es el único email de AUTH_ALLOWED_EMAILS en los tests
// (mismo armado que cli/main.go: Firebase + EmailAllowlist).
const testAllowedEmail = "duena@tekitla.test"

func newCreds(t *testing.T, _ *routes.Table) *creds {
	t.Helper()
	is := authtest.NewIssuer(t)
	is.IssuerOverride = "https://securetoken.google.com/" + testProject
	v, err := auth.NewFirebase(testProject, auth.FirebaseOptions{JWKSURL: is.JWKSURL()})
	if err != nil {
		t.Fatal(err)
	}
	allow, err := auth.NewEmailAllowlist(v, []string{testAllowedEmail})
	if err != nil {
		t.Fatal(err)
	}
	// Mismo armado que cli/main.go: tka_ a tekitla-issues, el resto a Firebase.
	tokens := &fakeTokens{byTok: map[string]*auth.TokenResolution{}}
	authn := auth.TokenOrJWT{Tokens: auth.NewTekitlaTokens(tokens), JWT: allow}
	return &creds{issuer: is, authn: authn, tokens: tokens}
}

// header devuelve (nombre, valor) de una credencial válida con esos roles
// (sin roles = válida pero sin permisos). El rol auth.AllowlistRole no viaja
// como claim (el gateway lo ignora): se obtiene con el email permitido y
// verificado.
//
// Los scopes de token (issues:read, issues:write) solo los da un token tka_:
// con alguno de ellos se emite un token de Tekitla de un agente de sub.
func (c *creds) header(sub string, roles ...string) (string, string) {
	for _, r := range roles {
		if slices.Contains(auth.TokenScopes, r) {
			return "Authorization", "Bearer " + c.tokens.add(sub, "agente-de-"+sub, roles...)
		}
	}
	tokenRoles := []string{}
	email := ""
	for _, r := range roles {
		if r == auth.AllowlistRole {
			email = testAllowedEmail
			continue
		}
		tokenRoles = append(tokenRoles, r)
	}
	return c.withEmail(sub, email, email != "", tokenRoles...)
}

// withEmail: credencial válida con claims email/email_verified (email vacío =
// sin claim) y roles en el claim `roles`.
func (c *creds) withEmail(sub, email string, verified bool, roles ...string) (string, string) {
	if roles == nil {
		roles = []string{}
	}
	cl := c.issuer.Claims(sub, testProject, roles...)
	if email != "" {
		cl["email"] = email
		cl["email_verified"] = verified
	}
	return "Authorization", "Bearer " + c.issuer.Sign(cl, false)
}

// invalid devuelve una credencial con firma válida pero vencida.
func (c *creds) invalid() (string, string) {
	cl := c.issuer.Claims("u-vencido", testProject)
	cl["exp"] = time.Now().Add(-time.Hour).Unix()
	return "Authorization", "Bearer " + c.issuer.Sign(cl, false)
}

// ---------------------------------------------------------------------------
// Entorno: gateway completo (mismo armado que cli/main.go) contra el fake.
// ---------------------------------------------------------------------------

type env struct {
	t      *testing.T
	h      http.Handler
	up     *fakeUpstream
	creds  *creds
	table  *routes.Table
	report *gateway.Report
	logs   *syncBuffer
}

type envOpt func(*gateway.Options)

// loadRoutes carga el routes.yaml REAL del repo.
func loadRoutes(t *testing.T) *routes.Table {
	t.Helper()
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no encontré la raíz del módulo")
		}
		dir = parent
	}
	table, err := routes.Load(filepath.Join(dir, "routes.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func newEnv(t *testing.T, table *routes.Table, opts ...envOpt) *env {
	t.Helper()
	if table == nil {
		table = loadRoutes(t)
	}
	up := newFakeUpstream(t)
	conn := up.conn(t)
	c := newCreds(t, table)
	logs := &syncBuffer{}
	o := gateway.Options{
		Logger:    slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Routes:    table,
		Auth:      c.authn,
		Limiter:   ratelimit.NewMemory(0),
		CORS:      middleware.CORSConfig{AllowedOrigins: []string{"https://app.ejemplo.com"}},
		Challenge: "Bearer",
	}
	o.Upstreams = upstreamsFor(conn)
	for _, opt := range opts {
		opt(&o)
	}
	h, rep, err := gateway.New(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, h: h, up: up, creds: c, table: table, report: rep, logs: logs}
}

// upstreamsFor apunta todos los servicios de app/upstream/registry.go al fake.
func upstreamsFor(conn *grpc.ClientConn) []gateway.Upstream {
	var out []gateway.Upstream
	for _, svc := range upstream.Services {
		out = append(out, gateway.Upstream{Service: svc, Conn: conn})
	}
	return out
}

// do ejecuta un request contra el gateway. headers: pares nombre, valor
// (los nombres vacíos se ignoran).
func (e *env) do(method, path, body string, headers ...string) *httptest.ResponseRecorder {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "203.0.113.7:4242"
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] != "" {
			r.Header.Set(headers[i], headers[i+1])
		}
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// request arma el request de prueba de una ruta (path de ejemplo y "{}" si lleva body).
func request(b routes.Binding) (method, path, body string) {
	if b.Body != "" {
		body = "{}"
	}
	return b.Verb, b.SamplePath(), body
}

// syncBuffer: buffer seguro para el logger (el handler loguea desde varias goroutines).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
