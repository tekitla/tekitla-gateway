// Package mcptools: las herramientas MCP de Tekitla (issue 030, fase 1 del
// MCP; nombres de docs/03 §6 de dev-plantillas). Todas de solo lectura.
//
// No salen de un solo método de routes.yaml (la plantilla no deja fijar
// parámetros ni componer RPCs, y el contrato pide el id del proyecto, no su
// clave), así que son herramientas propias (app/mcpserver/custom.go). Cada
// RPC que usan entra al mux como un request REST con la credencial del
// cliente: la política (roles allowlist o issues:read, rate limit, deadline)
// es la de su ruta en routes.yaml. list_projects, list_issues y get_issue
// toman nombre y descripción de routes.yaml; next_issue, que usa ListIssues
// con filtros fijos, trae los suyos acá.
//
// Las herramientas devuelven datos, no instrucciones: el contenido de un
// issue lo escribe cualquiera con acceso al proyecto.
package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tekitla/tekitla-gateway/app/mcpserver"
)

// Métodos de routes.yaml que usan las herramientas.
const (
	mListProjects = "issues.v1.IssueService/ListProjects"
	mListIssues   = "issues.v1.IssueService/ListIssues"
	mGetIssue     = "issues.v1.IssueService/GetIssue"
	mListEvents   = "issues.v1.IssueService/ListIssueEvents"
	mListStatuses = "issues.v1.IssueService/ListStatuses"
	mListLabels   = "issues.v1.IssueService/ListLabels"
)

const (
	pageMax = 100 // MaxPageLimit de tekitla-issues
	// Topes de páginas internas por llamada: cada una cuenta para el rate
	// limit de su ruta (ListIssues: ráfaga de 60; las demás lecturas, 100).
	maxProjectPages  = 10 // ListProjects: 1000 proyectos
	countPageBudget  = 20 // ListIssues de list_projects, entre todos los proyectos
	maxEventPages    = 10 // ListIssueEvents de get_issue: 1000 eventos
	defaultListLimit = 20
	defaultEvents    = 10
	maxEvents        = 50
	maxCommentRunes  = 500
)

// NextIssueDescription: next_issue no tiene ruta propia en routes.yaml (usa
// ListIssues, que ya es list_issues), así que su descripción vive acá.
const NextIssueDescription = "El siguiente issue libre de un proyecto: «por hacer», sin asignar, de mayor prioridad y después por rank. No lo toma ni lo cambia. project: clave (RND). Devuelve issue: null si no hay ninguno."

// Tools devuelve las herramientas de Tekitla para gateway.Options.MCPTools.
func Tools() []mcpserver.CustomTool {
	return []mcpserver.CustomTool{
		{
			Name:        "list_projects",
			InputSchema: object(nil),
			Methods:     []string{mListProjects, mListIssues},
			Handle:      listProjects,
			Hint:        hint,
		},
		{
			Name: "list_issues",
			InputSchema: object(map[string]any{
				"project":  str("Clave del proyecto, p. ej. RND"),
				"category": enum("Categoría del estado", categoryNames...),
				"status":   str("Nombre del estado (columna) del proyecto"),
				"priority": map[string]any{"type": "array", "items": enum("", priorityNames...), "description": "Una o varias prioridades"},
				"assignee": str("Id del actor asignado, o \"none\" para los sin asignar"),
				"query":    str("Texto en el título, o una clave (RND-12)"),
				"cursor":   str("next_cursor de la página anterior, con los mismos filtros"),
				"limit":    map[string]any{"type": "integer", "minimum": 1, "maximum": pageMax, "description": "Por página (20 por defecto)"},
			}, "project"),
			Methods: []string{mListProjects, mListIssues, mListStatuses, mListLabels},
			Handle:  listIssues,
			Hint:    hint,
		},
		{
			Name: "get_issue",
			InputSchema: object(map[string]any{
				"key":    str("Clave del issue, p. ej. RND-236"),
				"events": map[string]any{"type": "integer", "minimum": 0, "maximum": maxEvents, "description": "Últimos eventos del historial (10 por defecto)"},
			}, "key"),
			Methods: []string{mListProjects, mGetIssue, mListEvents, mListStatuses, mListLabels},
			Handle:  getIssue,
			Hint:    hint,
		},
		{
			Name:        "next_issue",
			Description: NextIssueDescription,
			InputSchema: object(map[string]any{"project": str("Clave del proyecto, p. ej. RND")}, "project"),
			Methods:     []string{mListProjects, mListIssues, mListStatuses, mListLabels},
			Handle:      nextIssue,
			Hint:        hint,
		},
	}
}

// ---------------------------------------------------------------------------
// Herramientas
// ---------------------------------------------------------------------------

type projectOut struct {
	Key    string         `json:"key"`
	Name   string         `json:"name"`
	Counts map[string]int `json:"counts"`
	// CountsPartial: no se llegaron a contar todos sus issues (el contrato
	// no tiene un conteo y se recorren con un tope); son un mínimo.
	CountsPartial bool `json:"counts_partial,omitempty"`
}

type projectsOut struct {
	Projects []projectOut `json:"projects"`
	// Partial: hay más proyectos de los que se listan (maxProjectPages).
	Partial bool `json:"projects_partial,omitempty"`
}

func listProjects(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args); err != nil {
		return nil, err
	}
	projects, partial, err := allProjects(ctx, c)
	if err != nil {
		return nil, err
	}
	out := projectsOut{Projects: make([]projectOut, 0, len(projects)), Partial: partial}
	budget := countPageBudget
	for _, p := range projects {
		po := projectOut{Key: p.GetKey(), Name: p.GetName(), Counts: map[string]int{}}
		for _, n := range categoryNames {
			po.Counts[n] = 0
		}
		cursor := ""
		for {
			if budget == 0 {
				po.CountsPartial = true
				break
			}
			budget--
			var resp issuesv1.ListIssuesResponse
			if err := get(ctx, c, mListIssues, map[string]any{"project_id": p.GetId(), "page": page(cursor, pageMax)}, &resp); err != nil {
				return nil, err
			}
			for _, is := range resp.GetIssues() {
				if n := categoryName(is.GetState()); n != "" {
					po.Counts[n]++
				}
			}
			if !resp.GetPage().GetHasNext() {
				break
			}
			cursor = resp.GetPage().GetNextCursor()
		}
		out.Projects = append(out.Projects, po)
	}
	return out, nil
}

type issueOut struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Category string   `json:"category"`
	Status   string   `json:"status,omitempty"`
	Priority string   `json:"priority"`
	Labels   []string `json:"labels,omitempty"`
	Assignee string   `json:"assignee,omitempty"` // id del actor; vacío = sin asignar
}

type listOut struct {
	Issues     []issueOut `json:"issues"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

func listIssues(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args, "project", "category", "status", "priority", "assignee", "query", "cursor", "limit"); err != nil {
		return nil, err
	}
	// Primero todos los argumentos: un error no gasta llamadas al servicio.
	key, err := strArg(args, "project", true)
	if err != nil {
		return nil, err
	}
	limit, err := intArg(args, "limit", defaultListLimit, 1, pageMax)
	if err != nil {
		return nil, err
	}
	cursor, err := strArg(args, "cursor", false)
	if err != nil {
		return nil, err
	}
	q := map[string]any{"page": page(cursor, limit)}
	cat, err := strArg(args, "category", false)
	if err != nil {
		return nil, err
	}
	if cat != "" {
		st, ok := categoryByName[cat]
		if !ok {
			return nil, invalid("category: una de %s", strings.Join(categoryNames, ", "))
		}
		q["state"] = st.String()
	}
	status, err := strArg(args, "status", false)
	if err != nil {
		return nil, err
	}
	if pr, ok := args["priority"]; ok {
		list, isList := pr.([]any)
		if s, isStr := pr.(string); isStr {
			list, isList = []any{s}, true
		}
		if !isList {
			return nil, invalid("priority: lista de %s", strings.Join(priorityNames, ", "))
		}
		var prs []any
		for _, v := range list {
			s, _ := v.(string)
			pv, ok := priorityByName[s]
			if !ok {
				return nil, invalid("priority: lista de %s", strings.Join(priorityNames, ", "))
			}
			prs = append(prs, pv.String())
		}
		if len(prs) > 0 {
			q["priorities"] = prs
		}
	}
	as, err := strArg(args, "assignee", false)
	if err != nil {
		return nil, err
	}
	if as == "none" {
		q["unassigned"] = true
	} else if as != "" {
		q["assignee_actor_ids"] = []any{as}
	}
	query, err := strArg(args, "query", false)
	if err != nil {
		return nil, err
	}
	if query != "" {
		q["query"] = query
	}

	p, err := findProject(ctx, c, key)
	if err != nil {
		return nil, err
	}
	q["project_id"] = p.GetId()
	names, err := loadNames(ctx, c, p.GetId())
	if err != nil {
		return nil, err
	}
	if status != "" {
		id, ok := names.statusID(status)
		if !ok {
			return nil, invalid("status: %q no es un estado de %s; los estados son: %s", status, p.GetKey(), strings.Join(names.statusNames(), ", "))
		}
		q["status_ids"] = []any{id}
	}

	var resp issuesv1.ListIssuesResponse
	if err := get(ctx, c, mListIssues, q, &resp); err != nil {
		return nil, err
	}
	out := listOut{Issues: make([]issueOut, 0, len(resp.GetIssues()))}
	for _, is := range resp.GetIssues() {
		out.Issues = append(out.Issues, summary(p.GetKey(), is, names))
	}
	if resp.GetPage().GetHasNext() {
		out.NextCursor = resp.GetPage().GetNextCursor()
	}
	return out, nil
}

func nextIssue(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args, "project"); err != nil {
		return nil, err
	}
	key, err := strArg(args, "project", true)
	if err != nil {
		return nil, err
	}
	p, err := findProject(ctx, c, key)
	if err != nil {
		return nil, err
	}
	// Por hacer, sin asignar, prioridad descendente (los empates, por rank:
	// lo garantiza ListIssues). Solo lee: no asigna ni mueve.
	var resp issuesv1.ListIssuesResponse
	if err := get(ctx, c, mListIssues, map[string]any{
		"project_id": p.GetId(),
		"state":      issuesv1.IssueState_ISSUE_STATE_TODO.String(),
		"unassigned": true,
		"order_by":   issuesv1.IssueOrder_ISSUE_ORDER_PRIORITY_DESC.String(),
		"page":       page("", 1),
	}, &resp); err != nil {
		return nil, err
	}
	if len(resp.GetIssues()) == 0 {
		return map[string]any{"issue": nil}, nil
	}
	names, err := loadNames(ctx, c, p.GetId())
	if err != nil {
		return nil, err
	}
	return map[string]any{"issue": summary(p.GetKey(), resp.GetIssues()[0], names)}, nil
}

type eventOut struct {
	At     string `json:"at"`
	Actor  string `json:"actor"`
	Agent  string `json:"agent,omitempty"`
	Change string `json:"change"`
}

type issueDetail struct {
	issueOut
	Description   string     `json:"description"`
	DiscardReason string     `json:"discard_reason,omitempty"`
	SourceKey     string     `json:"source_key,omitempty"`
	CreatedAt     string     `json:"created_at"`
	UpdatedAt     string     `json:"updated_at"`
	Events        []eventOut `json:"events"`
	// EventsTotal: largo del historial (con events=0 no se lee y no viene).
	EventsTotal int `json:"events_total,omitempty"`
	// EventsPartial: el historial es más largo de lo que se recorre
	// (maxEventPages); los eventos que se muestran no son los últimos.
	EventsPartial bool `json:"events_partial,omitempty"`
}

var issueKeyRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9]{1,9})-([1-9][0-9]{0,8})$`)

func getIssue(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args, "key", "events"); err != nil {
		return nil, err
	}
	key, err := strArg(args, "key", true)
	if err != nil {
		return nil, err
	}
	m := issueKeyRe.FindStringSubmatch(strings.TrimSpace(key))
	if m == nil {
		return nil, invalid("key: clave de issue como RND-236")
	}
	nEvents, err := intArg(args, "events", defaultEvents, 0, maxEvents)
	if err != nil {
		return nil, err
	}
	p, err := findProject(ctx, c, m[1])
	if err != nil {
		return nil, err
	}
	var resp issuesv1.GetIssueResponse
	if err := get(ctx, c, mGetIssue, map[string]any{"project_id": p.GetId(), "number": m[2]}, &resp); err != nil {
		return nil, err
	}
	is := resp.GetIssue()
	names, err := loadNames(ctx, c, p.GetId())
	if err != nil {
		return nil, err
	}
	d := issueDetail{
		issueOut:      summary(p.GetKey(), is, names),
		Description:   is.GetDescription(),
		DiscardReason: is.GetDiscard().GetReason(),
		SourceKey:     is.GetSourceKey(),
		CreatedAt:     rfc3339(is.GetCreatedAt()),
		UpdatedAt:     rfc3339(is.GetUpdatedAt()),
		Events:        []eventOut{},
	}

	if nEvents == 0 {
		return d, nil
	}
	// El historial viene del más viejo al más nuevo y el contrato no deja
	// pedir el final: se recorre (con tope) y se quedan los últimos nEvents.
	var all []*issuesv1.IssueEvent
	cursor := ""
	for i := 0; ; i++ {
		if i == maxEventPages {
			d.EventsPartial = true
			break
		}
		var ev issuesv1.ListIssueEventsResponse
		if err := get(ctx, c, mListEvents, map[string]any{"project_id": p.GetId(), "number": m[2], "page": page(cursor, pageMax)}, &ev); err != nil {
			return nil, err
		}
		all = append(all, ev.GetEvents()...)
		if !ev.GetPage().GetHasNext() {
			break
		}
		cursor = ev.GetPage().GetNextCursor()
	}
	d.EventsTotal = len(all)
	for _, e := range all[max(0, len(all)-nEvents):] {
		d.Events = append(d.Events, event(e, names))
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Ayudas
// ---------------------------------------------------------------------------

var unmarshal = protojson.UnmarshalOptions{DiscardUnknown: true}

func get(ctx context.Context, c *mcpserver.Caller, method string, args map[string]any, out proto.Message) error {
	b, err := c.Get(ctx, method, args)
	if err != nil {
		return err
	}
	if err := unmarshal.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s: respuesta ilegible: %w", method, err)
	}
	return nil
}

func page(cursor string, limit int) map[string]any {
	p := map[string]any{"limit": strconv.Itoa(limit)}
	if cursor != "" {
		p["cursor"] = cursor
	}
	return p
}

// allProjects: los proyectos visibles; partial = hay más de los que se leen.
func allProjects(ctx context.Context, c *mcpserver.Caller) ([]*issuesv1.Project, bool, error) {
	var out []*issuesv1.Project
	cursor := ""
	for range maxProjectPages {
		var resp issuesv1.ListProjectsResponse
		if err := get(ctx, c, mListProjects, map[string]any{"page": page(cursor, pageMax)}, &resp); err != nil {
			return nil, false, err
		}
		out = append(out, resp.GetProjects()...)
		if !resp.GetPage().GetHasNext() {
			return out, false, nil
		}
		cursor = resp.GetPage().GetNextCursor()
	}
	return out, true, nil
}

var projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9]{1,9}$`)

// findProject resuelve la clave (sin distinguir mayúsculas) entre los
// proyectos visibles para la credencial.
func findProject(ctx context.Context, c *mcpserver.Caller, key string) (*issuesv1.Project, error) {
	key = strings.ToUpper(strings.TrimSpace(key))
	if !projectKeyRe.MatchString(key) {
		return nil, invalid("project: clave de proyecto como RND (2 a 10 letras o dígitos)")
	}
	projects, partial, err := allProjects(ctx, c)
	if err != nil {
		return nil, err
	}
	for _, p := range projects {
		if strings.EqualFold(p.GetKey(), key) {
			return p, nil
		}
	}
	if partial {
		return nil, &mcpserver.ToolError{Code: "NOT_FOUND", Reason: "PROJECT_NOT_FOUND",
			Message: fmt.Sprintf("%s no está entre los primeros %d proyectos visibles para este token", key, maxProjectPages*pageMax)}
	}
	return nil, &mcpserver.ToolError{Code: "NOT_FOUND", Reason: "PROJECT_NOT_FOUND",
		Message: fmt.Sprintf("no hay un proyecto %s visible para este token; list_projects muestra las claves", key)}
}

// hint: qué hacer con un error de la API, por code (va en el resultado).
func hint(code string) string {
	switch code {
	case "UNAUTHENTICATED":
		return "Token ausente, vencido o revocado: pedile a la persona uno nuevo (tk token nuevo)."
	case "PERMISSION_DENIED":
		return "El token no tiene issues:read: hace falta uno con tk token nuevo --permiso issues:read."
	case "NOT_FOUND":
		return "No existe o no es visible para este token: revisá la clave con list_projects o list_issues."
	case "INVALID_ARGUMENT":
		return "Revisá los argumentos. Si cambiaste filtros, empezá de nuevo sin cursor."
	case "RESOURCE_EXHAUSTED":
		return "Límite de llamadas: esperá un minuto antes de reintentar."
	case "UNAVAILABLE", "DEADLINE_EXCEEDED", "INTERNAL", "UNKNOWN":
		return "Falla del servidor: reintentá en unos segundos; si sigue, avisá con el request_id."
	}
	return ""
}

// names: estados y etiquetas del proyecto, para mostrar nombres y no ids.
type names struct {
	statuses []*issuesv1.Status
	labels   map[string]string
}

func loadNames(ctx context.Context, c *mcpserver.Caller, projectID string) (*names, error) {
	var st issuesv1.ListStatusesResponse
	if err := get(ctx, c, mListStatuses, map[string]any{"project_id": projectID}, &st); err != nil {
		return nil, err
	}
	var lb issuesv1.ListLabelsResponse
	if err := get(ctx, c, mListLabels, map[string]any{"project_id": projectID}, &lb); err != nil {
		return nil, err
	}
	n := &names{statuses: st.GetStatuses(), labels: map[string]string{}}
	for _, l := range lb.GetLabels() {
		n.labels[l.GetId()] = l.GetName()
	}
	return n, nil
}

func (n *names) status(id string) string {
	for _, s := range n.statuses {
		if s.GetId() == id {
			return s.GetName()
		}
	}
	return ""
}

func (n *names) statusID(name string) (string, bool) {
	for _, s := range n.statuses {
		if strings.EqualFold(s.GetName(), strings.TrimSpace(name)) {
			return s.GetId(), true
		}
	}
	return "", false
}

func (n *names) statusNames() []string {
	out := make([]string, 0, len(n.statuses))
	for _, s := range n.statuses {
		out = append(out, s.GetName())
	}
	return out
}

func summary(projectKey string, is *issuesv1.Issue, n *names) issueOut {
	o := issueOut{
		Key:      fmt.Sprintf("%s-%d", projectKey, is.GetNumber()),
		Title:    is.GetTitle(),
		Category: categoryName(is.GetState()),
		Status:   n.status(is.GetStatusId()),
		Priority: priorityName(is.GetPriority()),
		Assignee: is.GetAssigneeActorId(),
	}
	for _, id := range is.GetLabelIds() {
		if name, ok := n.labels[id]; ok {
			o.Labels = append(o.Labels, name)
		}
	}
	return o
}

func event(e *issuesv1.IssueEvent, n *names) eventOut {
	o := eventOut{At: rfc3339(e.GetCreatedAt()), Actor: e.GetActorId(), Agent: e.GetAgentId()}
	switch ch := e.GetChange().(type) {
	case *issuesv1.IssueEvent_Created:
		o.Change = "creado"
	case *issuesv1.IssueEvent_StatusChanged:
		o.Change = fmt.Sprintf("estado: %s → %s", ch.StatusChanged.GetFromStatusName(), ch.StatusChanged.GetToStatusName())
	case *issuesv1.IssueEvent_TitleChanged:
		o.Change = fmt.Sprintf("título: «%s» → «%s»", ch.TitleChanged.GetFrom(), ch.TitleChanged.GetTo())
	case *issuesv1.IssueEvent_DescriptionChanged:
		o.Change = "descripción editada"
	case *issuesv1.IssueEvent_PriorityChanged:
		o.Change = fmt.Sprintf("prioridad: %s → %s", priorityName(ch.PriorityChanged.GetFrom()), priorityName(ch.PriorityChanged.GetTo()))
	case *issuesv1.IssueEvent_LabelsChanged:
		var parts []string
		for _, l := range ch.LabelsChanged.GetAddedLabelNames() {
			parts = append(parts, "+"+l)
		}
		for _, l := range ch.LabelsChanged.GetRemovedLabelNames() {
			parts = append(parts, "-"+l)
		}
		o.Change = "etiquetas: " + strings.Join(parts, ", ")
	case *issuesv1.IssueEvent_Commented:
		o.Change = "comentario: " + truncate(ch.Commented.GetBody(), maxCommentRunes)
	case *issuesv1.IssueEvent_Discarded:
		o.Change = fmt.Sprintf("descartado (%s → %s): %s", ch.Discarded.GetFromStatusName(), ch.Discarded.GetToStatusName(), ch.Discarded.GetReason())
	case *issuesv1.IssueEvent_Restored:
		o.Change = fmt.Sprintf("restaurado (%s → %s)", ch.Restored.GetFromStatusName(), ch.Restored.GetToStatusName())
	case *issuesv1.IssueEvent_AssigneeChanged:
		o.Change = fmt.Sprintf("asignado: %s → %s", who(ch.AssigneeChanged.GetFromDisplayName(), ch.AssigneeChanged.GetFromActorId()),
			who(ch.AssigneeChanged.GetToDisplayName(), ch.AssigneeChanged.GetToActorId()))
	default:
		o.Change = "otro cambio"
	}
	return o
}

func who(name, id string) string {
	switch {
	case name != "":
		return name
	case id != "":
		return id
	}
	return "sin asignar"
}

func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func rfc3339(t *timestamppb.Timestamp) string {
	if t == nil {
		return ""
	}
	return t.AsTime().UTC().Format(time.RFC3339)
}

var (
	categoryNames  = []string{"todo", "in_progress", "done", "canceled"}
	categoryByName = map[string]issuesv1.IssueState{
		"todo":        issuesv1.IssueState_ISSUE_STATE_TODO,
		"in_progress": issuesv1.IssueState_ISSUE_STATE_IN_PROGRESS,
		"done":        issuesv1.IssueState_ISSUE_STATE_DONE,
		"canceled":    issuesv1.IssueState_ISSUE_STATE_CANCELED,
	}
	priorityNames  = []string{"urgent", "high", "medium", "low"}
	priorityByName = map[string]issuesv1.Priority{
		"urgent": issuesv1.Priority_PRIORITY_URGENT,
		"high":   issuesv1.Priority_PRIORITY_HIGH,
		"medium": issuesv1.Priority_PRIORITY_MEDIUM,
		"low":    issuesv1.Priority_PRIORITY_LOW,
	}
)

func categoryName(s issuesv1.IssueState) string {
	for n, v := range categoryByName {
		if v == s {
			return n
		}
	}
	return ""
}

func priorityName(p issuesv1.Priority) string {
	for n, v := range priorityByName {
		if v == p {
			return n
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Argumentos y esquemas
// ---------------------------------------------------------------------------

func invalid(format string, a ...any) error {
	return &mcpserver.ToolError{Code: "INVALID_ARGUMENT", Message: fmt.Sprintf(format, a...)}
}

func onlyKeys(args map[string]any, allowed ...string) error {
	for k := range args {
		ok := false
		for _, a := range allowed {
			ok = ok || a == k
		}
		if !ok {
			return invalid("argumento desconocido %q", k)
		}
	}
	return nil
}

func strArg(args map[string]any, name string, required bool) (string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		if required {
			return "", invalid("falta el argumento %q", name)
		}
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", invalid("%s: se esperaba un texto", name)
	}
	s = strings.TrimSpace(s)
	if required && s == "" {
		return "", invalid("falta el argumento %q", name)
	}
	return s, nil
}

func intArg(args map[string]any, name string, def, lo, hi int) (int, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return def, nil
	}
	num, ok := v.(json.Number)
	if !ok {
		return 0, invalid("%s: se esperaba un entero entre %d y %d", name, lo, hi)
	}
	n, err := strconv.Atoi(num.String())
	if err != nil || n < lo || n > hi {
		return 0, invalid("%s: se esperaba un entero entre %d y %d", name, lo, hi)
	}
	return n, nil
}

func object(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func enum(desc string, values ...string) map[string]any {
	s := map[string]any{"type": "string", "enum": values}
	if desc != "" {
		s["description"] = desc
	}
	return s
}
