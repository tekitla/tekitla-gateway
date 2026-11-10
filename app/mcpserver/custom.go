package mcpserver

// Herramientas propias del repo (no vienen de la plantilla): las que no salen
// de un solo método de routes.yaml. Una herramienta propia puede llamar a
// varias rutas, fijar parámetros o armar su propia respuesta, pero cada
// llamada sigue entrando al mux como un request REST: auth, roles, rate
// limit, deadline y errores son los de esa ruta. No hay un segundo camino
// hacia los servicios.
//
// Si el nombre coincide con un `mcp.tool` de routes.yaml, la herramienta
// propia reemplaza a la genérica y toma su descripción de routes.yaml (la
// declaración queda en un solo lugar). Si no, trae su propia Description.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tekitla/tekitla-gateway/app/apierror"
	"github.com/tekitla/tekitla-gateway/app/auth"
	"github.com/tekitla/tekitla-gateway/app/reqctx"
	"github.com/tekitla/tekitla-gateway/app/routes"
)

// CustomTool es una herramienta propia del repo.
type CustomTool struct {
	Name string
	// Description: solo si Name no está en routes.yaml (si está, sale de ahí).
	Description string
	InputSchema map[string]any
	// Methods: rutas de routes.yaml que llama (<paquete>.<Servicio>/<Método>).
	// Tienen que estar declaradas, sin deny y con google.api.http. Si todas
	// son GET, la herramienta se anota como de solo lectura.
	Methods []string
	// Hint (opcional): qué hacer con un error de la API, por code (va en el
	// campo hint del resultado para que el agente corrija sin adivinar).
	Hint func(code string) string
	// Handle hace el trabajo. El resultado se devuelve como JSON. Un error
	// *CallError (una ruta respondió fuera de 2xx) o *ToolError se devuelve
	// como resultado con IsError; cualquier otro, como INTERNAL genérico.
	Handle func(ctx context.Context, c *Caller, args map[string]any) (any, error)
}

// ToolError es un error de la herramienta en sí (argumentos, datos que no
// están): sale con el mismo formato que los errores de la API.
type ToolError struct {
	Code    string
	Reason  string
	Message string
}

func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

// CallError: una ruta llamada por la herramienta respondió fuera de 2xx.
// Body es el error de la API tal cual (code, message, reason, request_id).
type CallError struct {
	Method string
	Status int
	Body   apierror.Body
}

func (e *CallError) Error() string {
	return fmt.Sprintf("%s: HTTP %d %s", e.Method, e.Status, e.Body.Error.Code)
}

// maxInternalBytes: tope de cada respuesta que lee una herramienta propia
// (p. ej. 100 issues con descripción completa). Lo que llega al agente sigue
// limitado a maxResultBytes.
const maxInternalBytes = 8 << 20

// Caller llama a las rutas declaradas de una herramienta propia con la
// credencial del cliente.
type Caller struct {
	mux      http.Handler
	header   http.Header
	bindings map[string]routes.Binding
	allowed  map[string]bool
	// identity: la de la última llamada (para la auditoría).
	identity *auth.Identity
}

// Get llama a method (GET) con args repartidos entre path y query como lo
// hace una herramienta genérica, y devuelve el cuerpo de la respuesta.
// Los valores de args son string, bool, json.Number, []any de esos o
// map[string]any (mensajes anidados, p. ej. page.limit).
func (c *Caller) Get(ctx context.Context, method string, args map[string]any) ([]byte, error) {
	if !c.allowed[method] {
		return nil, fmt.Errorf("mcp: la herramienta no declaró %s en Methods", method)
	}
	b := c.bindings[method]
	call, err := buildCall(b.Template, b.Body, args)
	if err != nil {
		return nil, &ToolError{Code: "INVALID_ARGUMENT", Message: err.Error()}
	}
	status, body, inner := (&tool{binding: b, limit: maxInternalBytes}).dispatch(ctx, c.mux, c.header, call)
	if inner.Identity != nil {
		c.identity = inner.Identity
	}
	if body == nil {
		return nil, &ToolError{Code: "RESOURCE_EXHAUSTED", Message: fmt.Sprintf("una respuesta interna supera %d MiB", maxInternalBytes>>20)}
	}
	if status < 200 || status > 299 {
		ce := &CallError{Method: method, Status: status}
		if err := json.Unmarshal(body, &ce.Body); err != nil || ce.Body.Error.Code == "" {
			ce.Body.Error = apierror.Detail{Code: "UNKNOWN", Message: "respuesta de error sin formato", RequestID: reqctx.RequestID(ctx)}
		}
		return nil, ce
	}
	return body, nil
}

// addCustom registra las herramientas propias. described: descripción de
// routes.yaml por nombre de herramienta (las que la propia reemplaza).
func addCustom(srv *mcp.Server, o Options, described map[string]string) (map[string]bool, error) {
	byMethod := map[string]routes.Binding{}
	for _, b := range o.Bindings {
		if _, dup := byMethod[b.Method]; !dup {
			byMethod[b.Method] = b
		}
	}
	replaced := map[string]bool{}
	var errs []error
	for _, ct := range o.Custom {
		desc, inRoutes := described[ct.Name]
		switch {
		case ct.Handle == nil || ct.InputSchema == nil || len(ct.Methods) == 0:
			errs = append(errs, fmt.Errorf("mcp: herramienta propia %q incompleta (Handle, InputSchema y Methods)", ct.Name))
			continue
		case replaced[ct.Name]:
			errs = append(errs, fmt.Errorf("mcp: herramienta propia %q repetida", ct.Name))
			continue
		case inRoutes && ct.Description != "":
			errs = append(errs, fmt.Errorf("mcp: %q ya tiene descripción en routes.yaml; sacala del código", ct.Name))
			continue
		case !inRoutes:
			desc = strings.TrimSpace(ct.Description)
			if desc == "" || len([]rune(desc)) > 300 {
				errs = append(errs, fmt.Errorf("mcp: %q sin ruta en routes.yaml: Description obligatoria y de hasta 300 caracteres", ct.Name))
				continue
			}
		}
		readOnly := true
		allowed := map[string]bool{}
		for _, m := range ct.Methods {
			p := o.Routes.Policy(m)
			b, ok := byMethod[m]
			switch {
			case p == nil:
				errs = append(errs, fmt.Errorf("mcp: %q usa %s, que no está en routes.yaml", ct.Name, m))
			case p.Deny:
				errs = append(errs, fmt.Errorf("mcp: %q usa %s, que está en deny", ct.Name, m))
			case !ok:
				errs = append(errs, fmt.Errorf("mcp: %q usa %s, sin google.api.http en un servicio registrado", ct.Name, m))
			default:
				if err := checkTemplate(b.Template); err != nil {
					errs = append(errs, fmt.Errorf("mcp: %q usa %s: %w", ct.Name, m, err))
				}
				if b.Verb != http.MethodGet {
					readOnly = false
				}
				allowed[m] = true
			}
		}
		if !readOnly {
			// Caller solo sabe hacer GET: una herramienta propia que escribe
			// necesitaría su propio diseño (confirmación, auditoría de escritura).
			errs = append(errs, fmt.Errorf("mcp: %q usa rutas que no son GET; las herramientas propias son de solo lectura", ct.Name))
		}
		replaced[ct.Name] = true
		srv.AddTool(&mcp.Tool{
			Name:        ct.Name,
			Description: desc,
			InputSchema: ct.InputSchema,
			Annotations: annotations(http.MethodGet),
		}, customHandler(o, ct, byMethod, allowed))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return replaced, nil
}

func customHandler(o Options, ct CustomTool, bindings map[string]routes.Binding, allowed map[string]bool) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()
		args := map[string]any{}
		if raw := req.Params.Arguments; len(raw) > 0 && string(raw) != "null" {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			if err := dec.Decode(&args); err != nil {
				return errorResult(ctx, "INVALID_ARGUMENT", "los argumentos tienen que ser un objeto JSON"), nil
			}
		}
		var header http.Header
		if req.Extra != nil {
			header = req.Extra.Header
		}
		c := &Caller{mux: o.Dispatch, header: header, bindings: bindings, allowed: allowed}
		out, err := ct.Handle(ctx, c, args)

		res, status := customResult(ctx, o, ct, out, err)
		// Auditoría (docs/15 §8): una línea por llamada, con actor, agente y resultado.
		attrs := []any{
			slog.String("request_id", reqctx.RequestID(ctx)),
			slog.String("tool", ct.Name),
			slog.Int("status", status),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
		}
		// El actor es el de /mcp (guard lo deja en reqctx), también si la
		// herramienta falla antes de llamar a una ruta.
		id := c.identity
		if info := reqctx.From(ctx); id == nil && info != nil {
			id = info.Identity
		}
		if id != nil {
			attrs = append(attrs, slog.String("user_id", id.Subject))
			if id.AgentID != "" {
				attrs = append(attrs, slog.String("agent_id", id.AgentID))
			}
		}
		o.Logger.InfoContext(ctx, "mcp tool", attrs...)
		return res, nil
	}
}

// customResult arma el resultado y el status para la auditoría.
func customResult(ctx context.Context, o Options, ct CustomTool, out any, err error) (*mcp.CallToolResult, int) {
	name := ct.Name
	hint := func(code string) string {
		if ct.Hint == nil {
			return ""
		}
		return ct.Hint(code)
	}
	var ce *CallError
	var te *ToolError
	switch {
	case err == nil:
		b, merr := json.Marshal(out)
		if merr != nil {
			o.Logger.ErrorContext(ctx, "mcp: resultado sin JSON", slog.String("tool", name), slog.String("error", merr.Error()))
			return errorResult(ctx, "INTERNAL", "error interno"), http.StatusInternalServerError
		}
		if len(b) > maxResultBytes {
			return errorResult(ctx, "RESOURCE_EXHAUSTED", fmt.Sprintf("la respuesta supera %d KiB; pedí menos resultados", maxResultBytes>>10)), http.StatusOK
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, http.StatusOK
	case errors.As(err, &ce):
		return hinted(ce.Body.Error, hint(ce.Body.Error.Code)), ce.Status
	case errors.As(err, &te):
		d := apierror.Detail{Code: te.Code, Message: te.Message, Reason: te.Reason, RequestID: reqctx.RequestID(ctx)}
		return hinted(d, ""), statusOf(te.Code)
	default:
		o.Logger.ErrorContext(ctx, "mcp: herramienta propia", slog.String("tool", name), slog.String("error", err.Error()))
		return errorResult(ctx, "INTERNAL", "error interno"), http.StatusInternalServerError
	}
}

// statusOf: status HTTP de un ToolError, para la auditoría.
func statusOf(code string) int {
	switch code {
	case "NOT_FOUND":
		return http.StatusNotFound
	case "RESOURCE_EXHAUSTED":
		return http.StatusTooManyRequests
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "UNAUTHENTICATED":
		return http.StatusUnauthorized
	case "INTERNAL", "UNKNOWN":
		return http.StatusInternalServerError
	}
	return http.StatusBadRequest
}

type hintedBody struct {
	Error hintedDetail `json:"error"`
}

type hintedDetail struct {
	apierror.Detail
	Hint string `json:"hint,omitempty"`
}

func hinted(d apierror.Detail, h string) *mcp.CallToolResult {
	b, _ := json.Marshal(hintedBody{Error: hintedDetail{Detail: d, Hint: h}})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, IsError: true}
}
