package mcptools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/tekitla/tekitla-gateway/app/mcpserver"
)

// Parte 3 del issue 036 en get_flow_metrics: dónde se va el tiempo según las
// sesiones de agentes (FlowEfficiency), la cola humana y la señal de
// contratación, en horas y en palabras. Con el argumento issue, la
// eficiencia de un issue suelto (GetIssueFlowEfficiency, CU-036-6).

const mIssueFlowEfficiency = "issues.v1.IssueService/GetIssueFlowEfficiency"

// timeOut es FlowEfficiency en horas con un decimal.
type timeOut struct {
	WorkingHours      float64            `json:"working_hours"`
	WaitingHumanHours float64            `json:"waiting_human_hours"`
	BlockedHours      float64            `json:"blocked_hours"`
	InReviewHours     float64            `json:"in_review_hours"`
	IdleHours         float64            `json:"idle_hours"`
	EfficiencyPct     *float64           `json:"efficiency_pct,omitempty"`
	Issues            int32              `json:"issues"`
	WaitingByReason   map[string]float64 `json:"waiting_human_hours_by_reason,omitempty"`
}

type queueOut struct {
	Pending             int32    `json:"pending"`
	OldestPendingHours  *float64 `json:"oldest_pending_hours,omitempty"`
	ResponseTimeCount   int      `json:"answered"`
	ResponseMedianHours *float64 `json:"response_median_hours,omitempty"`
	ResponseP85Hours    *float64 `json:"response_p85_hours,omitempty"`
}

type windowOut struct {
	From                string   `json:"from"`
	To                  string   `json:"to"`
	TimeSpent           timeOut  `json:"time_spent"`
	ResponseMedianHours *float64 `json:"response_median_hours,omitempty"`
	WIPMedian           int32    `json:"wip_median"`
	Closed              int32    `json:"closed"`
}

type profileOut struct {
	Profile  string  `json:"profile"`
	Reason   string  `json:"reason"`
	SharePct float64 `json:"share_pct"`
}

type signalOut struct {
	Verdict     string       `json:"verdict"`
	Summary     string       `json:"summary"`
	LiveWeeks   int32        `json:"live_weeks"`
	WeeksNeeded int32        `json:"weeks_needed"`
	Preliminary bool         `json:"preliminary,omitempty"`
	WIPHigh     bool         `json:"wip_high,omitempty"`
	Profiles    []profileOut `json:"profiles,omitempty"`
	Current     *windowOut   `json:"current,omitempty"`
	Previous    *windowOut   `json:"previous,omitempty"`
}

var reasonName = map[issuesv1.WaitingReason]string{
	issuesv1.WaitingReason_WAITING_REASON_UNSPECIFIED: "sin motivo",
	issuesv1.WaitingReason_WAITING_REASON_REVIEW:      "revision",
	issuesv1.WaitingReason_WAITING_REASON_TESTING:     "pruebas",
	issuesv1.WaitingReason_WAITING_REASON_DECISION:    "decision",
	issuesv1.WaitingReason_WAITING_REASON_DEPLOY:      "deploy",
}

var reasonProfile = map[issuesv1.WaitingReason]string{
	issuesv1.WaitingReason_WAITING_REASON_REVIEW:   "alguien senior que revise",
	issuesv1.WaitingReason_WAITING_REASON_TESTING:  "QA",
	issuesv1.WaitingReason_WAITING_REASON_DECISION: "producto",
	issuesv1.WaitingReason_WAITING_REASON_DEPLOY:   "DevOps",
}

func hours(d *durationpb.Duration) float64 {
	return math.Round(d.AsDuration().Hours()*10) / 10
}

func hoursPtr(d *durationpb.Duration) *float64 {
	if d == nil {
		return nil
	}
	h := hours(d)
	return &h
}

func timeSpent(e *issuesv1.FlowEfficiency) *timeOut {
	if e == nil {
		return nil
	}
	o := &timeOut{
		WorkingHours: hours(e.GetWorking()), WaitingHumanHours: hours(e.GetWaitingHuman()),
		BlockedHours: hours(e.GetBlocked()), InReviewHours: hours(e.GetInReview()), IdleHours: hours(e.GetIdle()),
		Issues: e.GetIssues(),
	}
	if e.EfficiencyBp != nil {
		pct := float64(e.GetEfficiencyBp()) / 100
		o.EfficiencyPct = &pct
	}
	for _, rt := range e.GetWaitingHumanByReason() {
		if o.WaitingByReason == nil {
			o.WaitingByReason = map[string]float64{}
		}
		o.WaitingByReason[reasonName[rt.GetReason()]] = hours(rt.GetDuration())
	}
	return o
}

func humanQueue(q *issuesv1.HumanQueue) *queueOut {
	if q == nil {
		return nil
	}
	o := &queueOut{Pending: q.GetPending(), OldestPendingHours: hoursPtr(q.GetOldestPending()), ResponseTimeCount: int(q.GetResponseTime().GetCount())}
	if o.ResponseTimeCount > 0 {
		o.ResponseMedianHours = hoursPtr(q.GetResponseTime().GetMedian())
		o.ResponseP85Hours = hoursPtr(q.GetResponseTime().GetP85())
	}
	return o
}

func window(w *issuesv1.SignalWindow, loc *time.Location) *windowOut {
	if w == nil {
		return nil
	}
	o := &windowOut{From: day(w.GetStart(), loc), To: lastDay(w.GetEnd(), loc), WIPMedian: w.GetWipMedian(), Closed: w.GetClosed()}
	if t := timeSpent(w.GetEfficiency()); t != nil {
		o.TimeSpent = *t
	}
	if w.GetHumanResponse().GetCount() > 0 {
		o.ResponseMedianHours = hoursPtr(w.GetHumanResponse().GetMedian())
	}
	return o
}

func hiringSignal(s *issuesv1.HiringSignal, loc *time.Location) *signalOut {
	if s == nil {
		return nil
	}
	o := &signalOut{
		LiveWeeks: s.GetLiveWeeks(), WeeksNeeded: s.GetWeeksNeeded(), Preliminary: s.GetPreliminary(), WIPHigh: s.GetWipHigh(),
		Current: window(s.GetCurrent(), loc), Previous: window(s.GetPrevious(), loc),
	}
	for _, p := range s.GetProfiles() {
		o.Profiles = append(o.Profiles, profileOut{Profile: reasonProfile[p.GetReason()], Reason: reasonName[p.GetReason()], SharePct: float64(p.GetShareBp()) / 100})
	}
	switch s.GetVerdict() {
	case issuesv1.HiringVerdict_HIRING_VERDICT_INSUFFICIENT_DATA:
		o.Verdict = "datos insuficientes"
		o.Summary = fmt.Sprintf("Faltan semanas de datos en vivo: hay %d de las %d que necesita la señal (%d para una lectura preliminar).",
			s.GetLiveWeeks(), s.GetWeeksNeeded(), s.GetWeeksNeededPreliminary())
	case issuesv1.HiringVerdict_HIRING_VERDICT_NEEDS_PEOPLE:
		o.Verdict = "falta gente"
		o.Summary = "El trabajo espera a personas mientras los agentes no son el cuello de botella."
		if len(o.Profiles) > 0 {
			o.Summary += " Perfil: " + o.Profiles[0].Profile + " (donde más se junta la espera)."
		} else {
			o.Summary += " Perfil sin dato: las esperas no traen motivo (espera.ps1 -Motivo)."
		}
		if s.GetWipHigh() {
			o.Summary += " Además hay mucho trabajo empezado."
		}
	case issuesv1.HiringVerdict_HIRING_VERDICT_REDUCE_WIP:
		o.Verdict = "bajar el WIP"
		o.Summary = "No falta gente: sobra trabajo empezado y casi no hay espera a personas."
	default:
		o.Verdict = "sin señal"
		o.Summary = "El flujo no pide contratar ni bajar el WIP."
	}
	if s.GetPreliminary() {
		o.Summary += " Lectura preliminar: todavía no compara con el mes anterior."
	}
	return o
}

// sessionNotes: notas de la parte de sesiones.
func sessionNotes(r *issuesv1.GetFlowMetricsResponse, loc *time.Location) []string {
	if r.GetSessionsRecordedSince() == nil {
		return []string{"Dónde se va el tiempo: todavía no hay historia de sesiones de agentes en este proyecto."}
	}
	return []string{
		"time_spent sale de las sesiones de agentes (registrado en vivo desde " + day(r.GetSessionsRecordedSince(), loc) +
			"): tiempo de reloj del issue con prioridad trabajando > espera a una persona > bloqueado > en revisión. Las noches y los fines de semana cuentan. efficiency_pct = trabajando / (trabajando + esperas).",
		"La señal de contratación es una señal, no una decisión: sus números están en current y previous.",
	}
}

type issueFlowOut struct {
	Project       string    `json:"project"`
	Issue         string    `json:"issue"`
	From          string    `json:"from,omitempty"`
	To            string    `json:"to,omitempty"`
	RecordedSince string    `json:"recorded_since,omitempty"`
	TimeSpent     *timeOut  `json:"time_spent,omitempty"`
	HumanQueue    *queueOut `json:"human_queue,omitempty"`
	Notes         []string  `json:"notes"`
}

// issueArg lee issue: un número o una clave del proyecto (RND-279).
func issueArg(args map[string]any, projectKey string) (int, bool, error) {
	v, ok := args["issue"]
	if !ok || v == nil {
		return 0, false, nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = strings.TrimSpace(x)
	case json.Number:
		s = x.String()
	default:
		return 0, false, invalid("issue: número o clave como %s-12", projectKey)
	}
	if m := issueKeyRe.FindStringSubmatch(s); m != nil {
		if !strings.EqualFold(m[1], projectKey) {
			return 0, false, invalid("issue: %s no es del proyecto %s", s, projectKey)
		}
		s = m[2]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false, invalid("issue: número o clave como %s-12", projectKey)
	}
	return n, true, nil
}

func issueFlow(ctx context.Context, c *mcpserver.Caller, p *issuesv1.Project, number int, q map[string]any, loc *time.Location) (any, error) {
	req := map[string]any{"project_id": p.GetId(), "number": strconv.Itoa(number)}
	for _, k := range []string{"from", "to"} {
		if v, ok := q[k]; ok {
			req[k] = v
		}
	}
	var resp issuesv1.GetIssueFlowEfficiencyResponse
	if err := get(ctx, c, mIssueFlowEfficiency, req, &resp); err != nil {
		return nil, err
	}
	out := issueFlowOut{
		Project: p.GetKey(), Issue: fmt.Sprintf("%s-%d", p.GetKey(), number),
		TimeSpent: timeSpent(resp.GetEfficiency()), HumanQueue: humanQueue(resp.GetHumanQueue()),
	}
	if v, ok := q["from"].(string); ok {
		t, _ := time.Parse(time.RFC3339, v)
		out.From = t.In(loc).Format(time.DateOnly)
	}
	if v, ok := q["to"].(string); ok {
		t, _ := time.Parse(time.RFC3339, v)
		out.To = t.In(loc).AddDate(0, 0, -1).Format(time.DateOnly)
	}
	if resp.GetRecordedSince() != nil {
		out.RecordedSince = day(resp.GetRecordedSince(), loc)
	}
	if out.TimeSpent == nil {
		out.Notes = append(out.Notes, "Este issue no tiene historia de sesiones de agentes en el período: la eficiencia se mide solo en vivo.")
	} else {
		out.Notes = append(out.Notes,
			"Tiempo de reloj del issue según sus sesiones de agentes, con prioridad trabajando > espera a una persona > bloqueado > en revisión. Las noches y los fines de semana cuentan.",
			"efficiency_pct = trabajando / (trabajando + esperas). idle_hours (en reposo sin esperar nada) no entra.")
	}
	return out, nil
}
