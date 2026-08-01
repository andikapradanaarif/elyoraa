package gh

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andikapradanaarif/elyoraa/internal/domain"
	ghinst "github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v66/github"
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
	Files       []*FileInfo
	PatchText   string
	PatchTooBig bool
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

func (c *Client) installationClient(ctx context.Context, installationID int64) *github.Client {
	tr := c.installationTransport(installationID)
	httpClient := &http.Client{Transport: tr, Timeout: 30 * time.Second}
	var client *github.Client
	if c.baseURL != "" {
		var err error
		client, err = github.NewEnterpriseClient(c.baseURL, c.baseURL+"/uploads", httpClient)
		if err != nil {
			// Should not happen with valid URL; fall back to standard client.
			client = github.NewClient(httpClient)
		}
	} else {
		client = github.NewClient(httpClient)
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
	}

	var files []*github.CommitFile
	page := 1
	for {
		pageFiles, resp, err := client.PullRequests.ListFiles(ctx, owner, repo, number, &github.ListOptions{Page: page, PerPage: 100})
		if err != nil {
			return nil, fmt.Errorf("list files: %w", err)
		}
		files = append(files, pageFiles...)
		if resp.NextPage == 0 || len(files) >= maxFiles {
			break
		}
		page = resp.NextPage
	}

	if len(files) > maxFiles {
		files = files[:maxFiles]
		info.PatchTooBig = true
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
func (c *Client) PostReview(ctx context.Context, installationID int64, owner, repo string, number int, summary string, comments []FileComment, headSHA string) error {
	client := c.installationClient(ctx, installationID)

	draftComments := make([]*github.DraftReviewComment, 0, len(comments))
	for _, cm := range comments {
		if cm.Position < 1 {
			continue
		}
		draftComments = append(draftComments, &github.DraftReviewComment{
			Path:     github.String(cm.Path),
			Position: github.Int(cm.Position),
			Body:     github.String(cm.Body),
			Side:     github.String("RIGHT"),
		})
	}

	_, _, err := client.PullRequests.CreateReview(ctx, owner, repo, number, &github.PullRequestReviewRequest{
		Body:     github.String(summary),
		Event:    github.String("COMMENT"),
		Comments: draftComments,
		CommitID: github.String(headSHA),
	})
	if err != nil {
		return fmt.Errorf("create review: %w", err)
	}
	return nil
}

// FileComment is a comment mapped to a diff position.
type FileComment struct {
	Path     string
	Position int
	Body     string
}

// PostPendingComment posts a temporary "Review in progress…" issue comment.
func (c *Client) PostPendingComment(ctx context.Context, installationID int64, owner, repo string, number int, body string) (int64, error) {
	client := c.installationClient(ctx, installationID)
	cm, _, err := client.Issues.CreateComment(ctx, owner, repo, number, &github.IssueComment{Body: github.String(body)})
	if err != nil {
		return 0, fmt.Errorf("create pending comment: %w", err)
	}
	return cm.GetID(), nil
}

// EditComment updates an existing issue/PR comment.
func (c *Client) EditComment(ctx context.Context, installationID int64, owner, repo string, commentID int64, body string) error {
	client := c.installationClient(ctx, installationID)
	_, _, err := client.Issues.EditComment(ctx, owner, repo, commentID, &github.IssueComment{Body: github.String(body)})
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
	positions := buildPositionMap(files)
	out := make([]FileComment, 0, len(comments))
	for _, cm := range comments {
		key := positionKey{path: cm.Path, line: cm.Line}
		pos, ok := positions[key]
		if !ok {
			continue
		}
		out = append(out, FileComment{
			Path:     cm.Path,
			Position: pos,
			Body:     fmt.Sprintf("**%s**: %s", strings.ToUpper(cm.Severity), cm.Body),
		})
	}
	// Sort by path then position for deterministic output.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Position < out[j].Position
	})
	return out
}

type positionKey struct {
	path string
	line int
}

func buildPositionMap(files []*FileInfo) map[positionKey]int {
	m := make(map[positionKey]int)
	for _, f := range files {
		if f.Patch == "" {
			continue
		}
		newLine := 0
		pos := 0
		lines := strings.Split(f.Patch, "\n")
		inHunk := false
		for _, line := range lines {
			if strings.HasPrefix(line, "@@") {
				inHunk = true
				// Parse hunk header: @@ -oldStart,oldCount +newStart,newCount @@
				newLine, _ = parseHunkNewStart(line)
				pos++
				continue
			}
			if !inHunk {
				continue
			}
			if line == "" {
				continue
			}
			pos++
			switch {
			case strings.HasPrefix(line, "+"):
				m[positionKey{path: f.Path, line: newLine}] = pos
				newLine++
			case strings.HasPrefix(line, " "):
				m[positionKey{path: f.Path, line: newLine}] = pos
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
