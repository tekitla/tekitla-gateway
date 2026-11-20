package gateway

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/apierror"
	"github.com/tekitla/tekitla-gateway/app/ratelimit"
	"github.com/tekitla/tekitla-gateway/app/reqctx"
)

// Webhooks de GitHub y GitLab (issue 016). No son rutas de routes.yaml:
// grpc-gateway parsea el JSON a proto (estricto) y la firma de GitHub es un
// HMAC sobre los bytes exactos del cuerpo. Este handler hace solo el
// transporte y le pasa el cuerpo crudo al RPC interno
// issues.v1.WebhookService/ReceiveWebhook (sin google.api.http: no hay ruta
// REST para llamarlo). La firma o el secreto los verifica tekitla-issues: el
// gateway no tiene secretos de terceros.
//
// Sin credenciales de Tekitla (no lee Authorization), sin CORS (no lo llama un
// navegador), con su propio bucket por IP, 1 MiB y 5 s.

// Paths de los webhooks. Inventario: docs/rutas.md.
const (
	WebhookPathGitHub = "/v1/webhooks/github"
	WebhookPathGitLab = "/v1/webhooks/gitlab"
)

const (
	webhookMaxBody = 1 << 20
	webhookTimeout = 5 * time.Second
)

// webhookRate es el bucket por IP de cada proveedor. GitHub entrega todo
// desde pocas IPs (y no reintenta solo un 429), así que es alto: es contra
// abuso, no contra GitHub. La App solo manda los cuatro eventos suscritos
// (push, pull_request, pull_request_review, check_suite), no check_run ni
// workflow_job. Si aun así se pierde una entrega, «Sincronizar» la recupera.
var webhookRate = ratelimit.Rate{PerMinute: 600, Burst: 300}

// webhookHeaders son las únicas cabeceras que se copian, por proveedor. El
// resto se descarta.
type webhookHeaders struct {
	provider issuesv1.RepoProvider
	event    string
	delivery string
	// deliveryFirst, si viene, gana sobre delivery: GitLab manda
	// Idempotency-Key igual en los reintentos de una entrega (17.4+), y
	// X-Gitlab-Event-UUID en versiones anteriores.
	deliveryFirst string
	signature     string // GitHub
	token         string // GitLab
}

var webhookProviders = map[string]webhookHeaders{
	WebhookPathGitHub: {
		provider: issuesv1.RepoProvider_REPO_PROVIDER_GITHUB,
		event:    "X-GitHub-Event", delivery: "X-GitHub-Delivery", signature: "X-Hub-Signature-256",
	},
	WebhookPathGitLab: {
		provider: issuesv1.RepoProvider_REPO_PROVIDER_GITLAB,
		event:    "X-Gitlab-Event", delivery: "X-Gitlab-Event-UUID", deliveryFirst: "Idempotency-Key",
		token: "X-Gitlab-Token",
	},
}

// headerMax acota cada cabecera copiada (el servidor ya limita el total a
// 32 KiB).
const headerMax = 256

type webhookHandler struct {
	log     *slog.Logger
	client  issuesv1.WebhookServiceClient
	limiter ratelimit.Limiter
}

// withWebhooks atiende los paths de webhooks antes del mux.
func withWebhooks(h *webhookHandler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hdr, ok := webhookProviders[r.URL.Path]; ok {
			h.serve(w, r, hdr)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *webhookHandler) serve(w http.ResponseWriter, r *http.Request, hdr webhookHeaders) {
	if r.Method != http.MethodPost {
		// Como el resto del gateway: un verbo que no existe es 404.
		apierror.Write(w, r, http.StatusNotFound, "NOT_FOUND", "ruta no encontrada")
		return
	}
	if r.ContentLength > webhookMaxBody {
		apierror.Write(w, r, http.StatusRequestEntityTooLarge, apierror.CodePayloadTooLarge, "el cuerpo supera el máximo permitido")
		return
	}
	clientIP := ""
	if info := reqctx.From(r.Context()); info != nil {
		clientIP = info.ClientIP
	}
	d := h.limiter.Allow("webhook:"+r.URL.Path+"|"+ipKey(clientIP), webhookRate)
	if !d.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, ceilSeconds(d.RetryAfter))))
		apierror.Write(w, r, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "demasiados requests; reintentá más tarde")
		return
	}
	if !isJSON(r.Header.Get("Content-Type")) {
		apierror.Write(w, r, http.StatusUnsupportedMediaType, apierror.CodeUnsupportedMediaType, "Content-Type tiene que ser application/json")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxBody+1))
	if err != nil {
		apierror.Write(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "no se pudo leer el cuerpo")
		return
	}
	if len(body) > webhookMaxBody {
		apierror.Write(w, r, http.StatusRequestEntityTooLarge, apierror.CodePayloadTooLarge, "el cuerpo supera el máximo permitido")
		return
	}
	get := func(name string) string {
		if name == "" {
			return ""
		}
		v := r.Header.Get(name)
		if len(v) > headerMax {
			return ""
		}
		return v
	}
	delivery := get(hdr.deliveryFirst)
	if delivery == "" {
		delivery = get(hdr.delivery)
	}
	req := &issuesv1.ReceiveWebhookRequest{
		Provider:   hdr.provider,
		Event:      get(hdr.event),
		DeliveryId: delivery,
		Signature:  get(hdr.signature),
		Token:      get(hdr.token),
		Body:       bytes.Clone(body),
	}
	// Llamada interna: sin identidad (el servicio no la lee), con el
	// request id para cruzar los logs.
	ctx, cancel := context.WithTimeout(reqctx.Internal(r.Context()), webhookTimeout)
	defer cancel()
	resp, err := h.client.ReceiveWebhook(ctx, req)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	switch resp.GetOutcome() {
	case issuesv1.WebhookOutcome_WEBHOOK_OUTCOME_IGNORED:
		writeJSON(w, http.StatusAccepted, `{"status":"ignored"}`)
	case issuesv1.WebhookOutcome_WEBHOOK_OUTCOME_DUPLICATE:
		writeJSON(w, http.StatusOK, `{"status":"duplicate"}`)
	case issuesv1.WebhookOutcome_WEBHOOK_OUTCOME_PROCESSED:
		writeJSON(w, http.StatusOK, `{"status":"processed"}`)
	default:
		// Sin resultado (o uno nuevo que este gateway no conoce): que la
		// entrega figure como fallida y se vea.
		h.log.ErrorContext(r.Context(), "webhook: resultado desconocido", slog.String("outcome", resp.GetOutcome().String()))
		apierror.Write(w, r, http.StatusInternalServerError, "INTERNAL", "error interno")
	}
}

// fail traduce el error del servicio sin dar detalles: un 401 no dice si
// faltaba la firma, era de otro secreto o el cuerpo cambió.
func (h *webhookHandler) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch status.Code(err) {
	case codes.Unauthenticated:
		apierror.Write(w, r, http.StatusUnauthorized, "UNAUTHENTICATED", "firma inválida")
	case codes.InvalidArgument:
		apierror.Write(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "entrega inválida")
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		// Canceled: el proveedor cortó la conexión; no es un error del servicio.
		apierror.Write(w, r, http.StatusServiceUnavailable, "UNAVAILABLE", "servicio no disponible")
	case codes.ResourceExhausted:
		apierror.Write(w, r, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "demasiados requests; reintentá más tarde")
	case codes.NotFound, codes.FailedPrecondition, codes.AlreadyExists:
		apierror.Write(w, r, http.StatusBadRequest, "INVALID_ARGUMENT", "entrega inválida")
	default:
		h.log.ErrorContext(r.Context(), "webhook: error del servicio", slog.String("code", status.Code(err).String()))
		apierror.Write(w, r, http.StatusInternalServerError, "INTERNAL", "error interno")
	}
}
