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
