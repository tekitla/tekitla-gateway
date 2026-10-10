package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// sinEnvDev evita que Load lea un .env.dev real del repo o de más arriba.
func sinEnvDev(t *testing.T) {
	t.Helper()
	t.Setenv("ENV_FILE", filepath.Join(t.TempDir(), "no-existe.env"))
}

func TestAllowedEmails(t *testing.T) {
	sinEnvDev(t)
	t.Setenv("AUTH_ALLOWED_EMAILS", " victor@ejemplo.com, ,otra@ejemplo.com ")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"victor@ejemplo.com", "otra@ejemplo.com"}; !slices.Equal(c.AllowedEmails, want) {
		t.Fatalf("AllowedEmails = %v, esperaba %v", c.AllowedEmails, want)
	}

	t.Setenv("AUTH_ALLOWED_EMAILS", "")
	if c, err = Load(); err != nil || len(c.AllowedEmails) != 0 {
		t.Fatalf("vacía: %v %v", c.AllowedEmails, err)
	}
}

func TestFirebaseJWKSURLSoloEnDesarrollo(t *testing.T) {
	sinEnvDev(t)
	t.Setenv("FIREBASE_JWKS_URL", "http://emisor-de-prueba:9000/jwks")
	t.Setenv("APP_ENV", "development")
	if _, err := Load(); err != nil {
		t.Fatalf("development: %v", err)
	}
	for _, env := range []string{"production", "staging"} {
		t.Setenv("APP_ENV", env)
		if _, err := Load(); err == nil || !strings.Contains(err.Error(), "FIREBASE_JWKS_URL") {
			t.Fatalf("APP_ENV=%s: esperaba error por FIREBASE_JWKS_URL, vino %v", env, err)
		}
	}
}
