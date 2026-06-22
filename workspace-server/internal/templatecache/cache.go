package templatecache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type ManifestEntry struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
	Ref  string `json:"ref"`
}

type manifestFile struct {
	WorkspaceTemplates []ManifestEntry `json:"workspace_templates"`
}

type TemplateResult struct {
	Name   string `json:"name"`
	Repo   string `json:"repo"`
	Ref    string `json:"ref"`
	SHA    string `json:"sha,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type RefreshReport struct {
	ManifestPath string           `json:"manifest_path"`
	CacheDir     string           `json:"cache_dir"`
	RefreshedAt  time.Time        `json:"refreshed_at"`
	Results      []TemplateResult `json:"results"`
}

func RefreshWorkspaceTemplates(ctx context.Context, manifestPath, cacheDir, token string) (RefreshReport, error) {
	report := RefreshReport{
		ManifestPath: manifestPath,
		CacheDir:     cacheDir,
		RefreshedAt:  time.Now().UTC(),
	}
	if strings.TrimSpace(token) == "" {
		return report, fmt.Errorf("template cache refresh requires MOLECULE_TEMPLATE_GITEA_TOKEN or MOLECULE_GITEA_TOKEN")
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return report, fmt.Errorf("read manifest: %w", err)
	}
	var manifest manifestFile
	if err := json.Unmarshal(data, &manifest); err != nil {
		return report, fmt.Errorf("parse manifest: %w", err)
	}
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return report, fmt.Errorf("mkdir cache: %w", err)
	}
	for _, entry := range manifest.WorkspaceTemplates {
		result := refreshOne(ctx, cacheDir, token, entry)
		report.Results = append(report.Results, result)
	}
	return report, nil
}

func refreshOne(ctx context.Context, cacheDir, token string, entry ManifestEntry) TemplateResult {
	result := TemplateResult{Name: entry.Name, Repo: entry.Repo, Ref: entry.Ref}
	if result.Ref == "" {
		result.Ref = "main"
	}
	if !safeTemplateName(entry.Name) {
		result.Status = "skipped"
		result.Error = "invalid template name"
		return result
	}
	if strings.TrimSpace(entry.Repo) == "" {
		result.Status = "skipped"
		result.Error = "missing repo"
		return result
	}

	tmp, err := os.MkdirTemp(cacheDir, ".tmp-"+entry.Name+"-")
	if err != nil {
		result.Status = "failed"
		result.Error = err.Error()
		return result
	}
	defer os.RemoveAll(tmp)

	cloneURL := authenticatedURL(entry.Repo, token)
	for _, args := range [][]string{
		{"init", "-q", tmp},
		{"-C", tmp, "remote", "add", "origin", cloneURL},
		{"-C", tmp, "fetch", "--depth=1", "-q", "origin", result.Ref},
		{"-C", tmp, "checkout", "-q", "--detach", "FETCH_HEAD"},
	} {
		cmd := exec.CommandContext(ctx, "git", args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			result.Status = "failed"
			result.Error = sanitizeGitError(out, err, token)
			return result
		}
	}
	shaCmd := exec.CommandContext(ctx, "git", "-C", tmp, "rev-parse", "HEAD")
	if out, err := shaCmd.Output(); err == nil {
		result.SHA = strings.TrimSpace(string(out))
	}
	_ = os.RemoveAll(filepath.Join(tmp, ".git"))

	target := filepath.Join(cacheDir, entry.Name)
	old := filepath.Join(cacheDir, ".old-"+entry.Name+"-"+fmt.Sprint(time.Now().UnixNano()))
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, old); err != nil {
			result.Status = "failed"
			result.Error = "replace old cache: " + err.Error()
			return result
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, target); err != nil {
		if old != "" {
			_ = os.Rename(old, target)
		}
		result.Status = "failed"
		result.Error = "install cache: " + err.Error()
		return result
	}
	result.Status = "refreshed"
	return result
}

func safeTemplateName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return false
	}
	return true
}

func authenticatedURL(repo, token string) string {
	if strings.HasPrefix(repo, "http://") || strings.HasPrefix(repo, "https://") {
		u, err := url.Parse(repo)
		if err == nil {
			u.User = url.UserPassword("oauth2", token)
			return u.String()
		}
	}
	u := &url.URL{
		Scheme: "https",
		Host:   "git.moleculesai.app",
		Path:   "/" + strings.TrimSuffix(repo, ".git") + ".git",
		User:   url.UserPassword("oauth2", token),
	}
	return u.String()
}

func sanitizeGitError(out []byte, err error, token string) string {
	msg := strings.TrimSpace(string(out))
	if msg == "" {
		msg = err.Error()
	}
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "***")
	}
	if len(msg) > 300 {
		msg = msg[:300]
	}
	return msg
}
