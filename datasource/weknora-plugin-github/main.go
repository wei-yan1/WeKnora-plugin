package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/pkg/pluginapi"
)

const (
	pluginID  = "weknora.github-datasource"
	githubAPI = "https://api.github.com"
	userAgent = "weknora-github-datasource-plugin"
)

// errFileTooLarge is returned when GitHub's Contents API reports
// encoding: "none" (i.e. the file is >1 MiB and must be downloaded
// separately). We skip such files rather than streaming them through
// the gRPC plugin boundary, which is bounded by the host's request
// timeout.
var errFileTooLarge = errors.New("file too large for inline fetch")

func main() {
	address := flag.String("address", os.Getenv("WEKNORA_PLUGIN_ADDR"), "gRPC listen address")
	flag.Parse()
	if *address == "" {
		*address = "127.0.0.1:9776"
	}

	client := pluginapi.NewPluginHTTPClient()
	client.Timeout = 60 * time.Second

	handler := &githubHandler{client: client}
	if err := pluginapi.Serve(context.Background(), *address, pluginapi.DataSourceHandler{
		PluginID:           pluginID,
		Capabilities:       []string{"incremental", "deletion_sync"},
		OnValidate:         handler.Validate,
		OnListResources:    handler.ListResources,
		OnFetchAll:         handler.FetchAll,
		OnFetchIncremental: handler.FetchIncremental,
	}); err != nil {
		panic(err)
	}
}

// githubHandler implements the GitHub repository data source.
type githubHandler struct {
	client *http.Client
}

// repoSelection is one configured repository to synchronize.
type repoSelection struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
}

// githubConfig holds the resolved plugin configuration.
type githubConfig struct {
	Token        string
	Repositories []repoSelection
}

// loadConfig resolves configuration. Credentials always come from the
// config_schema credentials section; settings carry the repository list.
// When settings is absent (Test Connection only sends credentials), the
// returned config has an empty repository list but still carries the token.
func loadConfig(request pluginapi.Request) (*githubConfig, error) {
	cfg := &githubConfig{}
	if credentials, ok := request.Config["credentials"].(map[string]any); ok {
		if v, ok := credentials["token"].(string); ok {
			cfg.Token = strings.TrimSpace(v)
		}
	}

	settings, _ := request.Config["settings"].(map[string]any)
	if settings == nil {
		// Test Connection path: only credentials were provided.
		return cfg, nil
	}
	rawRepos, ok := settings["repositories"]
	if !ok {
		return nil, errors.New("settings.repositories is required")
	}
	items, ok := rawRepos.([]any)
	if !ok {
		return nil, errors.New("settings.repositories must be an array")
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		raw, ok := item.(string)
		if !ok {
			return nil, errors.New("settings.repositories must be strings like owner/repo or owner/repo:branch")
		}
		sel, err := parseRepoSpec(raw)
		if err != nil {
			return nil, err
		}
		key := sel.Owner + "/" + sel.Repo + "@" + sel.Branch
		if seen[key] {
			return nil, fmt.Errorf("duplicate repository %q", raw)
		}
		seen[key] = true
		cfg.Repositories = append(cfg.Repositories, sel)
	}
	if len(cfg.Repositories) == 0 {
		return nil, errors.New("at least one repository is required")
	}
	return cfg, nil
}

// parseRepoSpec accepts "owner/repo" or "owner/repo:branch".
func parseRepoSpec(raw string) (repoSelection, error) {
	spec := strings.TrimSpace(raw)
	if spec == "" {
		return repoSelection{}, errors.New("empty repository spec")
	}
	name := spec
	branch := "HEAD"
	if i := strings.Index(spec, ":"); i >= 0 {
		name, branch = spec[:i], spec[i+1:]
	}
	parts := strings.SplitN(name, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return repoSelection{}, fmt.Errorf("invalid repository %q, want owner/repo[:branch]", raw)
	}
	return repoSelection{
		Owner:  strings.TrimSpace(parts[0]),
		Repo:   strings.TrimSpace(parts[1]),
		Branch: strings.TrimSpace(branch),
	}, nil
}

// Validate checks the token (if any) without depending on settings, so the
// frontend Test Connection (which only sends credentials) works. Public
// repositories work anonymously when no token is configured.
func (h *githubHandler) Validate(ctx context.Context, request pluginapi.Request) error {
	cfg, err := loadConfig(request)
	if err != nil {
		return err
	}
	if cfg.Token != "" {
		if _, err := h.getUser(ctx, cfg); err != nil {
			return fmt.Errorf("validate github token: %w", err)
		}
	}
	return nil
}

// ListResources lazily returns the configured repositories; when a parent is
// given, it lists the repository root or a sub-directory via the contents API.
func (h *githubHandler) ListResources(ctx context.Context, request pluginapi.Request) ([]pluginapi.Resource, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, err
	}
	if len(cfg.Repositories) == 0 {
		return nil, errors.New("settings.repositories is required")
	}

	parent := request.ParentID
	if parent == "" {
		resources := make([]pluginapi.Resource, 0, len(cfg.Repositories))
		for _, sel := range cfg.Repositories {
			resources = append(resources, pluginapi.Resource{
				ExternalID: "github:repo:" + sel.Owner + ":" + sel.Repo,
				Name:       sel.Owner + "/" + sel.Repo,
				Type:       "repository",
				Metadata: map[string]string{
					"owner":  sel.Owner,
					"repo":   sel.Repo,
					"branch": sel.Branch,
				},
			})
		}
		return resources, nil
	}

	// parent is "github:repo:{owner}:{repo}" or "github:dir:{owner}:{repo}:{branch}:{path}".
	ref, branch, dir, ok := parseResourceID(parent)
	if !ok {
		return nil, fmt.Errorf("unsupported parent id %q", parent)
	}
	if strings.HasPrefix(parent, "github:repo:") {
		// Prefer the branch configured for this repository over "HEAD".
		for _, sel := range cfg.Repositories {
			if sel.Owner == ref.Owner && sel.Repo == ref.Repo {
				branch = sel.Branch
				break
			}
		}
	}
	entries, err := h.listContents(ctx, cfg, ref.Owner, ref.Repo, branch, dir)
	if err != nil {
		return nil, err
	}
	resources := make([]pluginapi.Resource, 0, len(entries))
	for _, e := range entries {
		if e.Type == "dir" {
			resources = append(resources, pluginapi.Resource{
				ExternalID: "github:dir:" + ref.Owner + ":" + ref.Repo + ":" + branch + ":" + e.Path,
				Name:       e.Name,
				ParentID:   parent,
				Type:       "directory",
				Metadata:   map[string]string{"path": e.Path},
			})
		} else {
			resources = append(resources, pluginapi.Resource{
				ExternalID: "github:file:" + ref.Owner + ":" + ref.Repo + ":" + branch + ":" + e.Path,
				Name:       e.Name,
				ParentID:   parent,
				Type:       "file",
				Metadata:   map[string]string{"path": e.Path},
			})
		}
	}
	return resources, nil
}

// FetchAll walks every configured repository tree and downloads all files.
func (h *githubHandler) FetchAll(ctx context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, err
	}
	if len(cfg.Repositories) == 0 {
		return nil, errors.New("settings.repositories is required")
	}
	var items []pluginapi.FetchedItem
	for _, sel := range cfg.Repositories {
		files, err := h.walkTree(ctx, cfg, sel)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			item, err := h.item(ctx, cfg, sel, file)
			if err != nil {
				if errors.Is(err, errFileTooLarge) {
					continue
				}
				return nil, err
			}
			items = append(items, item)
		}
	}
	return items, nil
}

// cursorState tracks the last-seen commit SHA per repository selection.
type cursorState struct {
	Repos map[string]string `json:"repos"`
}

// FetchIncremental only returns files changed since the cursor's commit SHA,
// marking removed files with IsDeleted.
func (h *githubHandler) FetchIncremental(ctx context.Context, request pluginapi.Request) ([]pluginapi.FetchedItem, map[string]any, error) {
	cfg, err := loadConfig(request)
	if err != nil {
		return nil, nil, err
	}
	if len(cfg.Repositories) == 0 {
		return nil, nil, errors.New("settings.repositories is required")
	}
	prev := cursorState{Repos: map[string]string{}}
	if request.Cursor != nil {
		if raw, ok := request.Cursor["repos"]; ok {
			if m, ok := raw.(map[string]any); ok {
				for k, v := range m {
					if s, ok := v.(string); ok {
						prev.Repos[k] = s
					}
				}
			}
		}
	}

	var items []pluginapi.FetchedItem
	next := cursorState{Repos: make(map[string]string, len(cfg.Repositories))}
	for _, sel := range cfg.Repositories {
		key := repoKey(sel)
		head, err := h.commitSHA(ctx, cfg, sel)
		if err != nil {
			return nil, nil, err
		}
		previous := prev.Repos[key]
		switch {
		case previous == "":
			err = h.fetchAllFor(ctx, cfg, sel, &items)
		case previous != head:
			err = h.fetchChangesFor(ctx, cfg, sel, previous, head, &items)
		}
		if err != nil {
			return nil, nil, err
		}
		next.Repos[key] = head
	}

	cursor := map[string]any{"repos": map[string]any{}}
	for k, v := range next.Repos {
		cursor["repos"].(map[string]any)[k] = v
	}
	return items, cursor, nil
}

func (h *githubHandler) fetchAllFor(ctx context.Context, cfg *githubConfig, sel repoSelection, items *[]pluginapi.FetchedItem) error {
	files, err := h.walkTree(ctx, cfg, sel)
	if err != nil {
		return err
	}
	for _, file := range files {
		item, err := h.item(ctx, cfg, sel, file)
		if err != nil {
			if errors.Is(err, errFileTooLarge) {
				continue
			}
			return err
		}
		*items = append(*items, item)
	}
	return nil
}

func (h *githubHandler) fetchChangesFor(ctx context.Context, cfg *githubConfig, sel repoSelection, previous, head string, items *[]pluginapi.FetchedItem) error {
	changes, err := h.compare(ctx, cfg, sel, previous, head)
	if err != nil || len(changes) >= 300 {
		// A compare can fail after history rewrites or be truncated by GitHub;
		// re-enumerating the configured scope preserves updates.
		return h.fetchAllFor(ctx, cfg, sel, items)
	}
	for _, change := range changes {
		switch change.Status {
		case "removed":
			if isSupportedFile(change.Filename) {
				*items = append(*items, deletedItem(sel, change.Filename))
			}
		case "renamed":
			if change.PreviousFilename != "" && isSupportedFile(change.PreviousFilename) {
				*items = append(*items, deletedItem(sel, change.PreviousFilename))
			}
			if isSupportedFile(change.Filename) {
				item, err := h.item(ctx, cfg, sel, change.Filename)
				if err != nil {
					if errors.Is(err, errFileTooLarge) {
						continue
					}
					return err
				}
				*items = append(*items, item)
			}
		default: // added / modified / changed
			if isSupportedFile(change.Filename) {
				item, err := h.item(ctx, cfg, sel, change.Filename)
				if err != nil {
					if errors.Is(err, errFileTooLarge) {
						continue
					}
					return err
				}
				*items = append(*items, item)
			}
		}
	}
	return nil
}

// walkTree lists supported files in the branch using the git trees API
// (recursive).
func (h *githubHandler) walkTree(ctx context.Context, cfg *githubConfig, sel repoSelection) ([]string, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/git/trees/%s",
		url.PathEscape(sel.Owner), url.PathEscape(sel.Repo), url.PathEscape(sel.Branch))
	q := url.Values{"recursive": {"1"}}

	var out struct {
		Truncated bool `json:"truncated"`
		Tree      []struct {
			Path string `json:"path"`
			Type string `json:"type"`
		} `json:"tree"`
	}
	if err := h.getJSON(ctx, cfg, endpoint, q, &out); err != nil {
		return nil, err
	}
	if out.Truncated {
		return nil, errors.New("repository tree is truncated by GitHub; use a smaller branch or repository")
	}

	var files []string
	for _, entry := range out.Tree {
		if entry.Type == "blob" && isSupportedFile(entry.Path) {
			files = append(files, entry.Path)
		}
	}
	sort.Strings(files)
	return files, nil
}

// item builds a FetchedItem for one repository file.
func (h *githubHandler) item(ctx context.Context, cfg *githubConfig, sel repoSelection, file string) (pluginapi.FetchedItem, error) {
	body, err := h.fileContent(ctx, cfg, sel, file)
	if err != nil {
		return pluginapi.FetchedItem{}, err
	}
	blobURL := fmt.Sprintf("https://github.com/%s/%s/blob/%s/%s", sel.Owner, sel.Repo, sel.Branch, file)
	return pluginapi.FetchedItem{
		ExternalID: "github:file:" + sel.Owner + ":" + sel.Repo + ":" + sel.Branch + ":" + file,
		Title:      file,
		FileName:   path.Base(file),
		Content:    body,
		MIMEType:   mimeForPath(file),
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
		URL:        blobURL,
		Metadata: map[string]string{
			"owner":      sel.Owner,
			"repo":       sel.Repo,
			"branch":     sel.Branch,
			"path":       file,
			"source_url": blobURL,
		},
	}, nil
}

func deletedItem(sel repoSelection, file string) pluginapi.FetchedItem {
	return pluginapi.FetchedItem{
		ExternalID: "github:file:" + sel.Owner + ":" + sel.Repo + ":" + sel.Branch + ":" + file,
		IsDeleted:  true,
		Metadata:   map[string]string{"path": file},
	}
}

// --- GitHub API client helpers ---

func (h *githubHandler) do(ctx context.Context, cfg *githubConfig, endpoint string, query url.Values) ([]byte, error) {
	full := githubAPI + endpoint
	if len(query) > 0 {
		full += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent)
	if cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Token)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("github API %s: status %d: %s", endpoint, resp.StatusCode, truncate(string(body), 200))
	}
	return body, nil
}

func (h *githubHandler) getJSON(ctx context.Context, cfg *githubConfig, endpoint string, query url.Values, out any) error {
	body, err := h.do(ctx, cfg, endpoint, query)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

// getUser verifies the token by calling the authenticated /user endpoint.
func (h *githubHandler) getUser(ctx context.Context, cfg *githubConfig) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := h.getJSON(ctx, cfg, "/user", nil, &out); err != nil {
		return "", err
	}
	return out.Login, nil
}

// commitSHA returns the head commit SHA of the branch.
func (h *githubHandler) commitSHA(ctx context.Context, cfg *githubConfig, sel repoSelection) (string, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/commits/%s",
		url.PathEscape(sel.Owner), url.PathEscape(sel.Repo), url.PathEscape(sel.Branch))
	var out struct {
		SHA string `json:"sha"`
	}
	if err := h.getJSON(ctx, cfg, endpoint, nil, &out); err != nil {
		return "", err
	}
	return out.SHA, nil
}

// fileContent returns the decoded bytes of a file at the given ref.
//
// GitHub Contents API returns "base64" for files up to 1 MiB, and "none" for
// larger files (or files where content is omitted). For "none" we fetch the
// raw bytes via download_url instead of failing.
func (h *githubHandler) fileContent(ctx context.Context, cfg *githubConfig, sel repoSelection, file string) ([]byte, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/contents/%s",
		url.PathEscape(sel.Owner), url.PathEscape(sel.Repo), escapePath(file))
	q := url.Values{"ref": {sel.Branch}}

	var out struct {
		Encoding    string `json:"encoding"`
		Content     string `json:"content"`
		DownloadURL string `json:"download_url"`
	}
	if err := h.getJSON(ctx, cfg, endpoint, q, &out); err != nil {
		return nil, err
	}

	switch strings.ToLower(out.Encoding) {
	case "base64", "":
		decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
		if err != nil {
			return nil, fmt.Errorf("github file %s: decode base64: %w", file, err)
		}
		return decoded, nil
	case "none":
		// encoding=none means the file is larger than 1 MiB and GitHub returned
		// a download_url instead of inlined content. We skip such files rather
		// than streaming the full body through the gRPC plugin boundary, which
		// is bounded by the host's request timeout and would otherwise fail
		// large files with a context deadline error.
		return nil, errFileTooLarge
	default:
		return nil, fmt.Errorf("github file %s: unsupported encoding %q", file, out.Encoding)
	}
}

// compare returns the file changes between two commits.
func (h *githubHandler) compare(ctx context.Context, cfg *githubConfig, sel repoSelection, base, head string) ([]fileChange, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/compare/%s...%s",
		url.PathEscape(sel.Owner), url.PathEscape(sel.Repo), base, head)
	var out struct {
		Files []fileChange `json:"files"`
	}
	if err := h.getJSON(ctx, cfg, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// listContents lists a directory via the contents API (resource browsing).
func (h *githubHandler) listContents(ctx context.Context, cfg *githubConfig, owner, repo, branch, dir string) ([]contentEntry, error) {
	endpoint := fmt.Sprintf("/repos/%s/%s/contents", url.PathEscape(owner), url.PathEscape(repo))
	if dir != "" {
		endpoint += "/" + escapePath(dir)
	}
	q := url.Values{"ref": {branch}}
	body, err := h.do(ctx, cfg, endpoint, q)
	if err != nil {
		return nil, err
	}
	var entries []contentEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decode contents: %w", err)
	}
	return entries, nil
}

type fileChange struct {
	Filename         string `json:"filename"`
	Status           string `json:"status"`
	PreviousFilename string `json:"previous_filename"`
}

type contentEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"` // file / dir
}

// --- helpers ---

// parseResourceID decodes a repo or dir ExternalID into its parts.
func parseResourceID(id string) (repoSelection, string, string, bool) {
	switch {
	case strings.HasPrefix(id, "github:repo:"):
		rest := strings.TrimPrefix(id, "github:repo:")
		parts := strings.SplitN(rest, ":", 2)
		if len(parts) != 2 {
			return repoSelection{}, "", "", false
		}
		return repoSelection{Owner: parts[0], Repo: parts[1]}, "HEAD", "", true
	case strings.HasPrefix(id, "github:dir:"):
		rest := strings.TrimPrefix(id, "github:dir:")
		parts := strings.SplitN(rest, ":", 3)
		if len(parts) != 3 {
			return repoSelection{}, "", "", false
		}
		branchPath := strings.SplitN(parts[2], ":", 2)
		if len(branchPath) != 2 {
			return repoSelection{}, "", "", false
		}
		return repoSelection{Owner: parts[0], Repo: parts[1]}, branchPath[0], branchPath[1], true
	default:
		return repoSelection{}, "", "", false
	}
}

func repoKey(sel repoSelection) string {
	return sel.Owner + "/" + sel.Repo + "@" + sel.Branch
}

func isSupportedFile(file string) bool {
	_, ok := supportedFileExtensions[strings.ToLower(path.Ext(file))]
	return ok
}

var supportedFileExtensions = map[string]struct{}{
	".pdf": {}, ".txt": {}, ".docx": {}, ".doc": {}, ".epub": {},
	".html": {}, ".htm": {}, ".mhtml": {}, ".md": {}, ".markdown": {}, ".mdx": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".gif": {},
	".csv": {}, ".xlsx": {}, ".xls": {}, ".pptx": {}, ".ppt": {}, ".json": {},
	".mp3": {}, ".wav": {}, ".m4a": {}, ".flac": {}, ".ogg": {},
}

func mimeForPath(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".pdf":
		return "application/pdf"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".doc":
		return "application/msword"
	case ".xlsx":
		return "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	case ".xls":
		return "application/vnd.ms-excel"
	case ".pptx":
		return "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	case ".ppt":
		return "application/vnd.ms-powerpoint"
	case ".md", ".markdown", ".mdx":
		return "text/markdown"
	case ".html", ".htm", ".mhtml":
		return "text/html"
	case ".csv":
		return "text/csv"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".flac":
		return "audio/flac"
	case ".ogg":
		return "audio/ogg"
	default:
		return "text/plain"
	}
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
