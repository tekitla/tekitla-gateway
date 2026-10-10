package auth_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/auth/authtest"
)

const proyecto = "tekitla-test"

// firebaseConAllowlist arma lo mismo que cli/main.go: Firebase + allowlist.
func firebaseConAllowlist(t *testing.T, emails ...string) (*auth.EmailAllowlist, *authtest.Issuer) {
	t.Helper()
	is := authtest.NewIssuer(t)
	is.IssuerOverride = "https://securetoken.google.com/" + proyecto
	v, err := auth.NewFirebase(proyecto, auth.FirebaseOptions{JWKSURL: is.JWKSURL()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := auth.NewEmailAllowlist(v, emails)
	if err != nil {
		t.Fatal(err)
	}
	return a, is
}

// tokenConEmail firma un ID token de Firebase válido; email "" = sin claim.
func tokenConEmail(is *authtest.Issuer, email string, verified any, roles ...string) string {
	c := is.Claims("uid-1", proyecto, roles...)
	if email != "" {
		c["email"] = email
	}
	if verified != nil {
		c["email_verified"] = verified
	}
	return is.Sign(c, false)
}

func autenticar(a auth.Authenticator, tok string) (*auth.Identity, error) {
	r := httptest.NewRequest("GET", "/", nil)
	if tok != "" {
		r.Header.Set("Authorization", "Bearer "+tok)
	}
	return a.Authenticate(r)
}

func TestAllowlistOtorgaElRol(t *testing.T) {
	a, is := firebaseConAllowlist(t, " Victor@Ejemplo.com ", "otra@ejemplo.com")
	if a.Len() != 2 {
		t.Fatalf("Len = %d, esperaba 2", a.Len())
	}
	// Mayúsculas/espacios no importan; los roles del token se conservan.
	id, err := autenticar(a, tokenConEmail(is, "victor@EJEMPLO.com", true, "otro.rol"))
	if err != nil {
		t.Fatal(err)
	}
	if id.Subject != "uid-1" || !id.HasAnyRole([]string{auth.AllowlistRole}) || !id.HasAnyRole([]string{"otro.rol"}) {
		t.Fatalf("identidad inesperada: %+v", id)
	}
}

func TestAllowlistNoOtorgaElRol(t *testing.T) {
	a, is := firebaseConAllowlist(t, "victor@ejemplo.com")
	cases := map[string]string{
		"email fuera de la lista":     tokenConEmail(is, "intruso@ejemplo.com", true),
		"email sin verificar":         tokenConEmail(is, "victor@ejemplo.com", false),
		"sin email_verified":          tokenConEmail(is, "victor@ejemplo.com", nil),
		"email_verified como string":  tokenConEmail(is, "victor@ejemplo.com", "true"),
		"sin email":                   tokenConEmail(is, "", true),
		"rol como custom claim":       tokenConEmail(is, "intruso@ejemplo.com", true, auth.AllowlistRole),
		"rol como claim y sin email":  tokenConEmail(is, "", nil, auth.AllowlistRole, "otro.rol"),
		"subdominio del permitido":    tokenConEmail(is, "victor@ejemplo.com.ar", true),
		"prefijo del email permitido": tokenConEmail(is, "xvictor@ejemplo.com", true),
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			id, err := autenticar(a, tok)
			if err != nil {
				t.Fatalf("la credencial es válida (403, no 401): %v", err)
			}
			if id.HasAnyRole([]string{auth.AllowlistRole}) {
				t.Fatalf("otorgó %q: %+v", auth.AllowlistRole, id)
			}
		})
	}
}

func TestAllowlistVaciaNadieEntra(t *testing.T) {
	a, is := firebaseConAllowlist(t)
	if a.Len() != 0 {
		t.Fatalf("Len = %d", a.Len())
	}
	id, err := autenticar(a, tokenConEmail(is, "victor@ejemplo.com", true))
	if err != nil || id.HasAnyRole([]string{auth.AllowlistRole}) {
		t.Fatalf("lista vacía tiene que cerrar todo: id=%+v err=%v", id, err)
	}
}

func TestAllowlistPropagaErrores(t *testing.T) {
	a, is := firebaseConAllowlist(t, "victor@ejemplo.com")
	if _, err := autenticar(a, ""); !errors.Is(err, auth.ErrNoCredentials) {
		t.Fatalf("sin token: %v", err)
	}
	c := is.Claims("uid-1", proyecto)
	c["email"], c["email_verified"] = "victor@ejemplo.com", true
	c["exp"] = time.Now().Add(-time.Hour).Unix()
	if _, err := autenticar(a, is.Sign(c, false)); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("token vencido de un email permitido: %v", err)
	}
}

// identidadFija devuelve siempre la misma identidad (sin verificar nada).
type identidadFija struct{ id *auth.Identity }

func (f identidadFija) Authenticate(*http.Request) (*auth.Identity, error) { return f.id, nil }

// El wrapper no modifica la identidad que devuelve el verificador.
func TestAllowlistNoMutaLaIdentidadOriginal(t *testing.T) {
	orig := &auth.Identity{Subject: "s", Roles: []string{"a", auth.AllowlistRole, "b"}, Kind: "jwt", Email: "x@y.z", EmailVerified: true}
	a, err := auth.NewEmailAllowlist(identidadFija{orig}, []string{"x@y.z"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := autenticar(a, "cualquiera")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(orig.Roles, []string{"a", auth.AllowlistRole, "b"}) {
		t.Fatalf("mutó los roles originales: %v", orig.Roles)
	}
	if !slices.Equal(id.Roles, []string{"a", "b", auth.AllowlistRole}) {
		t.Fatalf("roles = %v", id.Roles)
	}
}

func TestAllowlistEmailsInvalidos(t *testing.T) {
	for _, e := range []string{"sin-arroba", "@ejemplo.com", "victor@", "a@b@c", "victor @ejemplo.com", "a<b>@c.d"} {
		if _, err := auth.NewEmailAllowlist(auth.None{}, []string{e}); err == nil {
			t.Errorf("%q: esperaba error", e)
		}
	}
	a, err := auth.NewEmailAllowlist(auth.None{}, []string{"", "  "})
	if err != nil || a.Len() != 0 {
		t.Fatalf("entradas vacías se ignoran: len=%v err=%v", a, err)
	}
}
