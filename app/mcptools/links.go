package mcptools

import (
	"context"
	"strings"

	issuesv1 "github.com/tekitla/tekitla-protos/gen/go/issues/v1"

	"github.com/tekitla/tekitla-gateway/app/mcpserver"
)

// Ramas y PRs ligados a un issue (issue 016): bloque `links` de get_issue.
// Sale de RepositoryService/ListIssueLinks con la política de su ruta
// (allowlist o issues:read).

const mListLinks = "issues.v1.RepositoryService/ListIssueLinks"

type linkOut struct {
	Repo      string `json:"repo"` // acme/api
	Branch    string `json:"branch"`
	PR        int32  `json:"pr,omitempty"`
	PRTitle   string `json:"pr_title,omitempty"`
	PRState   string `json:"pr_state"` // none, draft, open, merged, closed
	CI        string `json:"ci"`       // none, pending, success, failed, canceled
	Review    bool   `json:"review_requested,omitempty"`
	Commit    string `json:"commit,omitempty"` // primera línea del último commit
	Deleted   bool   `json:"branch_deleted,omitempty"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updated_at"`
}

// issueLinks lee los links de un issue. Un proyecto sin repos da lista vacía.
func issueLinks(ctx context.Context, c *mcpserver.Caller, projectID, number string) ([]linkOut, error) {
	var resp issuesv1.ListIssueLinksResponse
	if err := get(ctx, c, mListLinks, map[string]any{"project_id": projectID, "issue_number": number}, &resp); err != nil {
		return nil, err
	}
	out := make([]linkOut, 0, len(resp.GetLinks()))
	for _, l := range resp.GetLinks() {
		repo := ""
		if r := l.GetRepo(); r.GetOwner() != "" && r.GetName() != "" {
			repo = r.GetOwner() + "/" + r.GetName()
		}
		out = append(out, linkOut{
			Repo:      repo,
			Branch:    l.GetBranch(),
			PR:        l.GetPrNumber(),
			PRTitle:   l.GetPrTitle(),
			PRState:   enumName(l.GetPrState().String(), "PR_STATE_"),
			CI:        enumName(l.GetCiStatus().String(), "CI_STATUS_"),
			Review:    l.GetReviewRequested(),
			Commit:    l.GetHeadTitle(),
			Deleted:   l.GetBranchDeleted(),
			URL:       l.GetWebUrl(),
			UpdatedAt: rfc3339(l.GetUpdatedAt()),
		})
	}
	return out, nil
}

// enumName pasa PR_STATE_OPEN a open.
func enumName(s, prefix string) string {
	return strings.ToLower(strings.TrimPrefix(s, prefix))
}
