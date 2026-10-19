package build

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// push_image.go 实现「推送镜像节点」的**推送目标声明**。
//
// 语义(与旧行为对比):
//   - 旧:push_image 只是在 DAG 里占位/编排,自身 config 一个都不读;镜像是否推送、推到哪,
//     完全由「环境绑定的镜像仓库 / 内置 registry」在 build_image 节点内决定。
//   - 新:push_image 节点的 config 成为**该镜像推送目标的权威声明**:
//     artifactJob  必填 — 要推送的产物来源节点名(必须是**同阶段**的 build_image 节点)
//     registry     可选 — 目标仓库地址(host[:port],可带协议);留空回退环境绑定/内置 registry
//     imageName    可选 — 远端镜像名(默认沿用来源产物逻辑名)
//     tag          可选 — 远端 tag(默认沿用来源产物原 tag;支持 {{runId}}/{{branch}}/{{commit}}/{{project}} 占位)
//     credentialId 可选 — 私有仓库登录凭据(registry 类型);留空回退所选仓库的凭据
//
// 执行位置:推送在**来源 build_image 节点内**完成(它持有本地镜像 tag,且产物引用必须在
// EmitArtifact 前改写为远端引用,下游部署节点才能 pull)。故 build_image 节点被 push_image
// 引用时**不再自行猜测目标**(跳过按环境 registry 的自动推送),改按本节点声明的目标推送;
// push_image 节点自身只做目标合法性校验与结果播报(不重复执行 docker push)。
//
// 为何不在 push_image 节点里 push:产物是「一次性 Emit」的(无 update/replace API),
// 若改由 push_image 后置推送,产物引用仍是本地 tag → 部署节点会 pull 一个只存在于构建机的
// 本地引用而失败;若再补发一件远端引用产物,部署节点又会因同源两件产物而报歧义。
const pushImageArtifactJobKey = "artifactJob"

// isPushImageJob 判断是否「推送镜像」节点(画布 push_image 类型)。
func isPushImageJob(jobType string) bool {
	return strings.TrimSpace(jobType) == "push_image"
}

// pushSpec 是一个 push_image 节点声明的一次推送(registry 已最终确定;镜像名/tag 空表示用默认)。
type pushSpec struct {
	RegistryURL  string
	CredentialID string
	ImageName    string
	Tag          string
}

// imagePushPlan 描述某个 build_image 节点的镜像推送安排。
//   - Referenced: 该节点是否被同阶段的某个 push_image 节点引用(是 → 不再走环境 registry 自动推送)。
//   - Specs:      引用它的 push_image 节点声明出的推送目标(registry 解析不出来的会被跳过,
//     Referenced 仍为 true,由 push_image 节点在运行期明确报错)。
type imagePushPlan struct {
	Referenced bool
	Specs      []pushSpec
}

// pushPlansByBuildNode 汇总本阶段内各 push_image 节点对「产物来源(build_image 节点名)」的推送声明。
// registry 留空时回退内置 registry(与 build_image 的自动推送同源),故需 settings。
func (b *Builder) pushPlansByBuildNode(ctx context.Context, jobs []pipeline.Job, settings *pipeline.Settings, r *run.Run) map[string]imagePushPlan {
	out := make(map[string]imagePushPlan)
	for _, jb := range jobs {
		if !isPushImageJob(jb.Type) {
			continue
		}
		src := strings.TrimSpace(cfgString(jb.Config, pushImageArtifactJobKey))
		if src == "" {
			continue
		}
		plan := out[src]
		plan.Referenced = true

		regURL := strings.TrimSpace(cfgString(jb.Config, "registry"))
		credID := strings.TrimSpace(cfgString(jb.Config, "credentialId"))
		if regURL == "" && settings != nil {
			if reg := b.resolveRegistry(ctx); reg != nil {
				regURL = reg.URL
				if credID == "" {
					credID = reg.CredentialID
				}
			}
		}
		if regURL != "" {
			tag, terr := renderRunPlaceholders(strings.TrimSpace(cfgString(jb.Config, "tag")), r)
			if terr != nil {
				log.Printf("build: push_image 节点「%s」tag 占位渲染失败: %v", jb.Name, terr)
				continue
			}
			plan.Specs = append(plan.Specs, pushSpec{
				RegistryURL:  regURL,
				CredentialID: credID,
				ImageName:    strings.TrimSpace(cfgString(jb.Config, "imageName")),
				Tag:          tag,
			})
		}
		out[src] = plan
	}
	return out
}

// defaultImageName 取远端镜像名默认值:产物逻辑名优先(build_image 产出时即项目 slug);
// 为空时从本地引用里取 "repo" 段(去 registry 与 tag)。
func defaultImageName(a run.Artifact) string {
	if n := strings.TrimSpace(a.Name); n != "" {
		return n
	}
	ref := strings.TrimSpace(a.Reference)
	if i := strings.LastIndex(ref, ":"); i >= 0 && !strings.Contains(ref[i+1:], "/") {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// defaultImageTag 取远端 tag 默认值:沿用本地引用的 tag 部分(如 pipewright/app:abc1234 → abc1234);
// 引用里没有 tag(末段含 "/")时回退 "latest"。
func defaultImageTag(localRef string) string {
	ref := strings.TrimSpace(localRef)
	i := strings.LastIndex(ref, ":")
	if i < 0 || strings.Contains(ref[i+1:], "/") {
		return "latest"
	}
	if t := strings.TrimSpace(ref[i+1:]); t != "" {
		return t
	}
	return "latest"
}

// runPushImageJob 执行一个 push_image 节点:校验推送目标可用并播报结果。
// 真正的 docker push 已在来源 build_image 节点内按本节点声明完成(见文件头注释);
// 目标不可用(缺 artifactJob / 无仓库)时明确失败,阻断下游(强失败语义,对齐 deploy_ssh)。
func (b *Builder) runPushImageJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run, settings *pipeline.Settings) error {
	src := strings.TrimSpace(cfgString(jb.Config, pushImageArtifactJobKey))
	if src == "" {
		_ = rep.Log(ctx, streamError, "推送镜像节点:未指定产物来源节点(artifactJob)")
		return ErrBuildFailed
	}
	regURL := strings.TrimSpace(cfgString(jb.Config, "registry"))
	if regURL == "" && settings != nil {
		if reg := b.resolveRegistry(ctx); reg != nil {
			regURL = reg.URL
		}
	}
	if regURL == "" {
		_ = rep.Log(ctx, streamError, fmt.Sprintf("推送镜像节点「%s」:未指定目标镜像仓库(节点 registry 为空且内置 registry 未启用)", jb.Name))
		return ErrBuildFailed
	}
	name := strings.TrimSpace(cfgString(jb.Config, "imageName"))
	tag, terr := renderRunPlaceholders(strings.TrimSpace(cfgString(jb.Config, "tag")), r)
	if terr != nil {
		_ = rep.Log(ctx, streamError, "推送镜像节点「"+jb.Name+"」:tag 占位渲染失败:"+terr.Error())
		return ErrBuildFailed
	}
	detail := stripScheme(regURL)
	if name != "" || tag != "" {
		if name == "" {
			name = "(默认镜像名)"
		}
		if tag == "" {
			tag = "(默认 tag)"
		}
		detail = fmt.Sprintf("%s/%s:%s", detail, name, tag)
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· 推送镜像「%s」:已按本节点配置随来源节点「%s」完成推送 → %s", jb.Name, src, detail))
	return nil
}
