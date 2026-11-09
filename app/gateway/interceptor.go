package gateway

import (
	"context"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/tekitla/tekitla-gateway/app/reqctx"
)

// Metadata gRPC que el gateway manda a los servicios. Es lo ÚNICO que reciben:
// ninguna cabecera del cliente (tampoco Authorization) pasa tal cual.
const (
	MDRequestID = "x-request-id"
	MDUserID    = "x-user-id"
	MDAgentID   = "x-agent-id" // solo con un token de agente (issue 025)
	MDRoles     = "x-roles"    // separados por coma
	MDAuthType  = "x-auth-type"
	MDClientIP  = "x-client-ip"
)

// DialOptions son las opciones obligatorias de toda conexión a un upstream.
func DialOptions() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithUnaryInterceptor(unaryInterceptor),
		grpc.WithStreamInterceptor(streamInterceptor),
	}
}

func unaryInterceptor(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx, err := outgoing(ctx, method)
	if err != nil {
		return err
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

func streamInterceptor(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, err := outgoing(ctx, method)
	if err != nil {
		return nil, err
	}
	return streamer(ctx, desc, cc, method, opts...)
}

// outgoing reemplaza la metadata saliente por una armada desde cero y
// verifica que el método llamado sea el que autorizó routeMiddleware
// (defensa en profundidad: si el mapeo ruta -> método fallara, la llamada no
// sale).
func outgoing(ctx context.Context, method string) (context.Context, error) {
	if reqctx.IsInternal(ctx) {
		// Llamada del propio gateway: sin identidad, pero con el request id
		// (si hay request) para cruzar los logs con el servicio.
		md := metadata.MD{}
		if id := reqctx.RequestID(ctx); id != "" {
			md.Set(MDRequestID, id)
		}
		return metadata.NewOutgoingContext(ctx, md), nil
	}
	info := reqctx.From(ctx)
	if info == nil || info.Method == "" || "/"+info.Method != method {
		return nil, status.Error(codes.Internal, "llamada gRPC fuera de una ruta autorizada")
	}
	md := metadata.MD{}
	md.Set(MDRequestID, info.RequestID)
	if info.ClientIP != "" {
		md.Set(MDClientIP, info.ClientIP)
	}
	if id := info.Identity; id != nil {
		md.Set(MDUserID, id.Subject)
		md.Set(MDRoles, strings.Join(id.Roles, ","))
		md.Set(MDAuthType, id.Kind)
		if id.AgentID != "" {
			md.Set(MDAgentID, id.AgentID)
		}
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}
