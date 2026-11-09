package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Tokens de Tekitla (issue 025, ADR 0009): `Authorization: Bearer tka_…`,
// emitidos por tekitla-issues a nombre de una persona o de uno de sus
// agentes. El gateway no los conoce: los resuelve con el RPC interno
// TokenAuthService.ResolveToken y guarda el resultado en caché.

// TokenPrefix identifica un token de Tekitla (secret scanning, logs).
const TokenPrefix = "tka_"

// tokenLen: "tka_" + 32 bytes en base64url sin padding.
const tokenLen = len(TokenPrefix) + 43

// Scopes de un token: los roles que pueden pedir las rutas de routes.yaml.
// Solo los otorga un token de Tekitla (ver TokenOrJWT).
const (
	ScopeIssuesRead  = "issues:read"
	ScopeIssuesWrite = "issues:write"
)

// TokenScopes son todos los scopes de token.
var TokenScopes = []string{ScopeIssuesRead, ScopeIssuesWrite}

// TokenCacheTTL es lo máximo que se guarda un resultado: es la demora de una
// revocación (ADR 0009).
const TokenCacheTTL = 60 * time.Second

// tokenCacheMax acota la memoria de cada caché (válidos e inválidos van en
// mapas separados: una lluvia de tokens falsos no desaloja a los buenos).
const tokenCacheMax = 10000

// TokenResolution es la identidad de un token válido.
type TokenResolution struct {
	// UserID: uid de la persona responsable (va en x-user-id).
	UserID string
	// AgentID: id del agente si el token es de un agente (va en x-agent-id).
	AgentID string
	// TokenID: para la auditoría (nunca el token ni su hash).
	TokenID string
	Scopes  []string
	// ExpiresAt: la caché no lo guarda más allá.
	ExpiresAt time.Time
}

// TokenResolver resuelve un token en claro. Devuelve ErrInvalidCredentials si
// no vale (no existe, venció, revocado) y ErrUnavailable si no se pudo
// verificar (servicio caído): 401 y 503.
type TokenResolver interface {
	ResolveToken(ctx context.Context, token string) (*TokenResolution, error)
}

// MissLimiter decide si un request puede consultar a tekitla-issues por un
// token que no está en caché (devuelve false si no). La autenticación corre
// antes del rate limit de la ruta: sin esto, tokens inventados harían una
// consulta a la base por request.
type MissLimiter func(r *http.Request) bool

// TekitlaTokens es el Authenticator de los tokens `tka_`.
type TekitlaTokens struct {
	resolver TokenResolver
	now      func() time.Time
	allow    MissLimiter

	mu       sync.Mutex
	valid    map[[sha256.Size]byte]tokenCacheEntry
	rejected map[[sha256.Size]byte]tokenCacheEntry
	group    singleflight.Group
}

type tokenCacheEntry struct {
	res   *TokenResolution // nil = token inválido
	until time.Time
}

// NewTekitlaTokens construye el autenticador.
func NewTekitlaTokens(resolver TokenResolver) *TekitlaTokens {
	return &TekitlaTokens{
		resolver: resolver, now: time.Now,
		valid: map[[sha256.Size]byte]tokenCacheEntry{}, rejected: map[[sha256.Size]byte]tokenCacheEntry{},
	}
}

// WithMissLimiter acota las consultas por tokens que no están en caché.
func (t *TekitlaTokens) WithMissLimiter(allow MissLimiter) *TekitlaTokens {
	t.allow = allow
	return t
}

// WithClock reemplaza el reloj (tests).
func (t *TekitlaTokens) WithClock(now func() time.Time) *TekitlaTokens {
	t.now = now
	return t
}

// IsTekitlaToken informa si un bearer es un token de Tekitla (por el
// prefijo; el largo lo valida Authenticate).
func IsTekitlaToken(bearer string) bool { return strings.HasPrefix(bearer, TokenPrefix) }

// Authenticate implementa Authenticator.
func (t *TekitlaTokens) Authenticate(r *http.Request) (*Identity, error) {
	tok, err := bearerToken(r)
	if err != nil {
		return nil, err
	}
	if !IsTekitlaToken(tok) {
		return nil, ErrNoCredentials
	}
	if len(tok) != tokenLen {
		return nil, fmt.Errorf("%w: token de Tekitla mal formado", ErrInvalidCredentials)
	}
	key := sha256.Sum256([]byte(tok))
	now := t.now()
	if e, ok := t.lookup(key, now); ok {
		return identityFrom(e.res, now)
	}
	if t.allow != nil && !t.allow(r) {
		return nil, fmt.Errorf("%w: demasiados tokens desconocidos desde este cliente", ErrInvalidCredentials)
	}
	// Varios requests con el mismo token sin caché hacen una sola consulta.
	v, err, _ := t.group.Do(string(key[:]), func() (any, error) {
		res, err := t.resolver.ResolveToken(context.WithoutCancel(r.Context()), tok)
		switch {
		case errors.Is(err, ErrInvalidCredentials):
			t.store(t.rejected, key, tokenCacheEntry{until: now.Add(TokenCacheTTL)}, now)
			return nil, err
		case err != nil:
			// No se guarda: el próximo request vuelve a intentar.
			return nil, err
		}
		until := now.Add(TokenCacheTTL)
		if !res.ExpiresAt.IsZero() && res.ExpiresAt.Before(until) {
			until = res.ExpiresAt
		}
		t.store(t.valid, key, tokenCacheEntry{res: res, until: until}, now)
		return res, nil
	})
	if err != nil {
		return nil, err
	}
	return identityFrom(v.(*TokenResolution), now)
}

func (t *TekitlaTokens) lookup(key [sha256.Size]byte, now time.Time) (tokenCacheEntry, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, cache := range []map[[sha256.Size]byte]tokenCacheEntry{t.valid, t.rejected} {
		if e, ok := cache[key]; ok && now.Before(e.until) {
			return e, true
		}
	}
	return tokenCacheEntry{}, false
}

func (t *TekitlaTokens) store(cache map[[sha256.Size]byte]tokenCacheEntry, key [sha256.Size]byte, e tokenCacheEntry, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(cache) >= tokenCacheMax {
		for k, old := range cache {
			if !now.Before(old.until) {
				delete(cache, k)
			}
		}
		if len(cache) >= tokenCacheMax {
			// Muchos tokens distintos en menos de 60 s: se empieza de cero
			// antes que crecer sin límite (solo esta caché).
			clear(cache)
		}
	}
	cache[key] = e
}

// identityFrom arma la identidad de un token. Los roles son solo sus scopes
// conocidos: un token nunca trae `allowlist` ni otro rol.
func identityFrom(res *TokenResolution, now time.Time) (*Identity, error) {
	if res == nil {
		return nil, fmt.Errorf("%w: token de Tekitla inválido", ErrInvalidCredentials)
	}
	if !res.ExpiresAt.IsZero() && !now.Before(res.ExpiresAt) {
		return nil, fmt.Errorf("%w: token de Tekitla vencido", ErrInvalidCredentials)
	}
	if res.UserID == "" {
		return nil, fmt.Errorf("%w: token sin persona responsable", ErrInvalidCredentials)
	}
	roles := []string{}
	for _, s := range res.Scopes {
		if slices.Contains(TokenScopes, s) && !slices.Contains(roles, s) {
			roles = append(roles, s)
		}
	}
	return &Identity{Subject: res.UserID, AgentID: res.AgentID, TokenID: res.TokenID, Roles: roles, Kind: "token"}, nil
}

// TokenOrJWT elige el verificador por el bearer: un `tka_…` va a Tokens y
// cualquier otro a JWT (Firebase + allowlist). Los scopes de token son solo
// de los tokens: si un JWT los trae como claim, se descartan.
type TokenOrJWT struct {
	Tokens Authenticator
	JWT    Authenticator
}

// Authenticate implementa Authenticator.
func (a TokenOrJWT) Authenticate(r *http.Request) (*Identity, error) {
	if tok, err := bearerToken(r); err == nil && IsTekitlaToken(tok) {
		return a.Tokens.Authenticate(r)
	}
	id, err := a.JWT.Authenticate(r)
	if err != nil || id == nil {
		return id, err
	}
	out := *id
	out.Roles = slices.DeleteFunc(slices.Clone(id.Roles), func(r string) bool { return slices.Contains(TokenScopes, r) })
	out.AgentID, out.TokenID = "", ""
	return &out, nil
}
