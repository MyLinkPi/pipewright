package opschat

// 项目维度工具目录:全部作用于会话绑定的项目(隐含目标,不带 target 参数)。
// 只读工具直接执行;run_trigger/run_cancel 是变更,走确认卡(consent 流复用)。

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var projectRunStatuses = map[string]bool{
	"queued": true, "running": true, "waiting_approval": true, "success": true,
	"failed": true, "rejected": true, "partial_failed": true, "rolled_back": true,
}

func projectTools() []Tool {
	return []Tool{
		{ID: "project_info", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Description: "Basic facts of the bound project: name, repository URL, default branch, pipeline-as-code (repo .pipewright.yml) status and update time.",
			Notes:       nil, Targeted: false},
		{ID: "code_tree", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","maxLength":512},"ref":{"type":"string","maxLength":256}},"additionalProperties":false}`),
			Description: "List the direct children (name, type, size) of one directory in the project repository. Empty path is the repository root; empty ref is the default branch. Explore the layout with this before reading files.",
			Notes:       map[string]string{"path": "Relative directory path inside the repository (empty = root), no leading slash, no '..'.", "ref": "Branch or tag to read (empty = project default branch)."}, Targeted: false},
		{ID: "code_read", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","maxLength":512},"ref":{"type":"string","maxLength":256},"lines":{"type":"integer","minimum":1,"maximum":1000,"default":400}},"required":["path"],"additionalProperties":false}`),
			Description: "Read a text file from the project repository, starting at the top of the file (lines caps the returned head, default 400). Binary files are detected and reported as binary without content.",
			Notes:       map[string]string{"path": "Relative file path inside the repository, no leading slash, no '..'.", "ref": "Branch or tag to read (empty = project default branch).", "lines": "Number of leading lines to return, 1-1000, default 400."}, Targeted: false},
		{ID: "pipeline_config", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Description: "The pipeline configuration stored in the platform for the project: rendered YAML, a stage/job summary and its update time.",
			Notes:       nil, Targeted: false},
		{ID: "repo_pipeline_config", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string","maxLength":256}},"additionalProperties":false}`),
			Description: "The pipeline-as-code file (.pipewright.yml) read from the repository branch (empty = default branch), with the effective source and the fallback reason when it does not apply to this project.",
			Notes:       map[string]string{"branch": "Branch to read the file from (empty = project default branch)."}, Targeted: false},
		{ID: "project_runs", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"status":{"type":"string","enum":["queued","running","waiting_approval","success","failed","rejected","partial_failed","rolled_back"]},"limit":{"type":"integer","minimum":1,"maximum":100,"default":20}},"additionalProperties":false}`),
			Description: "Recent pipeline runs of the project: id, status, trigger, branch, commit and timings. Use run_detail / run_logs on a run id from here.",
			Notes:       map[string]string{"status": "Only return runs in this status (empty = all).", "limit": "Number of runs to return, 1-100, default 20."}, Targeted: false},
		{ID: "run_detail", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"runId":{"type":"string","maxLength":36}},"required":["runId"],"additionalProperties":false}`),
			Description: "One pipeline run in detail: status, trigger, commit, every node/step status with timings, spec source and the failure log. stepOrdinal values from here feed run_logs.",
			Notes:       map[string]string{"runId": "Run id as reported by project_runs or the conversation."}, Targeted: false},
		{ID: "run_logs", Mutation: false, Schema: json.RawMessage(`{"type":"object","properties":{"runId":{"type":"string","maxLength":36},"stepOrdinal":{"type":"integer","minimum":-1},"lines":{"type":"integer","minimum":1,"maximum":1000,"default":200}},"required":["runId"],"additionalProperties":false}`),
			Description: "Tail logs of a pipeline run, optionally filtered to one node by its stepOrdinal from run_detail (-1 or omitted = whole run). lines is 1-1000, default 200.",
			Notes:       map[string]string{"runId": "Run id as reported by project_runs or run_detail.", "stepOrdinal": "Node ordinal from run_detail (-1 or omitted = whole-run logs).", "lines": "Number of trailing log lines to return, 1-1000, default 200."}, Targeted: false},
		{ID: "run_trigger", Mutation: true, Schema: json.RawMessage(`{"type":"object","properties":{"branch":{"type":"string","maxLength":256},"params":{"type":"object","additionalProperties":{"type":"string","maxLength":256},"maxProperties":16}},"additionalProperties":false}`),
			Description: "Trigger a new manual run of the project pipeline. Requires confirmation; must carry a concrete purpose and reason.",
			Notes:       map[string]string{"branch": "Branch to build (empty = project default branch).", "params": "Optional build parameters, plain string values only."}, Targeted: false},
		{ID: "run_cancel", Mutation: true, Schema: json.RawMessage(`{"type":"object","properties":{"runId":{"type":"string","maxLength":36}},"required":["runId"],"additionalProperties":false}`),
			Description: "Cancel a queued or running pipeline run. Requires confirmation; must carry a concrete purpose and reason.",
			Notes:       map[string]string{"runId": "Run id to cancel, as reported by project_runs or run_detail."}, Targeted: false},
	}
}

func projectMutation(id string) bool {
	return id == "run_trigger" || id == "run_cancel"
}

func isProjectTool(id string) bool {
	switch id {
	case "project_info", "code_tree", "code_read", "pipeline_config", "repo_pipeline_config", "project_runs", "run_detail", "run_logs", "run_trigger", "run_cancel":
		return true
	}
	return false
}

// validRepoPath 限定仓库内相对路径:UTF-8、≤512B、无控制字符,拒绝绝对路径、
// 反斜杠、盘符与 ".." 段(允许空 = 仓库根,仅 code_tree)。
func validRepoPath(p string) bool {
	if !utf8.ValidString(p) || len(p) > 512 {
		return false
	}
	if p == "" {
		return true
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") || strings.Contains(p, "\\") || len(p) > 1 && p[1] == ':' {
		return false
	}
	if strings.HasPrefix(p, "-") {
		return false
	}
	for _, r := range p {
		if r <= 31 || r == 127 {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

var gitRefChars = regexp.MustCompile(`^[A-Za-z0-9._/+~-]{1,256}$`)

// validGitRef 限定分支/tag/引用名:受限字符集,拒绝 ".."、前导 "-"(选项伪装)。
func validGitRef(ref string) bool {
	if !gitRefChars.MatchString(ref) || strings.Contains(ref, "..") || strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/") {
		return false
	}
	return !strings.Contains(ref, "//")
}

// validRunParams 限定构建参数:≤16 项,键 ≤64B 受限字符集,值 ≤256B 无控制字符。
func validRunParams(params map[string]string) bool {
	if len(params) > 16 {
		return false
	}
	for k, v := range params {
		if len(k) == 0 || len(k) > 64 || !gitRefChars.MatchString(k) {
			return false
		}
		if !utf8.ValidString(v) || len(v) > 256 {
			return false
		}
		for _, r := range v {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

// parseProjectArgs 严格校验项目工具参数(结构对齐机器 parseArgs:严格解码 +
// 逐工具白名单键 + 语义校验;显式 0 与缺省同机器 lines 处理)。
func parseProjectArgs(id string, raw json.RawMessage) (ToolArgs, error) {
	var a ToolArgs
	if !utf8.Valid(raw) {
		return a, ErrInvalid
	}
	var supplied map[string]json.RawMessage
	if err := strict(raw, &a); err != nil {
		return a, err
	}
	if len(raw) > 0 {
		if json.Unmarshal(raw, &supplied) != nil {
			return a, ErrInvalid
		}
	}
	allowed := map[string]bool{}
	switch id {
	case "project_info", "pipeline_config":
	case "code_tree":
		allowed["path"] = true
		allowed["ref"] = true
		if !validRepoPath(a.Path) || (a.Ref != "" && !validGitRef(a.Ref)) {
			return a, ErrInvalid
		}
	case "code_read":
		allowed["path"] = true
		allowed["ref"] = true
		allowed["lines"] = true
		if a.Path == "" || !validRepoPath(a.Path) || (a.Ref != "" && !validGitRef(a.Ref)) {
			return a, ErrInvalid
		}
		if a.Lines == 0 {
			if _, ok := supplied["lines"]; ok {
				return a, ErrInvalid
			}
			a.Lines = 400
		}
		if a.Lines < 1 || a.Lines > 1000 {
			return a, ErrInvalid
		}
	case "repo_pipeline_config":
		allowed["branch"] = true
		if a.Branch != "" && !validGitRef(a.Branch) {
			return a, ErrInvalid
		}
	case "project_runs":
		allowed["status"] = true
		allowed["limit"] = true
		if a.Status != "" && !projectRunStatuses[a.Status] {
			return a, ErrInvalid
		}
		if a.Limit == 0 {
			if _, ok := supplied["limit"]; ok {
				return a, ErrInvalid
			}
			a.Limit = 20
		}
		if a.Limit < 1 || a.Limit > 100 {
			return a, ErrInvalid
		}
	case "run_detail":
		allowed["runId"] = true
		if !validID(a.RunID) {
			return a, ErrInvalid
		}
	case "run_logs":
		allowed["runId"] = true
		allowed["stepOrdinal"] = true
		allowed["lines"] = true
		if !validID(a.RunID) {
			return a, ErrInvalid
		}
		if a.StepOrdinal != nil && *a.StepOrdinal < -1 {
			return a, ErrInvalid
		}
		if a.Lines == 0 {
			if _, ok := supplied["lines"]; ok {
				return a, ErrInvalid
			}
			a.Lines = 200
		}
		if a.Lines < 1 || a.Lines > 1000 {
			return a, ErrInvalid
		}
	case "run_trigger":
		allowed["branch"] = true
		allowed["params"] = true
		if a.Branch != "" && !validGitRef(a.Branch) {
			return a, ErrInvalid
		}
		if !validRunParams(a.Params) {
			return a, ErrInvalid
		}
	case "run_cancel":
		allowed["runId"] = true
		if !validID(a.RunID) {
			return a, ErrInvalid
		}
	default:
		return a, ErrInvalid
	}
	for k := range supplied {
		if !allowed[k] {
			return a, ErrInvalid
		}
	}
	return a, nil
}
