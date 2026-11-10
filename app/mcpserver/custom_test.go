package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/routes"
)

type anyAuth struct{}

func (anyAuth) Authenticate(*http.Request) (*auth.Identity, error) {
	return &auth.Identity{Subject: "u"}, nil
}

const customRoutes = `
version: 1
routes:
  t.v1.S/List:
    roles: [r]
    mcp:
      tool: list_things
      description: "Lista cosas."
  t.v1.S/Create:
    roles: [w]
  t.v1.S/Internal:
    deny: true
`

var customBindings = []routes.Binding{
	{Method: "t.v1.S/List", Verb: http.MethodGet, Template: "/v1/things"},
	{Method: "t.v1.S/Create", Verb: http.MethodPost, Template: "/v1/things", Body: "*"},
	{Method: "t.v1.S/Internal", Verb: http.MethodGet, Template: "/v1/internal"},
}

func TestAddCustomValida(t *testing.T) {
	table, err := routes.Parse([]byte(customRoutes))
	if err != nil {
		t.Fatal(err)
	}
	handle := func(context.Context, *Caller, map[string]any) (any, error) { return nil, nil }
	schema := map[string]any{"type": "object"}
	cases := map[string]struct {
		tool CustomTool
		want string // "" = sin error
	}{
		"reemplaza la de routes.yaml": {CustomTool{Name: "list_things", InputSchema: schema, Methods: []string{"t.v1.S/List"}, Handle: handle}, ""},
		"propia con descripción":      {CustomTool{Name: "next_thing", Description: "La siguiente.", InputSchema: schema, Methods: []string{"t.v1.S/List"}, Handle: handle}, ""},
		"propia sin descripción":      {CustomTool{Name: "next_thing", InputSchema: schema, Methods: []string{"t.v1.S/List"}, Handle: handle}, "Description obligatoria"},
		"descripción duplicada":       {CustomTool{Name: "list_things", Description: "x", InputSchema: schema, Methods: []string{"t.v1.S/List"}, Handle: handle}, "ya tiene descripción"},
		"ruta sin declarar":           {CustomTool{Name: "x_y", Description: "x", InputSchema: schema, Methods: []string{"t.v1.S/Nada"}, Handle: handle}, "no está en routes.yaml"},
		"ruta en deny":                {CustomTool{Name: "x_y", Description: "x", InputSchema: schema, Methods: []string{"t.v1.S/Internal"}, Handle: handle}, "deny"},
		"ruta de escritura":           {CustomTool{Name: "x_y", Description: "x", InputSchema: schema, Methods: []string{"t.v1.S/Create"}, Handle: handle}, "solo lectura"},
		"incompleta":                  {CustomTool{Name: "x_y", Description: "x", Methods: []string{"t.v1.S/List"}, Handle: handle}, "incompleta"},
	}
	// list_things necesita el registro de protobuf si queda genérica: en
	// los casos que no la reemplazan, la reemplaza una propia válida.
	replacement := CustomTool{Name: "list_things", InputSchema: schema, Methods: []string{"t.v1.S/List"}, Handle: handle}
	for name, c := range cases {
		tools := []CustomTool{c.tool}
		if c.tool.Name != "list_things" {
			tools = append(tools, replacement)
		}
		_, err := New(Options{
			Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			Routes:   table,
			Bindings: customBindings,
			Auth:     anyAuth{},
			Name:     "t",
			Dispatch: http.NotFoundHandler(),
			Custom:   tools,
		})
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error = %v, se esperaba %q", name, err, c.want)
		}
	}
}

func TestCallerSoloMetodosDeclarados(t *testing.T) {
	c := &Caller{allowed: map[string]bool{"t.v1.S/List": true}}
	if _, err := c.Get(context.Background(), "t.v1.S/Create", nil); err == nil {
		t.Fatal("Caller llamó a un método que la herramienta no declaró")
	}
}
