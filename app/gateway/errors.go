package gateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/grpc-ecosystem/grpc-gateway/v2/runtime"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/tekitla/tekitla-gateway/app/apierror"
	"github.com/tekitla/tekitla-gateway/app/reqctx"
)

// clientCodes: códigos cuyo mensaje es para el cliente (errores de dominio
// de los servicios: validación, no encontrado, conflicto...). El resto sale
// con un mensaje genérico: nunca se reenvía el texto de un error interno.
var clientCodes = map[codes.Code]bool{
	codes.InvalidArgument:    true,
	codes.NotFound:           true,
	codes.AlreadyExists:      true,
	codes.PermissionDenied:   true,
	codes.Unauthenticated:    true,
	codes.FailedPrecondition: true,
	codes.OutOfRange:         true,
	codes.ResourceExhausted:  true,
	codes.Aborted:            true,
}

var genericMessages = map[codes.Code]string{
	codes.Canceled:         "request cancelado",
	codes.DeadlineExceeded: "el servicio tardó demasiado en responder",
	codes.Unimplemented:    "operación no disponible",
	codes.Unavailable:      "servicio no disponible; reintentá en unos segundos",
}

const maxClientMessage = 300

// maxReason: un reason más largo (o que no sea UPPER_SNAKE) no se reenvía.
const maxReason = 64

// errorHandler: errores de la llamada gRPC (o de grpc-gateway al armar el
// request) -> formato único. Los 5xx van al log con el detalle real y el
// request_id; al cliente, mensaje genérico + request_id.
func (g *gw) errorHandler(ctx context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, r *http.Request, err error) {
	httpStatus := 0
	var hse *runtime.HTTPStatusError
	if errors.As(err, &hse) {
		httpStatus, err = hse.HTTPStatus, hse.Err
	}
	st := status.Convert(err)
	code := st.Code()
	if httpStatus == 0 {
		httpStatus = runtime.HTTPStatusFromCode(code)
	}

	msg, reason := genericMessages[code], ""
	if clientCodes[code] && httpStatus < 500 {
		reason = reasonOf(st)
		msg = st.Message()
		if len(msg) > maxClientMessage {
			msg = msg[:maxClientMessage]
		}
	}
	if msg == "" {
		msg = "error interno"
	}

	if httpStatus >= 500 {
		g.log.ErrorContext(ctx, "error de upstream",
			slog.String("request_id", reqctx.RequestID(r.Context())),
			slog.String("route", routeOf(r)),
			slog.String("grpc_code", code.String()),
			slog.String("grpc_message", st.Message()),
		)
	}
	apierror.WriteReason(w, r, httpStatus, codeName(code), msg, reason)
}

// reasonOf: el Reason del primer google.rpc.ErrorInfo del status, si es
// UPPER_SNAKE y corto. Domain y Metadata no salen: pueden llevar datos
// internos del servicio (OWASP API3).
func reasonOf(st *status.Status) string {
	for _, d := range st.Details() {
		info, ok := d.(*errdetails.ErrorInfo)
		if !ok {
			continue
		}
		if r := info.GetReason(); validReason(r) {
			return r
		}
		return ""
	}
	return ""
}

func validReason(r string) bool {
	if r == "" || len(r) > maxReason || r[0] < 'A' || r[0] > 'Z' {
		return false
	}
	for i := 0; i < len(r); i++ {
		if c := r[i]; !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// streamErrorHandler: errores a mitad de un server streaming.
func (g *gw) streamErrorHandler(ctx context.Context, err error) *status.Status {
	st := status.Convert(err)
	if clientCodes[st.Code()] {
		return st
	}
	g.log.ErrorContext(ctx, "error en stream", slog.String("request_id", reqctx.RequestID(ctx)),
		slog.String("grpc_code", st.Code().String()), slog.String("grpc_message", st.Message()))
	return status.New(st.Code(), "error interno")
}

// routingErrorHandler: sin patrón que matchee. 405 sale como 404 para no
// revelar qué rutas existen con otro verbo.
func (g *gw) routingErrorHandler(_ context.Context, _ *runtime.ServeMux, _ runtime.Marshaler, w http.ResponseWriter, r *http.Request, httpStatus int) {
	if httpStatus == http.StatusBadRequest {
		apierror.Write(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "request inválido")
		return
	}
	notFound(w, r)
}

func routeOf(r *http.Request) string {
	if info := reqctx.From(r.Context()); info != nil {
		return info.Method
	}
	return ""
}

// codeName: codes.PermissionDenied -> "PERMISSION_DENIED".
func codeName(c codes.Code) string {
	if c == codes.OK {
		return "OK"
	}
	var sb strings.Builder
	for i, ch := range c.String() {
		if i > 0 && unicode.IsUpper(ch) {
			sb.WriteByte('_')
		}
		sb.WriteRune(unicode.ToUpper(ch))
	}
	return sb.String()
}
