package mcptools

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tekitla/tekitla-gateway/app/mcpserver"
)

// get_flow_metrics (issue 036): métricas de flujo de un proyecto por semana.
// Nombre acordado con la sesión plantillas el 2026-11-13 (docs/15 §7 de
// dev-plantillas). Traduce la clave del proyecto, las fechas y los nombres
// de etiquetas, y devuelve nombres en lugar de ids, duraciones en días y la
// confianza de cada número en palabras.

const (
	mFlowMetrics = "issues.v1.IssueService/GetFlowMetrics"
	// defaultFlowTimeZone: la del servicio (model.FlowDefaultTimeZone).
	defaultFlowTimeZone = "America/Mexico_City"
	maxStale            = 50
	defaultStale        = 10
)

func flowMetricsTool() mcpserver.CustomTool {
	return mcpserver.CustomTool{
		Name: "get_flow_metrics",
		InputSchema: object(map[string]any{
			"project":   str("Clave del proyecto, p. ej. RND"),
			"from":      str("Primer día, YYYY-MM-DD (por defecto, 12 semanas antes de to)"),
			"to":        str("Último día incluido, YYYY-MM-DD (por defecto, hoy)"),
			"labels":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Solo issues con alguna de estas etiquetas (nombres), p. ej. un área"},
			"time_zone": str("Zona IANA de las semanas y las fechas (America/Mexico_City por defecto)"),
			"stale":     map[string]any{"type": "integer", "minimum": 0, "maximum": maxStale, "description": "Cuántos estancados listar (10 por defecto)"},
		}, "project"),
		Methods: []string{mListProjects, mFlowMetrics, mListStatuses, mListLabels},
		Handle:  flowMetrics,
		Hint:    flowHint,
	}
}

// flowHint: como hint, con los errores propios de las métricas.
func flowHint(code string) string {
	if code == "INVALID_ARGUMENT" {
		return "Revisá from y to (YYYY-MM-DD, from antes que to, a lo sumo 104 semanas), time_zone (IANA) y labels (list_issues muestra las etiquetas)."
	}
	return hint(code)
}

type durationOut struct {
	Count      int      `json:"count"`
	MedianDays *float64 `json:"median_days,omitempty"`
	P85Days    *float64 `json:"p85_days,omitempty"`
}

type weekOut struct {
	Week        string      `json:"week"`
	LeadTime    durationOut `json:"lead_time"`
	CycleTime   durationOut `json:"cycle_time"`
	ClosedValue int32       `json:"closed_value"`
	ClosedOther int32       `json:"closed_other"`
	Canceled    int32       `json:"canceled"`
	Started     int32       `json:"started"`
	WIP         int32       `json:"wip"`
	Source      string      `json:"source"`
}

type summaryOut struct {
	LeadTime    durationOut    `json:"lead_time"`
	CycleTime   durationOut    `json:"cycle_time"`
	ClosedValue int32          `json:"closed_value"`
	ClosedOther int32          `json:"closed_other"`
	Canceled    int32          `json:"canceled"`
	Started     int32          `json:"started"`
	WIP         int32          `json:"wip"`
	Source      string         `json:"source"`
	Events      map[string]int `json:"events_by_source"`
}

type staleOut struct {
	Key             string `json:"key"`
	Title           string `json:"title"`
	Status          string `json:"status"`
	InProgressSince string `json:"in_progress_since"`
	LastActivity    string `json:"last_activity"`
	DaysIdle        int    `json:"days_without_activity"`
	Source          string `json:"source"`
}

type flowOut struct {
	Project     string     `json:"project"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	TimeZone    string     `json:"time_zone"`
	ValueLabels []string   `json:"value_labels"`
	Summary     summaryOut `json:"summary"`
	Weeks       []weekOut  `json:"weeks"`
	Stale       []staleOut `json:"stale"`
	StaleTotal  int32      `json:"stale_total"`
	Issues      int32      `json:"issues"`
	// DoneWithoutDates: hechos sin fecha de cierre, fuera del cálculo.
	DoneWithoutDates int32    `json:"done_without_dates"`
	Notes            []string `json:"notes"`
}

func flowMetrics(ctx context.Context, c *mcpserver.Caller, args map[string]any) (any, error) {
	if err := onlyKeys(args, "project", "from", "to", "labels", "time_zone", "stale"); err != nil {
		return nil, err
	}
	key, err := strArg(args, "project", true)
	if err != nil {
		return nil, err
	}
	tz, err := strArg(args, "time_zone", false)
	if err != nil {
		return nil, err
	}
	if tz == "" {
		tz = defaultFlowTimeZone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return nil, &mcpserver.ToolError{Code: "INVALID_ARGUMENT", Reason: "INVALID_TIME_ZONE",
			Message: fmt.Sprintf("time_zone: %q no es una zona IANA (p. ej. America/Mexico_City)", tz)}
	}
	q := map[string]any{"time_zone": tz}
	from, err := dayArg(args, "from", loc, 0)
	if err != nil {
		return nil, err
	}
	// to es el último día incluido: el período termina al empezar el siguiente.
	to, err := dayArg(args, "to", loc, 1)
	if err != nil {
		return nil, err
	}
	if from != nil {
		q["from"] = from.UTC().Format(time.RFC3339)
	}
	if to != nil {
		q["to"] = to.UTC().Format(time.RFC3339)
	}
	nStale, err := intArg(args, "stale", defaultStale, 0, maxStale)
	if err != nil {
		return nil, err
	}
	// stale=0: el contrato no deja pedir cero, se pide uno y no se muestra.
	q["stale_limit"] = strconv.Itoa(max(nStale, 1))
	labelNames, err := strList(args, "labels")
	if err != nil {
		return nil, err
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
	if len(labelNames) > 0 {
		var ids []any
		for _, n := range labelNames {
			id, ok := names.labelID(n)
			if !ok {
				return nil, invalid("labels: %q no es una etiqueta de %s", n, p.GetKey())
			}
			ids = append(ids, id)
		}
		q["label_ids"] = ids
	}

	var resp issuesv1.GetFlowMetricsResponse
	if err := get(ctx, c, mFlowMetrics, q, &resp); err != nil {
		return nil, err
	}
	return flowResult(p.GetKey(), &resp, names, loc, nStale), nil
}

func flowResult(projectKey string, r *issuesv1.GetFlowMetricsResponse, n *names, loc *time.Location, nStale int) flowOut {
	s := r.GetSummary()
	out := flowOut{
		Project:  projectKey,
		From:     day(r.GetFrom(), loc),
		To:       lastDay(r.GetTo(), loc),
		TimeZone: r.GetTimeZone(),
		Summary: summaryOut{
			LeadTime: durationDays(s.GetLeadTime()), CycleTime: durationDays(s.GetCycleTime()),
			ClosedValue: s.GetClosedValue(), ClosedOther: s.GetClosedOther(), Canceled: s.GetCanceled(),
			Started: s.GetStarted(), WIP: s.GetWip(), Source: confidence(s.GetSource().GetConfidence()),
			Events: map[string]int{
				"live":           int(s.GetSource().GetLive()),
				"import_history": int(s.GetSource().GetImportHistory()),
				"import_file":    int(s.GetSource().GetImportFile()),
				"import_sync":    int(s.GetSource().GetImportSync()),
			},
		},
		Weeks:            make([]weekOut, 0, len(r.GetWeeks())),
		Stale:            []staleOut{},
		StaleTotal:       r.GetStaleTotal(),
		Issues:           r.GetCoverage().GetIssues(),
		DoneWithoutDates: r.GetCoverage().GetDoneWithoutDates(),
		ValueLabels:      []string{},
	}
	for _, w := range r.GetWeeks() {
		out.Weeks = append(out.Weeks, weekOut{
			Week:     day(w.GetStart(), loc),
			LeadTime: durationDays(w.GetLeadTime()), CycleTime: durationDays(w.GetCycleTime()),
			ClosedValue: w.GetClosedValue(), ClosedOther: w.GetClosedOther(), Canceled: w.GetCanceled(),
			Started: w.GetStarted(), WIP: w.GetWip(), Source: confidence(w.GetSource().GetConfidence()),
		})
	}
	to := r.GetTo().AsTime()
	for i, st := range r.GetStale() {
		if i == nStale {
			break
		}
		last := st.GetLastActivityAt().AsTime()
		out.Stale = append(out.Stale, staleOut{
			Key: fmt.Sprintf("%s-%d", projectKey, st.GetNumber()), Title: st.GetTitle(), Status: n.status(st.GetStatusId()),
			InProgressSince: day(st.GetInProgressSince(), loc), LastActivity: day(st.GetLastActivityAt(), loc),
			DaysIdle: int(to.Sub(last).Hours() / 24), Source: confidence(st.GetConfidence()),
		})
	}
	for _, id := range r.GetValueLabelIds() {
		if name, ok := n.labels[id]; ok {
			out.ValueLabels = append(out.ValueLabels, name)
		}
	}

	out.Notes = append(out.Notes,
		"Lead time: de creado a hecho. Cycle time: de la primera vez en curso a hecho. Mediana y percentil 85 de los que terminaron esa semana.",
		"Métricas del sistema, no de personas: sirven para ver dónde se atora el trabajo.")
	if len(out.ValueLabels) == 0 {
		out.Notes = append(out.Notes, "El proyecto no tiene etiquetas de valor: todo lo hecho cuenta como closed_value.")
	} else {
		out.Notes = append(out.Notes, "closed_value: hechos con alguna etiqueta de value_labels; closed_other, el resto. Lead y cycle time son de todo lo hecho.")
	}
	if s.GetSource().GetConfidence() == issuesv1.MetricConfidence_METRIC_CONFIDENCE_APPROXIMATE {
		out.Notes = append(out.Notes, "«histórico aproximado»: hay fechas reconstruidas de un importador (commits o fechas escritas en el archivo); los estados se cambiaban a mano y a veces tarde.")
	}
	if out.DoneWithoutDates > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d issues hechos sin fecha de cierre conocida no entran al cálculo.", out.DoneWithoutDates))
	}
	return out
}

func confidence(c issuesv1.MetricConfidence) string {
	switch c {
	case issuesv1.MetricConfidence_METRIC_CONFIDENCE_RECORDED:
		return "registrado en vivo"
	case issuesv1.MetricConfidence_METRIC_CONFIDENCE_APPROXIMATE:
		return "histórico aproximado"
	}
	return "sin datos"
}

func durationDays(d *issuesv1.DurationStats) durationOut {
	o := durationOut{Count: int(d.GetCount())}
	if o.Count > 0 {
		m, p := days(d.GetMedian()), days(d.GetP85())
		o.MedianDays, o.P85Days = &m, &p
	}
	return o
}

// days: la duración en días con un decimal.
func days(d *durationpb.Duration) float64 {
	return math.Round(d.AsDuration().Hours()/24*10) / 10
}

func day(t *timestamppb.Timestamp, loc *time.Location) string {
	if t == nil {
		return ""
	}
	return t.AsTime().In(loc).Format(time.DateOnly)
}

// lastDay: el día anterior a un fin exclusivo que cae a las 00:00; si no,
// el mismo día (to = ahora).
func lastDay(t *timestamppb.Timestamp, loc *time.Location) string {
	if t == nil {
		return ""
	}
	lt := t.AsTime().In(loc)
	if lt.Hour() == 0 && lt.Minute() == 0 && lt.Second() == 0 && lt.Nanosecond() == 0 {
		lt = lt.AddDate(0, 0, -1)
	}
	return lt.Format(time.DateOnly)
}

// dayArg lee una fecha YYYY-MM-DD como las 00:00 de ese día (más addDays)
// en loc; nil si no vino.
func dayArg(args map[string]any, name string, loc *time.Location, addDays int) (*time.Time, error) {
	s, err := strArg(args, name, false)
	if err != nil || s == "" {
		return nil, err
	}
	d, err := time.ParseInLocation(time.DateOnly, s, loc)
	if err != nil {
		return nil, invalid("%s: fecha como 2026-06-18", name)
	}
	d = d.AddDate(0, 0, addDays)
	return &d, nil
}

// strList lee una lista de textos (o un texto suelto).
func strList(args map[string]any, name string) ([]string, error) {
	v, ok := args[name]
	if !ok || v == nil {
		return nil, nil
	}
	if s, isStr := v.(string); isStr {
		v = []any{s}
	}
	list, ok := v.([]any)
	if !ok {
		return nil, invalid("%s: lista de nombres", name)
	}
	var out []string
	for _, it := range list {
		s, ok := it.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, invalid("%s: lista de nombres", name)
		}
		out = append(out, strings.TrimSpace(s))
	}
	return out, nil
}

func (n *names) labelID(name string) (string, bool) {
	for id, l := range n.labels {
		if strings.EqualFold(l, name) {
			return id, true
		}
	}
	return "", false
}
