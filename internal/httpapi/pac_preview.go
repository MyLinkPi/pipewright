package httpapi

import (
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/huangchengsir/pipewright/internal/audit"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/pipelineyaml"
	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// ---- PAC 预览/校验契约 DTO(冻结;camelCase) ----
//
// {found, valid, ref, file, error, stageCount, stages:[{name,kind,jobCount}],
//  variables:[{name,description,required,secret,default}]}
// 绝不回显仓库 URL 凭据 / 任何 secret;解析失败 found=true valid=false error=<message> stages=[]。
// file 回实际生效文件(查询参数临时指定 || 项目 pac_file || 默认 .pipewright.yml)。

type pacStageSummaryDTO struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	JobCount int    `json:"jobCount"`
}

type pacPreviewDTO struct {
	Found      bool                 `json:"found"`
	Valid      bool                 `json:"valid"`
	Ref        string               `json:"ref"`
	File       string               `json:"file"`
	Error      string               `json:"error"`
	StageCount int                  `json:"stageCount"`
	Stages     []pacStageSummaryDTO `json:"stages"`
	Variables  []pacVarDefDTO       `json:"variables"`
}

// makePacPreviewHandler 返回 GET /api/projects/{id}/pac/preview?ref=<ref>&file=<file> handler(认证)。
//
// 在「依赖」仓库流水线文件驱动运行之前,让用户主动按 ref 拉取并校验它——看清运行时会用到的
// 配置摘要,并把 YAML 错误显式呈现(运行时是静默回退,无反馈)。镜像 pacloader 的拉取语义:
//   - ref 省略 → 项目默认分支;
//   - file 省略 → 项目 pac_file 配置(可指定文件,不写死 .pipewright.yml),再缺省默认文件;
//     临时 file 同样走 NormalizePacFile 校验(相对路径,拒绝绝对路径/.. 段);
//   - token 经保险库尽力解(取不到则空 token 试公开仓库,与 pacloader 一致);
//   - 文件不存在 → 200 found=false;克隆降级 → 200 found=false(无法判定);
//   - 解析/校验失败 → 200 found=true valid=false error=<message> stages=[];
//   - 成功 → found=true valid=true + 阶段摘要 + variables 声明,并把声明同步进 pipeline_settings
//     (「变量与缓存」据此展示填写;值按 name 保留)。同步失败只记日志,不影响预览响应。
//
// 绝不把仓库 URL 凭据 / 任何 secret 写入响应或 error。项目不存在 → 404;所需服务未初始化 → 503。
func makePacPreviewHandler(d sourceDeps, aud audit.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if d.projects == nil || d.reader == nil {
			writeError(w, http.StatusServiceUnavailable, "internal", "源码读取所需服务未初始化")
			return
		}

		id := chi.URLParam(r, "id")
		proj, err := d.projects.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, project.ErrNotFound) {
				writeError(w, http.StatusNotFound, "project_not_found", "项目不存在")
				return
			}
			writeError(w, http.StatusInternalServerError, "internal", "服务器内部错误")
			return
		}

		ref := strings.TrimSpace(r.URL.Query().Get("ref"))
		if ref == "" {
			ref = strings.TrimSpace(proj.DefaultBranch)
		}

		// 生效文件:查询参数临时指定 → 项目 pac_file → 默认 .pipewright.yml。
		file := strings.TrimSpace(r.URL.Query().Get("file"))
		if file != "" {
			normalized, ferr := project.NormalizePacFile(file)
			if ferr != nil {
				writeError(w, http.StatusBadRequest, "invalid_pac_file", ferr.Error())
				return
			}
			file = normalized
		} else {
			file = project.EffectivePacFile(proj.PacFile)
		}

		out := pacPreviewDTO{Ref: ref, File: file, Stages: []pacStageSummaryDTO{}, Variables: []pacVarDefDTO{}}

		if strings.TrimSpace(proj.RepoURL) == "" {
			// 未配置仓库:无从拉取,按「文件不存在」呈现(found=false),不报错。
			writeJSON(w, http.StatusOK, out)
			return
		}

		// 取仓库凭据(进程内取用即弃;取不到不致命 → 空凭据试公开仓库,与 pacloader 一致)。
		cred := vault.GitAuth{}
		if d.vault != nil && strings.TrimSpace(proj.CredentialID) != "" {
			if auth, terr := d.vault.GetGitAuth(proj.CredentialID); terr == nil {
				cred = auth
			}
		}

		blob, berr := d.reader.Blob(r.Context(), proj.RepoURL, cred, ref, file)
		if berr != nil {
			// 文件不存在 / 克隆失败 → found=false(运行时此路径会回退库内配置)。
			// 绝不外泄底层错误(可能含 URL 凭据);valid=false、error 留空(非 YAML 问题)。
			writeJSON(w, http.StatusOK, out)
			return
		}
		if blob.Degraded {
			// 克隆降级:无法判定文件是否存在 → found=false(不误报)。
			writeJSON(w, http.StatusOK, out)
			return
		}
		if blob.Binary || strings.TrimSpace(blob.Content) == "" {
			// 二进制 / 空文件:视作未提供有效配置(found=false)。
			writeJSON(w, http.StatusOK, out)
			return
		}

		// 文件存在。
		out.Found = true

		doc, perr := pipelineyaml.ParseDocument([]byte(blob.Content))
		if perr != nil {
			// YAML 非法 → found=true valid=false + 解析错误消息(pipelineyaml/pipeline 的错误不含密钥)。
			out.Valid = false
			out.Error = perr.Error()
			writeJSON(w, http.StatusOK, out)
			return
		}

		out.Valid = true
		stages := make([]pacStageSummaryDTO, 0, len(doc.Config.Spec.Stages))
		for _, st := range doc.Config.Spec.Stages {
			stages = append(stages, pacStageSummaryDTO{
				Name:     st.Name,
				Kind:     st.Kind,
				JobCount: len(st.Jobs),
			})
		}
		out.Stages = stages
		out.StageCount = len(stages)

		vars := make([]pacVarDefDTO, 0, len(doc.Variables))
		defs := make([]pipeline.PacVarDef, 0, len(doc.Variables))
		for _, vd := range doc.Variables {
			vars = append(vars, pacVarDefDTO{
				Name:        vd.Name,
				Description: vd.Description,
				Required:    vd.Required,
				Secret:      vd.Secret,
				Default:     vd.Default,
			})
			defs = append(defs, pipeline.PacVarDef{
				Name: vd.Name, Description: vd.Description,
				Required: vd.Required, Secret: vd.Secret, Default: vd.Default,
			})
		}
		out.Variables = vars

		// 校验通过:把变量声明同步进 pipeline_settings(「拉取并校验后读进流水线配置」)。
		// 已填值按 name 保留;零声明也同步(仓库删掉 variables 块后清掉残留声明)。
		// 同步失败不影响预览响应(降级为仅本次展示,记日志可排查)。
		if d.settings != nil {
			if _, serr := d.settings.SyncPacVars(r.Context(), proj.ID, defs, ref, file); serr != nil {
				log.Printf("[pac] 项目 %s 变量声明同步失败(预览仍返回): %v", proj.ID, serr)
			} else if len(defs) > 0 {
				// 轻量留痕:仅变量名列表,绝无值(同变量组 audit 口径)。
				names := make([]string, 0, len(defs))
				for _, vd := range defs {
					names = append(names, vd.Name)
				}
				recordAudit(r.Context(), aud, audit.Entry{
					Actor:      auditActor,
					Action:     audit.ActionPacVarsSync,
					TargetType: audit.TargetProject,
					TargetID:   proj.ID,
					Detail:     map[string]any{"ref": ref, "file": file, "vars": names},
					IP:         clientIP(r),
				})
			}
		}

		writeJSON(w, http.StatusOK, out)
	}
}
