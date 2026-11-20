package gateway_test

// get_flow_metrics (issue 036, CU-036-10): con issues:read, por clave del
// proyecto, con las fechas en la zona pedida, y de dónde sale cada número.

import (
	"slices"
	"strings"
	"testing"
	"time"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/tekitla/tekitla-gateway/app/auth"
)

// fakeFlowMetrics: dos semanas inventadas, la segunda con histórico.
func fakeFlowMetrics(in *issuesv1.GetFlowMetricsRequest) *issuesv1.GetFlowMetricsResponse {
	mx, _ := time.LoadLocation("America/Mexico_City")
	from := time.Date(2026, 9, 7, 0, 0, 0, 0, mx)
	to := time.Date(2026, 9, 21, 0, 0, 0, 0, mx)
	day := 24 * time.Hour
	approx := &issuesv1.MetricSource{Confidence: issuesv1.MetricConfidence_METRIC_CONFIDENCE_APPROXIMATE, Live: 1, ImportHistory: 2}
	lead := &issuesv1.DurationStats{Count: 2, Median: durationpb.New(9 * day), P85: durationpb.New(9*day + 15*time.Hour)}
	return &issuesv1.GetFlowMetricsResponse{
		From: timestamppb.New(from), To: timestamppb.New(to), TimeZone: in.GetTimeZone(),
		Weeks: []*issuesv1.FlowWeek{
			{Start: timestamppb.New(from), End: timestamppb.New(from.AddDate(0, 0, 7)), LeadTime: &issuesv1.DurationStats{},
				Source: &issuesv1.MetricSource{Confidence: issuesv1.MetricConfidence_METRIC_CONFIDENCE_NO_DATA}, Wip: 1},
			{Start: timestamppb.New(from.AddDate(0, 0, 7)), End: timestamppb.New(to), LeadTime: lead,
				CycleTime:   &issuesv1.DurationStats{Count: 1, Median: durationpb.New(7 * day), P85: durationpb.New(7 * day)},
				ClosedValue: 1, ClosedOther: 1, Wip: 1, Source: approx},
		},
		Summary: &issuesv1.FlowSummary{LeadTime: lead, ClosedValue: 1, ClosedOther: 1, Started: 2, Wip: 1, Source: approx},
		Stale: []*issuesv1.StaleIssue{
			{Number: 236, Title: "CLAUDE.md en capas", StatusId: stProg, InProgressSince: timestamppb.New(time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)),
				LastActivityAt: timestamppb.New(time.Date(2026, 8, 25, 15, 0, 0, 0, time.UTC)), Confidence: issuesv1.MetricConfidence_METRIC_CONFIDENCE_APPROXIMATE},
			{Number: 1, Title: "Baja", StatusId: stProg, InProgressSince: timestamppb.New(time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)),
				LastActivityAt: timestamppb.New(time.Date(2026, 8, 26, 15, 0, 0, 0, time.UTC)), Confidence: issuesv1.MetricConfidence_METRIC_CONFIDENCE_RECORDED},
		},
		StaleTotal:    2,
		ValueLabelIds: []string{lblInfra},
		Coverage:      &issuesv1.FlowCoverage{Issues: 5, DoneWithoutDates: 1},
		// Parte 3: historia de sesiones desde la segunda semana.
		SessionsRecordedSince: timestamppb.New(from.AddDate(0, 0, 8)),
		HiringSignal: &issuesv1.HiringSignal{
			Verdict: issuesv1.HiringVerdict_HIRING_VERDICT_INSUFFICIENT_DATA, LiveWeeks: 1, WeeksNeeded: 8, WeeksNeededPreliminary: 4,
		},
	}
}

// fakeEfficiency: 3 h trabajando y 5 h esperando a una persona (CU-036-6),
// 4 h de ellas por una revisión.
func fakeEfficiency() *issuesv1.FlowEfficiency {
	bp := int32(3750)
	return &issuesv1.FlowEfficiency{
		Working: durationpb.New(3 * time.Hour), WaitingHuman: durationpb.New(5 * time.Hour),
		Blocked: durationpb.New(0), InReview: durationpb.New(0), Idle: durationpb.New(time.Hour),
		EfficiencyBp: &bp, Issues: 1,
		WaitingHumanByReason: []*issuesv1.ReasonTime{
			{Reason: issuesv1.WaitingReason_WAITING_REASON_REVIEW, Duration: durationpb.New(4 * time.Hour)},
			{Reason: issuesv1.WaitingReason_WAITING_REASON_UNSPECIFIED, Duration: durationpb.New(time.Hour)},
		},
	}
}

func fakeIssueFlow(in *issuesv1.GetIssueFlowEfficiencyRequest) *issuesv1.GetIssueFlowEfficiencyResponse {
	if in.GetNumber() != 236 {
		return &issuesv1.GetIssueFlowEfficiencyResponse{}
	}
	return &issuesv1.GetIssueFlowEfficiencyResponse{
		Efficiency:    fakeEfficiency(),
		RecordedSince: timestamppb.New(time.Date(2026, 12, 1, 16, 0, 0, 0, time.UTC)),
		HumanQueue: &issuesv1.HumanQueue{Pending: 1, OldestPending: durationpb.New(90 * time.Minute),
			ResponseTime: &issuesv1.DurationStats{Count: 1, Median: durationpb.New(2 * time.Hour), P85: durationpb.New(2 * time.Hour)}},
	}
}

type flowDur struct {
	Count      int
	MedianDays *float64 `json:"median_days"`
	P85Days    *float64 `json:"p85_days"`
}

type flowResp struct {
	Project     string
	From        string
	To          string
	TimeZone    string   `json:"time_zone"`
	ValueLabels []string `json:"value_labels"`
	Summary     struct {
		LeadTime    flowDur `json:"lead_time"`
		ClosedValue int     `json:"closed_value"`
		ClosedOther int     `json:"closed_other"`
		Source      string
		Events      map[string]int `json:"events_by_source"`
	}
	Weeks []struct {
		Week     string
		End      string
		Partial  bool
		LeadTime flowDur `json:"lead_time"`
		Source   string
	}
	Stale []struct {
		Key, Status, Source string
		DaysIdle            int `json:"days_without_activity"`
	}
	StaleTotal       int `json:"stale_total"`
	DoneWithoutDates int `json:"done_without_dates"`
	Notes            []string
}

func TestMCPGetFlowMetrics(t *testing.T) {
	e := newMCPEnv(t)
	sess := e.mcpSession(e.creds.header("u-1", auth.ScopeIssuesRead))

	var out flowResp
	e.callOK(sess, "get_flow_metrics", map[string]any{
		"project": "rnd", "from": "2026-09-07", "to": "2026-09-20", "labels": []any{"INFRA"}, "stale": 1,
	}, &out)

	var req issuesv1.GetFlowMetricsRequest
	_ = proto.Unmarshal(lastRequest(t, e, "/issues.v1.IssueService/GetFlowMetrics"), &req)
	// Días en la zona por defecto (México, UTC-6); to incluye el día 20.
	if req.GetProjectId() != projRND || req.GetTimeZone() != "America/Mexico_City" ||
		!req.GetFrom().AsTime().Equal(time.Date(2026, 9, 7, 6, 0, 0, 0, time.UTC)) ||
		!req.GetTo().AsTime().Equal(time.Date(2026, 9, 21, 6, 0, 0, 0, time.UTC)) ||
		!slices.Equal(req.GetLabelIds(), []string{lblInfra}) || req.GetStaleLimit() != 1 {
		t.Fatalf("request a GetFlowMetrics = %v", &req)
	}

	if out.Project != "RND" || out.From != "2026-09-07" || out.To != "2026-09-20" || out.TimeZone != "America/Mexico_City" {
		t.Errorf("período = %s %s..%s %s", out.Project, out.From, out.To, out.TimeZone)
	}
	if !slices.Equal(out.ValueLabels, []string{"infra"}) {
		t.Errorf("etiquetas de valor = %v", out.ValueLabels)
	}
	s := out.Summary
	if s.LeadTime.Count != 2 || s.LeadTime.MedianDays == nil || *s.LeadTime.MedianDays != 9 || *s.LeadTime.P85Days != 9.6 ||
		s.ClosedValue != 1 || s.ClosedOther != 1 {
		t.Errorf("resumen = %+v", s)
	}
	if s.Source != "histórico aproximado" || s.Events["import_history"] != 2 || s.Events["live"] != 1 {
		t.Errorf("fuente del resumen = %q %v", s.Source, s.Events)
	}
	if len(out.Weeks) != 2 || out.Weeks[0].Week != "2026-09-07" || out.Weeks[0].Source != "sin datos" || out.Weeks[0].LeadTime.MedianDays != nil ||
		out.Weeks[1].Week != "2026-09-14" || out.Weeks[1].Source != "histórico aproximado" ||
		out.Weeks[0].End != "2026-09-13" || out.Weeks[0].Partial || out.Weeks[1].End != "2026-09-20" {
		t.Errorf("semanas = %+v", out.Weeks)
	}
	if len(out.Stale) != 1 || out.Stale[0].Key != "RND-236" || out.Stale[0].Status != "En curso" || out.Stale[0].DaysIdle != 26 || out.StaleTotal != 2 {
		t.Errorf("estancados = %+v (total %d)", out.Stale, out.StaleTotal)
	}
	notes := strings.Join(out.Notes, " ")
	if out.DoneWithoutDates != 1 || !strings.Contains(notes, "histórico aproximado") || !strings.Contains(notes, "no de personas") {
		t.Errorf("notas = %v", out.Notes)
	}

	// Sin fechas: no manda from ni to (el servicio pone 12 semanas).
	e.callOK(sess, "get_flow_metrics", map[string]any{"project": "RND", "time_zone": "UTC"}, &out)
	req.Reset()
	_ = proto.Unmarshal(lastRequest(t, e, "/issues.v1.IssueService/GetFlowMetrics"), &req)
	if req.From != nil || req.To != nil || req.GetTimeZone() != "UTC" || req.GetStaleLimit() != 10 {
		t.Errorf("request sin fechas = %v", &req)
	}
}

func TestMCPGetFlowMetricsArgumentosInvalidos(t *testing.T) {
	e := newMCPEnv(t)
	sess := e.mcpSession(e.creds.header("u-1", auth.ScopeIssuesRead))
	for name, args := range map[string]map[string]any{
		"zona inválida":        {"project": "RND", "time_zone": "Marte/Olympus"},
		"fecha mal escrita":    {"project": "RND", "from": "07/09/2026"},
		"etiqueta que no hay":  {"project": "RND", "labels": []any{"nope"}},
		"argumento de más":     {"project": "RND", "desde": "2026-09-07"},
		"stale fuera de rango": {"project": "RND", "stale": 51},
		"sin proyecto":         {"from": "2026-09-07"},
	} {
		res, text := e.callTool(sess, "get_flow_metrics", args)
		if !res.IsError || !strings.Contains(text, `"INVALID_ARGUMENT"`) {
			t.Errorf("%s: isError=%v %s", name, res.IsError, text)
		}
	}
	if _, text := e.callTool(sess, "get_flow_metrics", map[string]any{"project": "RND", "time_zone": "Marte/Olympus"}); !strings.Contains(text, "INVALID_TIME_ZONE") {
		t.Errorf("zona inválida sin reason: %s", text)
	}
	for _, r := range e.up.requests() {
		if r.method == "/issues.v1.IssueService/GetFlowMetrics" {
			t.Fatalf("un argumento inválido llegó a GetFlowMetrics")
		}
	}
}

// Parte 3 (CU-036-6): con issue, la eficiencia de ese issue en horas; sin
// historia, una nota que lo dice. El período y la señal salen en palabras.
func TestMCPGetFlowMetricsIssue(t *testing.T) {
	e := newMCPEnv(t)
	sess := e.mcpSession(e.creds.header("u-1", auth.ScopeIssuesRead))

	var out struct {
		Project, Issue string
		RecordedSince  string `json:"recorded_since"`
		TimeSpent      *struct {
			WorkingHours      float64            `json:"working_hours"`
			WaitingHumanHours float64            `json:"waiting_human_hours"`
			EfficiencyPct     *float64           `json:"efficiency_pct"`
			ByReason          map[string]float64 `json:"waiting_human_hours_by_reason"`
		} `json:"time_spent"`
		HumanQueue *struct {
			Pending             int
			OldestPendingHours  *float64 `json:"oldest_pending_hours"`
			ResponseMedianHours *float64 `json:"response_median_hours"`
		} `json:"human_queue"`
		Notes []string
	}
	e.callOK(sess, "get_flow_metrics", map[string]any{"project": "RND", "issue": "rnd-236", "from": "2026-12-01"}, &out)
	var req issuesv1.GetIssueFlowEfficiencyRequest
	_ = proto.Unmarshal(lastRequest(t, e, "/issues.v1.IssueService/GetIssueFlowEfficiency"), &req)
	if req.GetProjectId() != projRND || req.GetNumber() != 236 || !req.GetFrom().AsTime().Equal(time.Date(2026, 12, 1, 6, 0, 0, 0, time.UTC)) || req.To != nil {
		t.Fatalf("request = %v", &req)
	}
	ts := out.TimeSpent
	if out.Issue != "RND-236" || ts == nil || ts.WorkingHours != 3 || ts.WaitingHumanHours != 5 || ts.EfficiencyPct == nil || *ts.EfficiencyPct != 37.5 ||
		ts.ByReason["revision"] != 4 || ts.ByReason["sin motivo"] != 1 || out.RecordedSince != "2026-12-01" {
		t.Errorf("CU-036-6: %+v %+v", out, ts)
	}
	if q := out.HumanQueue; q == nil || q.Pending != 1 || q.OldestPendingHours == nil || *q.OldestPendingHours != 1.5 ||
		q.ResponseMedianHours == nil || *q.ResponseMedianHours != 2 {
		t.Errorf("cola: %+v", q)
	}

	// Un número suelto y sin historia: sin time_spent, con la nota.
	out.TimeSpent, out.Notes = nil, nil
	e.callOK(sess, "get_flow_metrics", map[string]any{"project": "RND", "issue": 7}, &out)
	if out.TimeSpent != nil || !strings.Contains(strings.Join(out.Notes, " "), "no tiene historia") {
		t.Errorf("sin historia: %+v", out)
	}
	// Un issue de otro proyecto o mal escrito no llega al servicio.
	for _, bad := range []map[string]any{
		{"project": "RND", "issue": "TK-3"},
		{"project": "RND", "issue": "abc"},
		{"project": "RND", "issue": -2},
		{"project": "RND", "issue": 236, "labels": []any{"no-existe"}},
		{"project": "RND", "issue": 236, "stale": 3},
	} {
		if res, text := e.callTool(sess, "get_flow_metrics", bad); !res.IsError || !strings.Contains(text, "INVALID_ARGUMENT") {
			t.Errorf("%v: %s", bad, text)
		}
	}
	for _, r := range e.up.requests() {
		if r.method == "/issues.v1.IssueService/GetIssueFlowEfficiency" {
			var got issuesv1.GetIssueFlowEfficiencyRequest
			_ = proto.Unmarshal(r.raw, &got)
			if got.GetNumber() == 236 && got.From == nil {
				t.Fatalf("un issue con labels o stale llegó al servicio: %v", &got)
			}
		}
	}
}

func TestMCPGetFlowMetricsSesionesYSenal(t *testing.T) {
	e := newMCPEnv(t)
	sess := e.mcpSession(e.creds.header("u-1", auth.ScopeIssuesRead))
	var out struct {
		SessionsSince string `json:"sessions_recorded_since"`
		HiringSignal  struct {
			Verdict, Summary string
			LiveWeeks        int `json:"live_weeks"`
			WeeksNeeded      int `json:"weeks_needed"`
		} `json:"hiring_signal"`
		Notes []string
	}
	e.callOK(sess, "get_flow_metrics", map[string]any{"project": "RND"}, &out)
	s := out.HiringSignal
	if out.SessionsSince != "2026-09-15" || s.Verdict != "datos insuficientes" || s.LiveWeeks != 1 || s.WeeksNeeded != 8 ||
		!strings.Contains(s.Summary, "hay 1 de las 8") {
		t.Errorf("señal: %+v (desde %s)", s, out.SessionsSince)
	}
	if !strings.Contains(strings.Join(out.Notes, " "), "una señal, no una decisión") {
		t.Errorf("notas: %v", out.Notes)
	}
}
