package mcptools

import (
	"context"
	"fmt"
	"strings"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/mcpserver"
)

// Sesiones de agentes (issue 026): el bloque `sessions` de list_issues y
// get_issue (Issue.sessions) y la herramienta list_agent_sessions. Los
// estados salen con los nombres de docs/03 §4 de dev-plantillas, que son
// los que usan los agentes.

const mListAgentSessions = "issues.v1.AgentSessionService/ListAgentSessions"

var (
	sessionStatusNames = []string{"trabajando", "esperando-ok", "en-reposo", "terminada", "desconocido", "colgado"}
	sessionStatusBy    = map[string]issuesv1.SessionStatus{
		"trabajando":   issuesv1.SessionStatus_SESSION_STATUS_WORKING,
		"esperando-ok": issuesv1.SessionStatus_SESSION_STATUS_WAITING_OK,
		"en-reposo":    issuesv1.SessionStatus_SESSION_STATUS_IDLE,
		"terminada":    issuesv1.SessionStatus_SESSION_STATUS_ENDED,
		"desconocido":  issuesv1.SessionStatus_SESSION_STATUS_UNKNOWN,
		"colgado":      issuesv1.SessionStatus_SESSION_STATUS_STALLED,
	}
	sessionStatusName = invertMap(sessionStatusBy)
	waitingOnName     = map[issuesv1.WaitingOn]string{
		issuesv1.WaitingOn_WAITING_ON_PERSON:  "person",
		issuesv1.WaitingOn_WAITING_ON_SESSION: "session",
		issuesv1.WaitingOn_WAITING_ON_OTHER:   "other",
	}
)

func invertMap[K, V comparable](m map[K]V) map[V]K {
	out := make(map[V]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

// sessionsOut: el resumen de las sesiones activas de un issue. Mismos
// nombres en list_issues (sin items) y get_issue.
type sessionsOut struct {
	Active      int32 `json:"active"`
	Working     int32 `json:"working"`
	Waiting     int32 `json:"waiting"`
	Stalled     int32 `json:"stalled"`
	Idle        int32 `json:"idle"`
	NeedsPerson bool  `json:"needs_person"`
	// Items: quién atiende (hasta 5), solo en get_issue.
	Items []sessionItem `json:"items,omitempty"`
}

type sessionItem struct {
	Agent         string `json:"agent"`
	Name          string `json:"name,omitempty"`
	Machine       string `json:"machine"`
	Status        string `json:"status"`
	WaitingOn     string `json:"waiting_on,omitempty"`
	WaitingTarget string `json:"waiting_target,omitempty"`
	Since         string `json:"since"`
}

// issueSessions convierte Issue.sessions (nil si no hay sesiones activas).
func issueSessions(s *issuesv1.IssueSessions) *sessionsOut {
	if s == nil {
		return nil
	}
	out := &sessionsOut{
		Active: s.GetActive(), Working: s.GetWorking(), Waiting: s.GetWaiting(),
		Stalled: s.GetStalled(), Idle: s.GetIdle(), NeedsPerson: s.GetNeedsPerson(),
	}
	for _, it := range s.GetItems() {
		out.Items = append(out.Items, sessionItem{
			Agent:         it.GetAgentDisplayName(),
			Name:          it.GetSessionName(),
			Machine:       it.GetMachine(),
			Status:        sessionStatusName[it.GetStatus()],
			WaitingOn:     waitingOnName[it.GetWaitingOn()],
			WaitingTarget: it.GetWaitingTarget(),
			Since:         rfc3339(it.GetStatusChangedAt()),
		})
	}
	return out
}

func listAgentSessionsTool() mcpserver.CustomTool {
	return mcpserver.CustomTool{
		Name: "list_agent_sessions",
		InputSchema: object(map[string]any{
			"project":          str("Clave del proyecto (RND): solo sus sesiones"),
			"issue":            str("Clave del issue (RND-236): solo sus sesiones"),
			"status":           map[string]any{"type": "array", "items": enum("", sessionStatusNames...), "description": "Uno o varios estados"},
			"unlinked":         map[string]any{"type": "boolean", "description": "Solo las sesiones sin issue"},
			"include_inactive": map[string]any{"type": "boolean", "description": "También terminadas y sin reporte hace más de 24 h"},
			"cursor":           str("next_cursor de la página anterior, con los mismos filtros"),
			"limit":            map[string]any{"type": "integer", "minimum": 1, "maximum": pageMax, "description": "Por página (20 por defecto)"},
		}),
		Methods: []string{mListProjects, mListAgentSessions},
		Handle:  listAgentSessions,
		Hint:    hint,
	}
}

type agentSessionOut struct {
	Issue         string `json:"issue,omitempty"` // RND-236; vacío = sin issue
	Agent         string `json:"agent"`
	Name          string `json:"name,omitempty"`
	Workspace     string `json:"workspace,omitempty"`
	Machine       string `json:"machine"`
	Cwd           string `json:"cwd,omitempty"`
	Status        string `json:"status"`
	WaitingOn     string `json:"waiting_on,omitempty"`
	WaitingTarget string `json:"waiting_target,omitempty"`
	WaitingText   string `json:"waiting_text,omitempty"`
	Since         string `json:"since"`
	LastSeen      string `json:"last_seen"`
}

type agentSessionsOut struct {
	Sessions   []agentSessionOut `json:"sessions"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

func listAgentSessions(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args, "project", "issue", "status", "unlinked", "include_inactive", "cursor", "limit"); err != nil {
		return nil, err
	}
	project, err := strArg(args, "project", false)
	if err != nil {
		return nil, err
	}
	issue, err := strArg(args, "issue", false)
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
	number := ""
	if issue != "" {
		m := issueKeyRe.FindStringSubmatch(issue)
		if m == nil {
			return nil, invalid("issue: clave de issue como RND-236")
		}
		if project != "" && !strings.EqualFold(project, m[1]) {
			return nil, invalid("issue: %s no es del proyecto %s", issue, project)
		}
		project, number = m[1], m[2]
	}
	for _, b := range []string{"unlinked", "include_inactive"} {
		v, ok := args[b]
		if !ok || v == nil {
			continue
		}
		on, isBool := v.(bool)
		if !isBool {
			return nil, invalid("%s: se esperaba true o false", b)
		}
		if on {
			q[b] = true
		}
	}
	if q["unlinked"] == true && project != "" {
		return nil, invalid("unlinked: no se combina con project ni issue")
	}
	if st, ok := args["status"]; ok {
		list, isList := st.([]any)
		if s, isStr := st.(string); isStr {
			list, isList = []any{s}, true
		}
		if !isList {
			return nil, invalid("status: lista de %s", strings.Join(sessionStatusNames, ", "))
		}
		var sts []any
		for _, v := range list {
			s, _ := v.(string)
			sv, ok := sessionStatusBy[s]
			if !ok {
				return nil, invalid("status: lista de %s", strings.Join(sessionStatusNames, ", "))
			}
			if sv == issuesv1.SessionStatus_SESSION_STATUS_ENDED {
				q["include_inactive"] = true
			}
			sts = append(sts, sv.String())
		}
		if len(sts) > 0 {
			q["statuses"] = sts
		}
	}

	projects, _, err := allProjects(ctx, c)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]string, len(projects))
	for _, p := range projects {
		keys[p.GetId()] = p.GetKey()
	}
	if project != "" {
		var p *issuesv1.Project
		for _, cand := range projects {
			if strings.EqualFold(cand.GetKey(), project) {
				p = cand
			}
		}
		if p == nil {
			// Clave mal formada o no visible: el mismo error que las demás.
			if p, err = findProject(ctx, c, project); err != nil {
				return nil, err
			}
		}
		q["project_id"] = p.GetId()
		if number != "" {
			q["issue_number"] = number
		}
	}
	var resp issuesv1.ListAgentSessionsResponse
	if err := get(ctx, c, mListAgentSessions, q, &resp); err != nil {
		return nil, err
	}
	out := agentSessionsOut{Sessions: make([]agentSessionOut, 0, len(resp.GetSessions()))}
	if resp.GetPage().GetHasNext() {
		out.NextCursor = resp.GetPage().GetNextCursor()
	}
	for _, s := range resp.GetSessions() {
		o := agentSessionOut{
			Agent:         s.GetAgentDisplayName(),
			Name:          s.GetSessionName(),
			Workspace:     s.GetWorkspaceLabel(),
			Machine:       s.GetMachine(),
			Cwd:           s.GetCwdRel(),
			Status:        sessionStatusName[s.GetStatus()],
			WaitingOn:     waitingOnName[s.GetWaitingOn()],
			WaitingTarget: s.GetWaitingTarget(),
			WaitingText:   s.GetWaitingText(),
			Since:         rfc3339(s.GetStatusChangedAt()),
			LastSeen:      rfc3339(s.GetLastSeenAt()),
		}
		if key, ok := keys[s.GetProjectId()]; ok && s.GetIssueNumber() > 0 {
			o.Issue = fmt.Sprintf("%s-%d", key, s.GetIssueNumber())
		}
		out.Sessions = append(out.Sessions, o)
	}
	return out, nil
}
