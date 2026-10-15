package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	ghinst "github.com/bradleyfalzon/ghinstallation/v2"
	go_github "github.com/google/go-github/v88/github"
)

// Client wraps GitHub App auth and common PR operations.
type Client struct {
	apps    *ghinst.AppsTransport
	baseURL string
}

// PRInfo holds pull request metadata and patch data.
type PRInfo struct {
	Owner       string
	Repo        string
	Number      int
	Title       string
	Body        string
	Author      string
	BaseSHA     string
	HeadSHA     string
	IsDraft     bool
	Merged      bool
	Closed      bool
	Files       []*FileInfo
	PatchText   string
	PatchTooBig bool
	// FilesTruncated is true when the changed-file list hit the maxFiles cap,
	// so the real PR size is larger than what Files describes.
	FilesTruncated bool
}

// FileInfo holds per-file diff data.
type FileInfo struct {
	Path      string
	Patch     string
	Status    string
	Additions int
	Deletions int
}

// NewClient creates a GitHub App client from a private key file.
func NewClient(appID int64, privateKeyPath string) (*Client, error) {
	apps, err := ghinst.NewAppsTransportKeyFromFile(http.DefaultTransport, appID, privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github app transport: %w", err)
	}
	return &Client{apps: apps}, nil
}

// NewEnterpriseClient is like NewClient but for a GitHub Enterprise instance.
func NewEnterpriseClient(appID int64, privateKeyPath, baseURL string) (*Client, error) {
	c, err := NewClient(appID, privateKeyPath)
	if err != nil {
		return nil, err
	}
	if baseURL != "" {
		c.baseURL = strings.TrimSuffix(baseURL, "/")
		c.apps.BaseURL = c.baseURL
	}
	return c, nil
}

func (c *Client) installationTransport(installationID int64) *ghinst.Transport {
	return ghinst.NewFromAppsTransport(c.apps, installationID)
}

func (c *Client) installationClient(ctx context.Context, installationID int64) *go_github.Client {
	tr := c.installationTransport(installationID)
	httpClient := &http.Client{Transport: tr, Timeout: 30 * time.Second}

	opts := []go_github.ClientOptionsFunc{go_github.WithHTTPClient(httpClient)}
	if c.baseURL != "" {
		opts = append(opts, go_github.WithEnterpriseURLs(c.baseURL, c.baseURL+"/uploads"))
	}

	client, err := go_github.NewClient(opts...)
	if err != nil {
		// Should not happen with valid URL; fall back to a default client.
		slog.Error("github client", "error", err)
		client, _ = go_github.NewClient()
	}
	return client
}

// VerifyWebhookSignature validates the HMAC-SHA256 signature of a webhook payload.
func (c *Client) VerifyWebhookSignature(payload []byte, signature string, secret string) error {
	if !strings.HasPrefix(signature, "sha256=") {
		return fmt.Errorf("missing sha256= prefix")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(signature, "sha256="))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// InstallationToken returns a short-lived access token for git operations.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	tr := c.installationTransport(installationID)
	tok, err := tr.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("installation token: %w", err)
	}
	return tok, nil
}

// GetPR fetches PR metadata and changed files with pagination.
func (c *Client) GetPR(ctx context.Context, installationID int64, owner, repo string, number int, maxPatchBytes, maxFiles int) (*PRInfo, error) {
	client := c.installationClient(ctx, installationID)

	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get pr: %w", err)
	}

	info := &PRInfo{
		Owner:   owner,
		Repo:    repo,
		Number:  number,
		Title:   pr.GetTitle(),
		Body:    pr.GetBody(),
		Author:  pr.GetUser().GetLogin(),
		BaseSHA: pr.GetBase().GetSHA(),
		HeadSHA: pr.GetHead().GetSHA(),
		IsDraft: pr.GetDraft(),
		Merged:  pr.GetMerged(),
		Closed:  pr.GetState() == "closed",
	}

	var files []*go_github.CommitFile
	page := 1
	for {
		pageFiles, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, number, &go_github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list files: %w", err)
		}
		files = append(files, pageFiles...)
		if resp.NextPage == 0 {
			break
		}
		if len(files) >= maxFiles {
			info.FilesTruncated = true
			break
		}
		page = resp.NextPage
	}

	if len(files) > maxFiles {
		files = files[:maxFiles]
		info.PatchTooBig = true
		info.FilesTruncated = true
	}

	var patchBuilder strings.Builder
	for _, f := range files {
		fi := &FileInfo{
			Path:      f.GetFilename(),
			Patch:     f.GetPatch(),
			Status:    f.GetStatus(),
			Additions: f.GetAdditions(),
			Deletions: f.GetDeletions(),
		}
		info.Files = append(info.Files, fi)

		if fi.Patch == "" {
			continue
		}
		if patchBuilder.Len()+len(fi.Patch) > maxPatchBytes {
			info.PatchTooBig = true
			break
		}
		patchBuilder.WriteString(fmt.Sprintf("--- a/%s\n+++ b/%s\n", fi.Path, fi.Path))
		patchBuilder.WriteString(fi.Patch)
		patchBuilder.WriteString("\n")
	}

	info.PatchText = patchBuilder.String()
	if patchBuilder.Len() > maxPatchBytes {
		info.PatchTooBig = true
	}

	return info, nil
}

// PostReview posts a summary review with inline positional comments.
// state is one of "APPROVE", "REQUEST_CHANGES", or "COMMENT".
func (c *Client) PostReview(ctx context.Context, installationID int64, owner, repo string, number int, summary string, comments []FileComment, headSHA, state string) error {
	client := c.installationClient(ctx, installationID)

	draftComments := make([]*go_github.DraftReviewComment, 0, len(comments))
	for _, cm := range comments {
		if cm.Line < 1 {
			continue
		}
		draftComments = append(draftComments, &go_github.DraftReviewComment{
			Path: go_github.Ptr(cm.Path),
			Line: go_github.Ptr(cm.Line),
			Side: go_github.Ptr("RIGHT"),
			Body: go_github.Ptr(redactSecrets(cm.Body)),
		})
	}

	_, _, err := client.PullRequests.CreateReview(ctx, owner, repo, number, &go_github.PullRequestReviewRequest{
		Body:     go_github.Ptr(redactSecrets(summary)),
		Event:    go_github.Ptr(state),
		Comments: draftComments,
		CommitID: go_github.Ptr(headSHA),
	})
	if err != nil {
		return fmt.Errorf("create review: %w", err)
	}
	return nil
}

// FileComment is a comment mapped to a diff line.
type FileComment struct {
	Path string
	Line int
	Body string
}

// IssueComment is a single comment on an issue (or PR conversation).
type IssueComment struct {
	ID        int64
	Author    string
	Body      string
	CreatedAt time.Time
	IsBot     bool
}

// ReviewSummary is a submitted PR review (approval, changes-requested, or
// plain comment) — the review-level body, not its inline comments.
type ReviewSummary struct {
	ID          int64
	Author      string
	IsBot       bool
	State       string // APPROVED, CHANGES_REQUESTED, COMMENTED, …
	Body        string
	SubmittedAt time.Time
}

// ListReviewSummaries returns submitted PR reviews, oldest first, capped.
func (c *Client) ListReviewSummaries(ctx context.Context, installationID int64, owner, repo string, number int) ([]ReviewSummary, error) {
	client := c.installationClient(ctx, installationID)

	var out []ReviewSummary
	page := 1
	for {
		reviews, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, &go_github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list reviews: %w", err)
		}
		for _, rv := range reviews {
			author := rv.GetUser().GetLogin()
			out = append(out, ReviewSummary{
				ID:          rv.GetID(),
				Author:      author,
				IsBot:       strings.HasSuffix(strings.ToLower(author), "[bot]"),
				State:       rv.GetState(),
				Body:        rv.GetBody(),
				SubmittedAt: rv.GetSubmittedAt().Time,
			})
		}
		if resp.NextPage == 0 || len(out) >= 200 {
			return out, nil
		}
		page = resp.NextPage
	}
}

// PullReviewComment is an inline comment left on a PR diff.
type PullReviewComment struct {
	ID        int64
	Author    string
	IsBot     bool
	Path      string
	Line      int
	Body      string
	InReplyTo int64 // parent comment ID when this is a reply in a thread
	CreatedAt time.Time
}

// ListPullReviewComments returns inline PR review comments, oldest first,
// capped.
func (c *Client) ListPullReviewComments(ctx context.Context, installationID int64, owner, repo string, number int) ([]PullReviewComment, error) {
	client := c.installationClient(ctx, installationID)

	var out []PullReviewComment
	page := 1
	for {
		comments, resp, err := client.PullRequests.ListComments(ctx, owner, repo, number, &go_github.PullRequestListCommentsOptions{
			ListOptions: go_github.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, fmt.Errorf("list review comments: %w", err)
		}
		for _, cm := range comments {
			author := cm.GetUser().GetLogin()
			// Outdated comments report Line==0 — OriginalLine keeps their
			// anchor instead of rendering `path:0`.
			line := cm.GetLine()
			if line == 0 {
				line = cm.GetOriginalLine()
			}
			out = append(out, PullReviewComment{
				ID:        cm.GetID(),
				Author:    author,
				IsBot:     strings.HasSuffix(strings.ToLower(author), "[bot]"),
				Path:      cm.GetPath(),
				Line:      line,
				Body:      cm.GetBody(),
				InReplyTo: cm.GetInReplyTo(),
				CreatedAt: cm.GetCreatedAt().Time,
			})
		}
		if resp.NextPage == 0 || len(out) >= 300 {
			return out, nil
		}
		page = resp.NextPage
	}
}

// ListIssueComments returns the comment thread for an issue, oldest first.
// It is bounded by maxComments and maxBytes so threads stay prompt-sized.
func (c *Client) ListIssueComments(ctx context.Context, installationID int64, owner, repo string, number, maxComments, maxBytes int) ([]IssueComment, error) {
	client := c.installationClient(ctx, installationID)

	var out []IssueComment
	totalBytes := 0
	page := 1
	for {
		comments, resp, err := client.Issues.ListComments(ctx, owner, repo, number, &go_github.IssueListCommentsOptions{
			Sort:        go_github.Ptr("created"),
			Direction:   go_github.Ptr("asc"),
			ListOptions: go_github.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, fmt.Errorf("list issue comments: %w", err)
		}
		for _, cm := range comments {
			if len(out) >= maxComments || totalBytes >= maxBytes {
				return out, nil
			}
			body := cm.GetBody()
			totalBytes += len(body)
			author := cm.GetUser().GetLogin()
			out = append(out, IssueComment{
				ID:        cm.GetID(),
				Author:    author,
				Body:      body,
				CreatedAt: cm.GetCreatedAt().Time,
				IsBot:     strings.HasSuffix(strings.ToLower(author), "[bot]"),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		page = resp.NextPage
	}
}

// CreatedIssue is a GitHub issue created by the bot.
type CreatedIssue struct {
	Number int
	URL    string
	Title  string
}

// CreateIssue creates a GitHub issue with the given title, body, and labels.
func (c *Client) CreateIssue(ctx context.Context, installationID int64, owner, repo, title, body string, labels []string) (*CreatedIssue, error) {
	client := c.installationClient(ctx, installationID)

	req := &go_github.IssueRequest{
		Title: go_github.Ptr(redactSecrets(title)),
		Body:  go_github.Ptr(redactSecrets(body)),
	}
	if len(labels) > 0 {
		req.Labels = &labels
	}
	issue, _, err := client.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		return nil, fmt.Errorf("create issue: %w", err)
	}
	return &CreatedIssue{Number: issue.GetNumber(), URL: issue.GetHTMLURL(), Title: issue.GetTitle()}, nil
}

// ListLabels returns the names of all labels in the repository.
func (c *Client) ListLabels(ctx context.Context, installationID int64, owner, repo string) ([]string, error) {
	client := c.installationClient(ctx, installationID)

	var names []string
	page := 1
	for {
		labels, resp, err := client.Issues.ListLabels(ctx, owner, repo, &go_github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list labels: %w", err)
		}
		for _, l := range labels {
			names = append(names, l.GetName())
		}
		if resp.NextPage == 0 {
			return names, nil
		}
		page = resp.NextPage
	}
}

// CreateLabel creates a label. No error is returned if it already exists.
func (c *Client) CreateLabel(ctx context.Context, installationID int64, owner, repo, name, color, description string) error {
	client := c.installationClient(ctx, installationID)
	_, _, err := client.Issues.CreateLabel(ctx, owner, repo, &go_github.Label{
		Name:        go_github.Ptr(name),
		Color:       go_github.Ptr(color),
		Description: go_github.Ptr(description),
	})
	if err != nil {
		var ghErr *go_github.ErrorResponse
		if errors.As(err, &ghErr) && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
			return nil // already exists
		}
		return fmt.Errorf("create label %q: %w", name, err)
	}
	return nil
}

// labelAutoCreate is the vocabulary of labels the bot may create when missing,
// mapped to their GitHub default colors. Anything else is only applied when it
// already exists in the repository.
var labelAutoCreate = map[string]string{
	"elyoraa":          "ededed",
	"bug":              "d73a4a",
	"documentation":    "0075ca",
	"enhancement":      "a2eeef",
	"good first issue": "7057ff",
	"help wanted":      "008672",
	"question":         "d876e3",
	"refactor":         "1f6feb",
	"chore":            "ededed",
	"testing":          "bfdadc",
	"performance":      "f9d0c4",
	"security":         "e99695",
	"tech-debt":        "f9d0c4",
	"dependencies":     "0366d6",
	"ci":               "bfd4f2",
	"epic":             "3e4b9e",
}

// ResolveLabels maps suggested label names to labels that exist (or are
// safely auto-created) in the repo. Names that can't be matched or created are
// dropped. always contains labels that must be present (e.g. the bot tag).
// Returns nil on API failure so issue creation can proceed unlabeled.
func (c *Client) ResolveLabels(ctx context.Context, installationID int64, owner, repo string, always, suggested []string) map[string]string {
	existing, err := c.ListLabels(ctx, installationID, owner, repo)
	if err != nil {
		slog.Warn("could not list repo labels; creating issues unlabeled", "owner", owner, "repo", repo, "error", err)
		return nil
	}

	byLower := make(map[string]string, len(existing))
	for _, n := range existing {
		byLower[strings.ToLower(n)] = n
	}

	out := make(map[string]string)
	for i, name := range append(append([]string{}, always...), suggested...) {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		mandatory := i < len(always)
		lower := strings.ToLower(name)
		if actual, ok := byLower[lower]; ok {
			out[lower] = actual
			continue
		}
		color, ok := labelAutoCreate[lower]
		if !ok {
			if !mandatory {
				continue
			}
			color = "ededed"
		}
		if err := c.CreateLabel(ctx, installationID, owner, repo, name, color, "created by elyoraa"); err != nil {
			slog.Warn("could not create label", "owner", owner, "repo", repo, "label", name, "error", err)
			continue
		}
		byLower[lower] = name
		out[lower] = name
	}
	return out
}

// PriorReply is a threaded reply on a previous review finding.
type PriorReply struct {
	Author string
	Body   string
	IsBot  bool
}

// PriorFinding is one inline comment left by a previous bot review.
type PriorFinding struct {
	Path     string
	Line     int
	Body     string
	Outdated bool // the diff moved since the comment was posted
	Replies  []PriorReply
}

// PreviousReview is the most recent bot-authored review on a PR, with its
// inline findings and any replies from the discussion.
type PreviousReview struct {
	State       string
	Body        string
	SubmittedAt time.Time
	Findings    []PriorFinding
}

// GetPreviousReview returns the most recent bot-authored review on the PR
// (matched by the bot handle when set), including the inline findings that
// belong to that review and their reply threads. Returns nil, nil when no
// prior bot review exists. Fetches at most maxFindings findings.
func (c *Client) GetPreviousReview(ctx context.Context, installationID int64, owner, repo string, number int, botHandle string, maxFindings int) (*PreviousReview, error) {
	client := c.installationClient(ctx, installationID)

	isOurBot := func(login string) bool {
		l := strings.ToLower(login)
		if !strings.HasSuffix(l, "[bot]") {
			return false
		}
		return botHandle == "" || strings.Contains(l, strings.ToLower(botHandle))
	}

	var reviews []*go_github.PullRequestReview
	page := 1
	for {
		rv, resp, err := client.PullRequests.ListReviews(ctx, owner, repo, number, &go_github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list reviews: %w", err)
		}
		reviews = append(reviews, rv...)
		if resp.NextPage == 0 || len(reviews) >= 100 {
			break
		}
		page = resp.NextPage
	}

	sort.Slice(reviews, func(i, j int) bool {
		return reviews[i].GetSubmittedAt().Time.After(reviews[j].GetSubmittedAt().Time)
	})

	var prev *go_github.PullRequestReview
	for _, r := range reviews {
		if isOurBot(r.GetUser().GetLogin()) {
			prev = r
			break
		}
	}
	if prev == nil {
		return nil, nil
	}

	out := &PreviousReview{
		State:       prev.GetState(),
		Body:        prev.GetBody(),
		SubmittedAt: prev.GetSubmittedAt().Time,
	}
	if maxFindings <= 0 {
		return out, nil
	}

	var comments []*go_github.PullRequestComment
	page = 1
	for len(comments) < 300 {
		cm, resp, err := client.PullRequests.ListComments(ctx, owner, repo, number, &go_github.PullRequestListCommentsOptions{
			Sort:        "created",
			Direction:   "asc",
			ListOptions: go_github.ListOptions{Page: page, PerPage: 100},
		})
		if err != nil {
			return nil, fmt.Errorf("list review comments: %w", err)
		}
		comments = append(comments, cm...)
		if resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}

	reviewID := prev.GetID()
	replies := make(map[int64][]PriorReply)
	for _, cm := range comments {
		if parent := cm.GetInReplyTo(); parent != 0 {
			replies[parent] = append(replies[parent], PriorReply{
				Author: cm.GetUser().GetLogin(),
				Body:   cm.GetBody(),
				IsBot:  strings.HasSuffix(strings.ToLower(cm.GetUser().GetLogin()), "[bot]"),
			})
		}
	}
	for _, cm := range comments {
		if len(out.Findings) >= maxFindings {
			break
		}
		if cm.GetInReplyTo() != 0 || cm.GetPullRequestReviewID() != reviewID || !isOurBot(cm.GetUser().GetLogin()) {
			continue
		}
		line := cm.GetLine()
		if line == 0 {
			line = cm.GetOriginalLine()
		}
		out.Findings = append(out.Findings, PriorFinding{
			Path:     cm.GetPath(),
			Line:     line,
			Body:     cm.GetBody(),
			Outdated: cm.Position == nil,
			Replies:  replies[cm.GetID()],
		})
	}
	return out, nil
}

// PostPendingComment posts a temporary "Review in progress…" issue comment.
func (c *Client) PostPendingComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (int64, error) {
	client := c.installationClient(ctx, installationID)
	cm, _, err := client.Issues.CreateComment(ctx, owner, repo, number, &go_github.IssueComment{Body: go_github.Ptr(redactSecrets(body))})
	if err != nil {
		return 0, fmt.Errorf("create pending comment: %w", err)
	}
	return cm.GetID(), nil
}

// EditComment updates an existing issue/PR comment.
func (c *Client) EditComment(ctx context.Context, installationID int64, owner, repo string, commentID int64, body string) error {
	client := c.installationClient(ctx, installationID)
	_, _, err := client.Issues.EditComment(ctx, owner, repo, commentID, &go_github.IssueComment{Body: go_github.Ptr(redactSecrets(body))})
	if err != nil {
		return fmt.Errorf("edit comment: %w", err)
	}
	return nil
}

// DeleteComment removes an issue/PR comment.
func (c *Client) DeleteComment(ctx context.Context, installationID int64, owner, repo string, commentID int64) error {
	client := c.installationClient(ctx, installationID)
	_, err := client.Issues.DeleteComment(ctx, owner, repo, commentID)
	if err != nil {
		return fmt.Errorf("delete comment: %w", err)
	}
	return nil
}

// MapCommentsToPositions maps review comments (path+new line) to GitHub diff positions.
func (c *Client) MapCommentsToPositions(files []*FileInfo, comments []domain.Comment) []FileComment {
	lineSet := buildLineSet(files)
	out := make([]FileComment, 0, len(comments))
	for _, cm := range comments {
		key := positionKey{path: cm.Path, line: cm.Line}
		if !lineSet[key] {
			continue
		}
		out = append(out, FileComment{
			Path: cm.Path,
			Line: cm.Line,
			Body: fmt.Sprintf("**%s**: %s", strings.ToUpper(cm.Severity), cm.Body),
		})
	}
	// Sort by path then line for deterministic output.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Line < out[j].Line
	})
	return out
}

type positionKey struct {
	path string
	line int
}

// buildLineSet returns the set of (path, line) pairs that exist in the
// diff's new side. A comment can be posted on any of these lines using
// the Side+Line form.
func buildLineSet(files []*FileInfo) map[positionKey]bool {
	m := make(map[positionKey]bool)
	for _, f := range files {
		if f.Patch == "" {
			continue
		}
		newLine := 0
		lines := strings.Split(f.Patch, "\n")
		inHunk := false
		for _, line := range lines {
			if strings.HasPrefix(line, "@@") {
				inHunk = true
				newLine, _ = parseHunkNewStart(line)
				continue
			}
			if !inHunk || line == "" {
				continue
			}
			switch {
			case strings.HasPrefix(line, "+"):
				m[positionKey{path: f.Path, line: newLine}] = true
				newLine++
			case strings.HasPrefix(line, " "):
				m[positionKey{path: f.Path, line: newLine}] = true
				newLine++
			case strings.HasPrefix(line, "-"):
				// removed line, no new line number
			}
		}
	}
	return m
}

func parseHunkNewStart(header string) (int, error) {
	// header format: "@@ -oldStart,oldCount +newStart,newCount @@"
	parts := strings.Split(header, " ")
	for i, p := range parts {
		if strings.HasPrefix(p, "+") && i > 0 {
			p = strings.TrimPrefix(p, "+")
			p = strings.TrimSuffix(p, ",")
			if idx := strings.Index(p, ","); idx > 0 {
				p = p[:idx]
			}
			n, err := strconv.Atoi(p)
			if err != nil {
				return 0, err
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("no new hunk start")
}
