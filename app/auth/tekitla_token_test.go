package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

type countingResolver struct {
	mu    sync.Mutex
	res   *auth.TokenResolution
	err   error
	calls int
}

func (c *countingResolver) ResolveToken(context.Context, string) (*auth.TokenResolution, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.res, c.err
}

func authenticate(t *testing.T, a auth.Authenticator, tok string) (*auth.Identity, error) {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/projects", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	return a.Authenticate(r)
}

var validTok = auth.TokenPrefix + strings.Repeat("A", 43)

// La caché guarda 60 s como máximo (la demora de una revocación) y nunca
// más allá del vencimiento del token.
func TestTekitlaTokensCache(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	res := &countingResolver{res: &auth.TokenResolution{UserID: "u1", AgentID: "a1", TokenID: "t1",
		Scopes: []string{"issues:read", "allowlist", "issues:read"}, ExpiresAt: now.Add(time.Hour)}}
	a := auth.NewTekitlaTokens(res).WithClock(func() time.Time { return now })

	id, err := authenticate(t, a, validTok)
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "u1" || id.AgentID != "a1" || id.TokenID != "t1" || id.Kind != "token" || !slices.Equal(id.Roles, []string{"issues:read"}) {
		t.Errorf("identidad: %+v (los roles son solo scopes conocidos, sin repetidos)", id)
	}
	now = now.Add(59 * time.Second)
	authenticate(t, a, validTok)
	if res.calls != 1 {
		t.Errorf("dentro de los 60 s: %d llamadas, esperaba 1", res.calls)
	}
	now = now.Add(time.Second)
	authenticate(t, a, validTok)
	if res.calls != 2 {
		t.Errorf("a los 60 s: %d llamadas, esperaba 2", res.calls)
	}

	// Vence en 10 s: no se guarda más que eso.
	res.res = &auth.TokenResolution{UserID: "u1", Scopes: []string{"issues:read"}, ExpiresAt: now.Add(10 * time.Second)}
	other := auth.TokenPrefix + strings.Repeat("B", 43)
	authenticate(t, a, other)
	now = now.Add(11 * time.Second)
	authenticate(t, a, other)
	if res.calls != 4 {
		t.Errorf("caché más allá del vencimiento: %d llamadas, esperaba 4", res.calls)
	}

	// Inválido: también se guarda (no se consulta en cada intento) y da 401.
	res.res, res.err = nil, auth.ErrInvalidCredentials
	bad := auth.TokenPrefix + strings.Repeat("C", 43)
	for range 3 {
		if _, err := authenticate(t, a, bad); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("inválido: %v", err)
		}
	}
	if res.calls != 5 {
		t.Errorf("inválidos repetidos: %d llamadas, esperaba 5", res.calls)
	}

	// No disponible: 503 y no se guarda.
	res.err = auth.ErrUnavailable
	down := auth.TokenPrefix + strings.Repeat("D", 43)
	for range 2 {
		if _, err := authenticate(t, a, down); !errors.Is(err, auth.ErrUnavailable) {
			t.Errorf("no disponible: %v", err)
		}
	}
	if res.calls != 7 {
		t.Errorf("no disponible no se guarda: %d llamadas, esperaba 7", res.calls)
	}
}

func TestTekitlaTokensFormato(t *testing.T) {
	res := &countingResolver{res: &auth.TokenResolution{UserID: "u1"}}
	a := auth.NewTekitlaTokens(res)
	for _, tok := range []string{"tka_", "tka_corto", validTok + "x"} {
		if _, err := authenticate(t, a, tok); !errors.Is(err, auth.ErrInvalidCredentials) {
			t.Errorf("%q: %v", tok, err)
		}
	}
	if _, err := authenticate(t, a, "eyJhbGciOi.jwt.token"); !errors.Is(err, auth.ErrNoCredentials) {
		t.Errorf("un JWT no es de este autenticador: %v", err)
	}
	if res.calls != 0 {
		t.Errorf("mal formados no se consultan: %d", res.calls)
	}
	res.res = &auth.TokenResolution{}
	if _, err := authenticate(t, a, validTok); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("sin persona responsable: %v", err)
	}
}

// recordingAuth devuelve siempre id y cuenta las llamadas.
type recordingAuth struct {
	id    *auth.Identity
	calls int
}

func (r *recordingAuth) Authenticate(*http.Request) (*auth.Identity, error) {
	r.calls++
	return r.id, nil
}

func TestTokenOrJWT(t *testing.T) {
	jwt := &recordingAuth{id: &auth.Identity{Subject: "u1", Kind: "jwt", Roles: []string{"allowlist", "issues:read", "issues:write", "otro"}, AgentID: "colado"}}
	tok := &recordingAuth{id: &auth.Identity{Subject: "u2", Kind: "token", Roles: []string{"issues:read"}}}
	a := auth.TokenOrJWT{Tokens: tok, JWT: jwt}

	id, err := authenticate(t, a, validTok)
	if err != nil || id.Subject != "u2" || tok.calls != 1 || jwt.calls != 0 {
		t.Fatalf("tka_ va a Tokens: %+v %v", id, err)
	}
	id, err = authenticate(t, a, "eyJ.otro.jwt")
	if err != nil || jwt.calls != 1 {
		t.Fatalf("el resto va a JWT: %v", err)
	}
	if !slices.Equal(id.Roles, []string{"allowlist", "otro"}) || id.AgentID != "" {
		t.Errorf("un JWT no trae scopes de token ni agente: %+v", id)
	}
	if !slices.Equal(jwt.id.Roles, []string{"allowlist", "issues:read", "issues:write", "otro"}) {
		t.Errorf("no tiene que mutar la identidad original")
	}
}

// Una resolución ya vencida (relojes desfasados) no autentica.
func TestTekitlaTokensResolucionVencida(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	res := &countingResolver{res: &auth.TokenResolution{UserID: "u1", Scopes: []string{"issues:read"}, ExpiresAt: now.Add(-5 * time.Second)}}
	a := auth.NewTekitlaTokens(res).WithClock(func() time.Time { return now })
	if _, err := authenticate(t, a, validTok); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("vencida: %v", err)
	}
}

// Las consultas por tokens sin caché tienen su propio límite; los que ya
// están en caché no lo gastan.
func TestTekitlaTokensMissLimiter(t *testing.T) {
	res := &countingResolver{res: &auth.TokenResolution{UserID: "u1", Scopes: []string{"issues:read"}, ExpiresAt: time.Now().Add(time.Hour)}}
	budget := 1
	a := auth.NewTekitlaTokens(res).WithMissLimiter(func(*http.Request) bool {
		budget--
		return budget >= 0
	})
	if _, err := authenticate(t, a, validTok); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticate(t, a, validTok); err != nil {
		t.Errorf("en caché no gasta el límite: %v", err)
	}
	other := auth.TokenPrefix + strings.Repeat("Z", 43)
	if _, err := authenticate(t, a, other); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("sin presupuesto: %v", err)
	}
	if res.calls != 1 {
		t.Errorf("consultas: %d, esperaba 1", res.calls)
	}
}

// Los tokens rechazados no desalojan a los válidos.
func TestTekitlaTokensRechazosNoDesalojan(t *testing.T) {
	good := &auth.TokenResolution{UserID: "u1", Scopes: []string{"issues:read"}, ExpiresAt: time.Now().Add(time.Hour)}
	res := &countingResolver{res: good}
	a := auth.NewTekitlaTokens(res)
	authenticate(t, a, validTok)
	res.res, res.err = nil, auth.ErrInvalidCredentials
	for i := range 10001 {
		authenticate(t, a, auth.TokenPrefix+fmt.Sprintf("%043d", i))
	}
	before := res.calls
	res.res, res.err = good, nil
	if _, err := authenticate(t, a, validTok); err != nil || res.calls != before {
		t.Errorf("el válido tenía que seguir en caché: %v (%d consultas nuevas)", err, res.calls-before)
	}
}

// slowResolver tarda en responder, para juntar requests concurrentes.
type slowResolver struct {
	countingResolver
	gate chan struct{}
}

func (s *slowResolver) ResolveToken(ctx context.Context, tok string) (*auth.TokenResolution, error) {
	<-s.gate
	return s.countingResolver.ResolveToken(ctx, tok)
}

// Varios requests con el mismo token sin caché hacen una sola consulta.
func TestTekitlaTokensUnaConsultaPorToken(t *testing.T) {
	res := &slowResolver{countingResolver: countingResolver{res: &auth.TokenResolution{UserID: "u1", Scopes: []string{"issues:read"}, ExpiresAt: time.Now().Add(time.Hour)}}, gate: make(chan struct{})}
	a := auth.NewTekitlaTokens(res)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest("GET", "/v1/projects", nil)
			r.Header.Set("Authorization", "Bearer "+validTok)
			if _, err := a.Authenticate(r); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(res.gate)
	wg.Wait()
	if res.calls != 1 {
		t.Errorf("consultas: %d, esperaba 1", res.calls)
	}
}
