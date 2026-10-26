// pacvars.go 是「流水线即代码」变量的执行侧解析与 ${NAME} 占位替换(流水线即代码 · 变量)。
//
// 仓库 YAML 顶层 variables 声明的变量(数据库连接串、服务注册基域等无法在代码里决定的字段),
// 值在平台「变量与缓存」填写(secret 只存保险库 credentialId 引用)。YAML 里的 ${NAME} 占位符
// 原样保留在 spec 里;本文件在**阶段执行瞬间**把占位符替换为真实值——落库 spec / 回显恒为
// 占位符,secret 明文只在执行进程内短暂存在,与构建变量(buildArgs)同一套铁律。
//
// 替换面(字符串表面):job.Config 全部字符串值(覆盖 deploy 节点配置、script 的 env JSON 串
// 与 commands 串)、stage.Services[].Env 条目、stage.Post[].Commands。未解析出的占位符原样保留
// (容器内 shell 可自行展开环境变量)。
package build

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// pacVarRe 匹配 ${NAME} 占位符(NAME 与 pipelineyaml 变量声明同一套标识符规则)。
var pacVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// revealPacSecret 经保险库取 secret 明文(即取即用;失败返回空)。与 buildArgs 同口径。
func (b *Builder) revealPacSecret(credID string) string {
	if credID == "" || b.vault == nil {
		return ""
	}
	pt, err := b.vault.Reveal(credID)
	if err != nil {
		return ""
	}
	return pt
}

// ResolvePacVarValues 把声明 + 填写值解析为「变量名 → 执行值」:
//   - 明文值直取;secret 值经 reveal 解密(reveal 失败按未解析处理,不中断);
//   - 未填(无值/无凭据/reveal 失败)且声明带 default 的取 default(secret 声明无 default,
//     pipelineyaml 已在解析时拒绝);
//   - 返回 values(已解析)与 unresolved(声明了但拿不到值的变量名,排序稳定)。
//
// 未声明的名字不出现在任一返回值里。
func ResolvePacVarValues(pv pipeline.PacVarsConfig, reveal func(credID string) string) (map[string]string, []string) {
	filled := make(map[string]string, len(pv.Values))
	for _, v := range pv.Values {
		switch {
		case v.Secret:
			if pt := reveal(v.CredentialID); pt != "" {
				filled[v.Key] = pt
			}
		case strings.TrimSpace(v.Value) != "":
			filled[v.Key] = v.Value
		}
	}

	values := make(map[string]string, len(pv.Defs))
	var unresolved []string
	for _, d := range pv.Defs {
		if v, ok := filled[d.Name]; ok {
			values[d.Name] = v
			continue
		}
		if !d.Secret && strings.TrimSpace(d.Default) != "" {
			values[d.Name] = strings.TrimSpace(d.Default)
			continue
		}
		unresolved = append(unresolved, d.Name)
	}
	sort.Strings(unresolved)
	return values, unresolved
}

// MissingRequiredPacVars 返回「required: true 且未解析出值」的变量名(排序稳定)。
// 用于 PAC 来源运行的执行拦截:缺 required 变量时诚实失败并列出变量名(绝不输出值)。
func MissingRequiredPacVars(defs []pipeline.PacVarDef, unresolved []string) []string {
	if len(defs) == 0 || len(unresolved) == 0 {
		return nil
	}
	missing := make(map[string]struct{}, len(unresolved))
	for _, n := range unresolved {
		missing[n] = struct{}{}
	}
	var out []string
	for _, d := range defs {
		if d.Required {
			if _, ok := missing[d.Name]; ok {
				out = append(out, d.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// ApplyPacVarsToStage 就地把 ${NAME} 替换为 values[NAME](字符串表面全替换;未知占位原样保留)。
// 幂等:已替换的值里若再次出现占位(如值本身含 ${X})不递归展开,防止注入循环。
func ApplyPacVarsToStage(stage *pipeline.Stage, values map[string]string) {
	if stage == nil || len(values) == 0 {
		return
	}
	// 快路径:值里没出现任何占位符就跳过(绝大多数阶段)。
	if !stageHasPacPlaceholder(stage) {
		return
	}
	for ji := range stage.Jobs {
		applyPacVarsToConfig(stage.Jobs[ji].Config, values)
	}
	for si := range stage.Services {
		for ei := range stage.Services[si].Env {
			stage.Services[si].Env[ei] = renderPacVars(stage.Services[si].Env[ei], values)
		}
	}
	for pi := range stage.Post {
		for ci := range stage.Post[pi].Commands {
			stage.Post[pi].Commands[ci] = renderPacVars(stage.Post[pi].Commands[ci], values)
		}
	}
}

// applyPacVarsToConfig 替换 config map 的全部字符串值(script 的 env/commands 是 JSON 编码串,
// 同样按整串替换覆盖)。
func applyPacVarsToConfig(cfg map[string]any, values map[string]string) {
	for k, v := range cfg {
		if s, ok := v.(string); ok {
			cfg[k] = renderPacVars(s, values)
		}
	}
}

// renderPacVars 把 s 里的 ${NAME} 替换为 values[NAME];未在 values 里的占位原样保留。
func renderPacVars(s string, values map[string]string) string {
	if !strings.Contains(s, "${") {
		return s
	}
	return pacVarRe.ReplaceAllStringFunc(s, func(m string) string {
		name := pacVarRe.FindStringSubmatch(m)[1]
		if v, ok := values[name]; ok {
			return v
		}
		return m
	})
}

// stageHasPacPlaceholder 判断阶段的字符串表面是否出现 ${ 占位(快速预检,避免无谓遍历)。
func stageHasPacPlaceholder(stage *pipeline.Stage) bool {
	found := false
	for ji := range stage.Jobs {
		for _, v := range stage.Jobs[ji].Config {
			if s, ok := v.(string); ok && strings.Contains(s, "${") {
				found = true
			}
		}
	}
	for si := range stage.Services {
		for _, e := range stage.Services[si].Env {
			if strings.Contains(e, "${") {
				found = true
			}
		}
	}
	for pi := range stage.Post {
		for _, c := range stage.Post[pi].Commands {
			if strings.Contains(c, "${") {
				found = true
			}
		}
	}
	return found
}

// applyStagePacVars 是阶段执行器入口的统一接线:仅对**仓库 YAML 驱动的运行**(r.SpecSource=repo)
// 生效——解析「变量与缓存」里填写的 PAC 变量值,替换本阶段字符串表面的 ${NAME} 占位符;
// required: true 的变量未填值时返回缺失变量名(调用方拦截本阶段并失败,日志只列名)。
//
// 任何内部错误(配置读不到等)都不中断执行:占位符原样保留,由下游按未配置呈现
// (与 PAC「绝不因配置问题中断运行」的铁律一致);required 拦截是唯一例外——它就是
// 「配置问题要在运行前显式暴露」的产品语义。
func (b *Builder) applyStagePacVars(ctx context.Context, r *run.Run, stage *pipeline.Stage) []string {
	if r == nil || r.SpecSource.Source != run.SpecSourceRepo || stage == nil {
		return nil
	}
	_, settings, err := b.resolve(ctx, r)
	if err != nil || settings == nil || len(settings.PacVars.Defs) == 0 {
		return nil
	}
	values, unresolved := ResolvePacVarValues(settings.PacVars, b.revealPacSecret)
	if miss := MissingRequiredPacVars(settings.PacVars.Defs, unresolved); len(miss) > 0 {
		return miss
	}
	ApplyPacVarsToStage(stage, values)
	return nil
}
