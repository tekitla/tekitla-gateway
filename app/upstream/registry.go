package upstream

import (
	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
)

// Services es la ÚNICA lista de servicios gRPC que expone el gateway.
//
// Para agregar uno (ver AGENTS.md, "Agregar una ruta"):
//  1. El .proto del servicio tiene anotaciones google.api.http y el repo de
//     protos generó su <archivo>.pb.gw.go.
//  2. Una entrada acá: Name (define la variable UPSTREAM_<NAME>_ADDR),
//     GRPCService (nombre completo) y Register (RegisterXxxHandler generado).
//  3. Sus métodos en routes.yaml (si no, quedan en 404) y go test ./...
//
// Varios servicios gRPC pueden compartir upstream (mismo Name): se usa una
// sola conexión.
var Services = []Service{
	{
		// tekitla-issues: proyectos e issues (issue 002, rebanada 1).
		Name:        "issues",
		GRPCService: "issues.v1.IssueService",
		Register:    issuesv1.RegisterIssueServiceHandler,
	},
	{
		// Agentes y tokens de la persona (issue 025): mismo upstream.
		Name:        "issues",
		GRPCService: "issues.v1.ActorService",
		Register:    issuesv1.RegisterActorServiceHandler,
	},
	{
		// Enlaces de solo lectura (issue 009): gestión, autenticada.
		Name:        "issues",
		GRPCService: "issues.v1.ShareLinkService",
		Register:    issuesv1.RegisterShareLinkServiceHandler,
	},
	{
		// Enlaces de solo lectura (issue 009): lectura pública por token.
		Name:        "issues",
		GRPCService: "issues.v1.SharedProjectService",
		Register:    issuesv1.RegisterSharedProjectServiceHandler,
	},
	{
		// Sesiones de agentes (issue 026): mismo upstream.
		Name:        "issues",
		GRPCService: "issues.v1.AgentSessionService",
		Register:    issuesv1.RegisterAgentSessionServiceHandler,
	},
}
