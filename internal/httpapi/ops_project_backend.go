package httpapi

// opschat 项目维度工具的后端适配器:把 opschat.ProjectBackend 端口落到
// project / pipeline / run 领域服务与 SourceReader 上(httpapi 已 import opschat,
// 无反向依赖)。所有输出都是有界 JSON 文本;opschat 侧仍会按会话读预算二次
// 裁剪并掩码,这里只保证不无界放大(大文本字段先裁剪再序列化)。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/huangchengsir/pipewright/internal/opschat"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/pipelineyaml"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/run"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 项目工具输出的单字段裁剪上限(字节):YAML/文件内容 32KB,失败日志 4KB。
const (
	opsProjectTextBytes = 32 << 10
	opsProjectFailBytes = 4 << 10
)

// OpsProjectBackend 实现 opschat.ProjectBackend;各字段均为只读服务引用。
type OpsProjectBackend struct {
	projects  project.Service
	pipelines pipeline.Service
	runs      run.Service
	vault     vault.Vault
	reader    SourceReader
}

// NewOpsProjectBackend 构造项目维度后端(依赖在 main 装配层均已就绪)。
func NewOpsProjectBackend(projects project.Service, pipelines pipeline.Service, runs run.Service, v vault.Vault, reader SourceReader) *OpsProjectBackend {
	return &OpsProjectBackend{projects: projects, pipelines: pipelines, runs: runs, vault: v, reader: reader}
}

var _ opschat.ProjectBackend = (*OpsProjectBackend)(nil)

func (b *OpsProjectBackend) Snapshot(ctx context.Context, projectID string) (opschat.ProjectSnapshot, error) {
	p, err := b.project(ctx, projectID)
	if err != nil {
		return opschat.ProjectSnapshot{}, err
	}
	return opschat.ProjectSnapshot{ID: p.ID, Name: p.Name, RepoURL: p.RepoURL, DefaultBranch: p.DefaultBranch, PacEnabled: p.PacEnabled, UpdatedAt: p.UpdatedAt}, nil
}

func (b *OpsProjectBackend) project(ctx context.Context, projectID string) (*project.Project, error) {
	if b.projects == nil {
		return nil, errors.New("project service unavailable")
	}
	p, err := b.projects.Get(ctx, projectID)
	if err != nil {
		if errors.Is(err, project.ErrNotFound) {
			return nil, errors.New("project not found")
		}
		return nil, errors.New("project lookup failed")
	}
	return p, nil
}

// resolveSource 解析仓库读取所需的 repoURL + 凭据(尽力解,失败用空凭据试公开仓库,
// 与 pac 预览/pacloader 同语义)。
func (b *OpsProjectBackend) resolveSource(p *project.Project) (string, vault.GitAuth) {
	cred := vault.GitAuth{}
	if b.vault != nil && strings.TrimSpace(p.CredentialID) != "" {
		if auth, err := b.vault.GetGitAuth(p.CredentialID); err == nil {
			cred = auth
		}
	}
	return strings.TrimSpace(p.RepoURL), cred
}

func opsProjectMarshal(v any) (string, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return "", errors.New("projection failed")
	}
	return string(raw), nil
}

func (b *OpsProjectBackend) Info(ctx context.Context, projectID string) (string, error) {
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	out := map[string]any{
		"name":          p.Name,
		"repoUrl":       p.RepoURL,
		"defaultBranch": p.DefaultBranch,
		"sourceType":    p.SourceType,
		"pacEnabled":    p.PacEnabled,
		"createdAt":     p.CreatedAt.UTC(),
		"updatedAt":     p.UpdatedAt.UTC(),
	}
	if p.CredentialName != "" {
		out["credentialName"] = p.CredentialName
	}
	if b.runs != nil {
		if latest, e := b.runs.LatestByProject(ctx, []string{p.ID}); e == nil && latest[p.ID].Status != "" {
			out["lastRunStatus"] = latest[p.ID].Status
			out["lastRunAt"] = latest[p.ID].CreatedAt.UTC()
		}
	}
	return opsProjectMarshal(out)
}

func (b *OpsProjectBackend) CodeTree(ctx context.Context, projectID, ref, path string) (string, error) {
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	if b.reader == nil {
		return "", errors.New("source reader unavailable")
	}
	repoURL, cred := b.resolveSource(p)
	if repoURL == "" {
		return "", errors.New("project has no repository configured")
	}
	if ref == "" {
		ref = strings.TrimSpace(p.DefaultBranch)
	}
	tree, err := b.reader.Tree(ctx, repoURL, cred, ref, path)
	if err != nil {
		return "", errors.New("repository read failed (clone degraded or ref unavailable)")
	}
	return opsProjectMarshal(tree)
}

func (b *OpsProjectBackend) CodeRead(ctx context.Context, projectID, ref, path string, lines int) (string, error) {
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	if b.reader == nil {
		return "", errors.New("source reader unavailable")
	}
	repoURL, cred := b.resolveSource(p)
	if repoURL == "" {
		return "", errors.New("project has no repository configured")
	}
	if ref == "" {
		ref = strings.TrimSpace(p.DefaultBranch)
	}
	blob, err := b.reader.Blob(ctx, repoURL, cred, ref, path)
	if err != nil {
		return "", errors.New("file not found or clone degraded")
	}
	if blob.Binary {
		return opsProjectMarshal(map[string]any{"path": path, "ref": blob.Ref, "size": blob.Size, "binary": true})
	}
	content, truncated := headLines(blob.Content, lines, opsProjectTextBytes)
	return opsProjectMarshal(map[string]any{
		"path": path, "ref": blob.Ref, "size": blob.Size, "binary": false,
		"truncated": truncated || blob.Truncated, "content": content,
	})
}

func (b *OpsProjectBackend) PipelineConfig(ctx context.Context, projectID string) (string, error) {
	if b.pipelines == nil {
		return "", errors.New("pipeline service unavailable")
	}
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	cfg, err := b.pipelines.Get(ctx, p.ID)
	if err != nil {
		return "", errors.New("pipeline config unavailable")
	}
	stages := []map[string]any{}
	for _, st := range cfg.Spec.Stages {
		jobs := []map[string]any{}
		for _, j := range st.Jobs {
			jobs = append(jobs, map[string]any{"id": j.ID, "name": j.Name, "type": j.Type, "summary": j.Summary})
		}
		stages = append(stages, map[string]any{
			"name": st.Name, "kind": st.Kind, "needs": st.Needs,
			"allowFailure": st.AllowFailure, "jobCount": len(st.Jobs), "jobs": jobs,
		})
	}
	return opsProjectMarshal(map[string]any{
		"yaml":      clipText(cfg.YAML, opsProjectTextBytes),
		"status":    cfg.Status,
		"updatedAt": cfg.UpdatedAt.UTC(),
		"stages":    stages,
	})
}

func (b *OpsProjectBackend) RepoConfig(ctx context.Context, projectID, branch string) (string, error) {
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	if b.reader == nil {
		return "", errors.New("source reader unavailable")
	}
	if branch == "" {
		branch = strings.TrimSpace(p.DefaultBranch)
	}
	// 生效文件:项目可指定 pac_file(可指定文件,不写死 .pipewright.yml);空回默认。
	file := project.EffectivePacFile(p.PacFile)
	out := map[string]any{"pacEnabled": p.PacEnabled, "ref": branch, "file": file, "found": false}
	repoURL, cred := b.resolveSource(p)
	if repoURL == "" {
		return opsProjectMarshal(out)
	}
	blob, err := b.reader.Blob(ctx, repoURL, cred, branch, file)
	if err != nil || blob.Binary {
		// 拉不到/二进制:按「文件不存在」呈现(与 pac 预览一致,不报错)。
		return opsProjectMarshal(out)
	}
	out["found"] = true
	if strings.TrimSpace(blob.Content) == "" {
		out["valid"] = false
		out["error"] = "file is empty"
		return opsProjectMarshal(out)
	}
	doc, perr := pipelineyaml.ParseDocument([]byte(blob.Content))
	if perr != nil {
		out["valid"] = false
		out["error"] = clipText(perr.Error(), 512)
		return opsProjectMarshal(out)
	}
	out["valid"] = true
	out["stageCount"] = len(doc.Config.Spec.Stages)
	stages := []map[string]any{}
	for _, st := range doc.Config.Spec.Stages {
		stages = append(stages, map[string]any{"name": st.Name, "kind": st.Kind, "jobCount": len(st.Jobs)})
	}
	out["stages"] = stages
	if len(doc.Variables) > 0 {
		vars := []map[string]any{}
		for _, vd := range doc.Variables {
			vars = append(vars, map[string]any{
				"name": vd.Name, "description": vd.Description,
				"required": vd.Required, "secret": vd.Secret, "default": vd.Default,
			})
		}
		out["variables"] = vars
	}
	out["yaml"] = clipText(blob.Content, opsProjectTextBytes)
	return opsProjectMarshal(out)
}

func (b *OpsProjectBackend) Runs(ctx context.Context, projectID, status string, limit int) (string, error) {
	if b.runs == nil {
		return "", errors.New("run service unavailable")
	}
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	res, err := b.runs.List(ctx, run.ListFilter{ProjectID: p.ID, Status: status, Page: 1, PageSize: limit})
	if err != nil {
		return "", errors.New("run listing failed")
	}
	items := []map[string]any{}
	for _, r := range res.Items {
		items = append(items, map[string]any{
			"runId": r.ID, "status": r.Status,
			"trigger":   map[string]any{"type": r.Trigger.Type, "branch": r.Trigger.Branch, "commit": r.Trigger.Commit, "actor": r.Trigger.Actor},
			"createdAt": r.CreatedAt.UTC(), "startedAt": timePtrUTC(r.StartedAt), "finishedAt": timePtrUTC(r.FinishedAt),
			"failureLog": clipText(r.FailureLog, 256),
		})
	}
	return opsProjectMarshal(map[string]any{"total": res.Total, "runs": items})
}

func (b *OpsProjectBackend) RunDetail(ctx context.Context, runID string) (string, error) {
	if b.runs == nil {
		return "", errors.New("run service unavailable")
	}
	r, err := b.runs.Get(ctx, runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return "", errors.New("run not found")
		}
		return "", errors.New("run lookup failed")
	}
	steps := []map[string]any{}
	for _, st := range r.Steps {
		steps = append(steps, map[string]any{
			"ordinal": st.Ordinal, "name": st.Name, "stage": st.Stage, "status": st.Status,
			"startedAt": timePtrUTC(st.StartedAt), "finishedAt": timePtrUTC(st.FinishedAt),
		})
	}
	return opsProjectMarshal(map[string]any{
		"runId": r.ID, "projectId": r.ProjectID, "projectName": r.ProjectName,
		"status":     r.Status,
		"trigger":    map[string]any{"type": r.Trigger.Type, "branch": r.Trigger.Branch, "commit": r.Trigger.Commit, "actor": r.Trigger.Actor},
		"specSource": map[string]any{"source": r.SpecSource.Source, "ref": r.SpecSource.Ref, "file": r.SpecSource.File, "fallback": r.SpecSource.Fallback},
		"createdAt":  r.CreatedAt.UTC(), "startedAt": timePtrUTC(r.StartedAt), "finishedAt": timePtrUTC(r.FinishedAt),
		"steps":         steps,
		"failureLog":    clipText(r.FailureLog, opsProjectFailBytes),
		"resumeOfRunId": r.ResumeOfRunID,
	})
}

func (b *OpsProjectBackend) RunLogs(ctx context.Context, runID string, stepOrdinal, lines int) (string, error) {
	if b.runs == nil {
		return "", errors.New("run service unavailable")
	}
	logs, err := b.runs.GetLogs(ctx, runID, 0)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			return "", errors.New("run not found")
		}
		return "", errors.New("log listing failed")
	}
	return tailRender(logs, lines, stepOrdinal), nil
}

func (b *OpsProjectBackend) TriggerRun(ctx context.Context, projectID, branch string, params map[string]string) (string, error) {
	if b.runs == nil {
		return "", errors.New("run service unavailable")
	}
	p, err := b.project(ctx, projectID)
	if err != nil {
		return "", err
	}
	triggerParams := map[string]string{}
	for k, v := range params {
		if k = strings.TrimSpace(k); k != "" {
			triggerParams[k] = v
		}
	}
	r, err := b.runs.Create(ctx, p.ID, run.Trigger{Type: run.TriggerManual, Branch: strings.TrimSpace(branch), Actor: "ai-assistant", Params: triggerParams})
	if err != nil {
		return "", fmt.Errorf("trigger rejected: %v", clipText(err.Error(), 256))
	}
	return opsProjectMarshal(map[string]any{"runId": r.ID, "status": r.Status, "branch": r.Trigger.Branch})
}

func (b *OpsProjectBackend) CancelRun(ctx context.Context, runID string) (string, error) {
	if b.runs == nil {
		return "", errors.New("run service unavailable")
	}
	r, err := b.runs.Cancel(ctx, runID)
	if err != nil {
		return "", fmt.Errorf("cancel rejected: %v", clipText(err.Error(), 256))
	}
	return opsProjectMarshal(map[string]any{"runId": r.ID, "status": r.Status})
}

// headLines 取文本前 lines 行并按字节上限裁剪;返回裁剪标志。
func headLines(text string, lines, limit int) (string, bool) {
	if lines < 1 {
		lines = 1
	}
	truncated := false
	selected := []string{}
	for i, line := range strings.Split(text, "\n") {
		if i >= lines {
			truncated = true
			break
		}
		selected = append(selected, line)
	}
	out := strings.Join(selected, "\n")
	if len(out) > limit {
		out = out[:limit]
		truncated = true
	}
	return out, truncated || len(text) > len(out)+1
}

// tailRender 把日志行渲染为紧凑文本(seq/ts/stream/text),取尾部 lines 行并按
// 32KB 裁剪;stepOrdinal>=0 时过滤到该节点,否则整个运行。
func tailRender(lines []run.LogLine, limit, stepOrdinal int) string {
	if stepOrdinal >= 0 {
		filtered := lines[:0]
		for _, l := range lines {
			if l.StepOrdinal == stepOrdinal {
				filtered = append(filtered, l)
			}
		}
		lines = filtered
	}
	if limit < 1 {
		limit = 1
	}
	start := 0
	if len(lines) > limit {
		start = len(lines) - limit
	}
	out := make([]string, 0, limit)
	for _, l := range lines[start:] {
		ts := l.Ts.UTC().Format(time.RFC3339)
		out = append(out, fmt.Sprintf("%d %s %s %s", l.Seq, ts, l.Stream, l.Text))
	}
	text := strings.Join(out, "\n")
	if len(text) > opsProjectTextBytes {
		text = text[len(text)-opsProjectTextBytes:]
	}
	return text
}

func clipText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}

func timePtrUTC(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	utc := t.UTC()
	return &utc
}
