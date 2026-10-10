package auth

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// AllowlistRole es el rol que otorga EmailAllowlist. routes.yaml lo pide en
// todas las rutas de Tekitla (rebanada 1: un solo dueño, sin roles propios).
const AllowlistRole = "allowlist"

// EmailAllowlist envuelve a otro Authenticator (Firebase) y otorga el rol
// AllowlistRole SOLO a las identidades cuyo email verificado (claims `email`
// y `email_verified=true` del ID token) está en la lista.
//
//   - Credencial inválida o ausente: se propaga el error del verificador (401).
//   - Credencial válida fuera de la lista (o email sin verificar): la
//     identidad pasa sin el rol, así que toda ruta con roles: [allowlist]
//     responde 403.
//   - Lista vacía: nadie recibe el rol (fail closed); cli/main.go lo avisa al
//     arrancar.
//
// El rol nunca se toma del token: si un custom claim lo trae, se descarta, para
// que la única forma de obtenerlo sea estar en AUTH_ALLOWED_EMAILS.
type EmailAllowlist struct {
	next   Authenticator
	emails map[string]struct{}
}

// NewEmailAllowlist valida los emails (comparación sin distinguir mayúsculas).
func NewEmailAllowlist(next Authenticator, emails []string) (*EmailAllowlist, error) {
	a := &EmailAllowlist{next: next, emails: make(map[string]struct{}, len(emails))}
	for _, e := range emails {
		n := normalizeEmail(e)
		if n == "" {
			continue
		}
		local, domain, ok := strings.Cut(n, "@")
		if !ok || local == "" || domain == "" || strings.ContainsAny(n, " ,;<>") || strings.Count(n, "@") != 1 {
			return nil, fmt.Errorf("AUTH_ALLOWED_EMAILS: %q no es un email válido", e)
		}
		a.emails[n] = struct{}{}
	}
	return a, nil
}

// Len es la cantidad de emails permitidos (0 = nadie entra).
func (a *EmailAllowlist) Len() int { return len(a.emails) }

// Allowed informa si el email está en la lista.
func (a *EmailAllowlist) Allowed(email string) bool {
	_, ok := a.emails[normalizeEmail(email)]
	return ok
}

// Authenticate implementa Authenticator.
func (a *EmailAllowlist) Authenticate(r *http.Request) (*Identity, error) {
	id, err := a.next.Authenticate(r)
	if err != nil || id == nil {
		return id, err
	}
	out := *id
	out.Roles = slices.DeleteFunc(slices.Clone(id.Roles), func(r string) bool { return r == AllowlistRole })
	if id.EmailVerified && id.Email != "" && a.Allowed(id.Email) {
		out.Roles = append(out.Roles, AllowlistRole)
	}
	return &out, nil
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }
