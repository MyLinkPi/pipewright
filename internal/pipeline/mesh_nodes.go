// mesh_nodes.go 是集群节点的保存期校验(挂 normalizeSpec 钩子)+ pushed 源不变式。
//
//   - validateMeshNodes:mesh_push 必填目标节点与条目;mesh_trigger 必填目标节点,
//     explicit 寻址必填对方项目;pushed_source 只能出现在 source 阶段,且与 git_source
//     互斥(一条流水线只能有一种源)。
//   - Save 级的项目源类型匹配(pushed 项目必须用 pushed_source,git 项目必须用 git_source)
//     在 service.Save 里查库校验(需要项目上下文;NormalizeSpec 纯函数层不触库)。
package pipeline

import (
	"fmt"
	"strings"
)

// 节点类型常量(与 build 包判别值一致;本包不依赖 build,各自定义字符串字面量)。
const (
	JobTypeGitSource    = "git_source"
	JobTypePushedSource = "pushed_source"
	JobTypeMeshPush     = "mesh_push"
	JobTypeMeshTrigger  = "mesh_trigger"
)

// 源类型枚举(与 project 包一致;本包不依赖 project,字面量独立定义)。
const (
	sourceTypeGit    = "git"
	sourceTypePushed = "pushed"
)

// validateMeshNodes 校验集群节点配置与 pushed 源不变式。
func validateMeshNodes(stages []Stage) error {
	hasGit, hasPushed := false, false
	for _, st := range stages {
		for _, jb := range st.Jobs {
			t := strings.TrimSpace(jb.Type)
			switch t {
			case JobTypeMeshPush:
				if strings.TrimSpace(str(jb.Config["node"])) == "" {
					return fmt.Errorf("%w: mesh_push 节点「%s」未指定目标节点", ErrInvalidJob, jb.Name)
				}
				// 条目可空(空=推送本 run 全部产物),不强制。
			case JobTypeMeshTrigger:
				if strings.TrimSpace(str(jb.Config["node"])) == "" {
					return fmt.Errorf("%w: mesh_trigger 节点「%s」未指定目标节点", ErrInvalidJob, jb.Name)
				}
				mode := strings.TrimSpace(str(jb.Config["targetMode"]))
				if mode == "" || mode == "explicit" {
					if strings.TrimSpace(str(jb.Config["project"])) == "" {
						return fmt.Errorf("%w: mesh_trigger 节点「%s」未指定对方项目", ErrInvalidJob, jb.Name)
					}
				} else if mode != "origin" {
					return fmt.Errorf("%w: mesh_trigger 节点「%s」的寻址模式必须是 origin|explicit", ErrInvalidJob, jb.Name)
				}
			case JobTypePushedSource:
				if st.Kind != KindSource {
					return fmt.Errorf("%w: pushed_source 节点「%s」只能在源阶段", ErrInvalidJob, jb.Name)
				}
				hasPushed = true
			case JobTypeGitSource:
				if st.Kind == KindSource {
					hasGit = true
				}
			}
		}
	}
	if hasGit && hasPushed {
		return fmt.Errorf("%w: git_source 与 pushed_source 不能同时出现(流水线只能有一种源)", ErrInvalidStage)
	}
	return nil
}

// str 宽容取字符串(config 值可能是任意 JSON 形状)。
func str(v any) string {
	s, _ := v.(string)
	return s
}

// validateSourceTypeForProject 在 Save 级做项目源类型匹配(需触库,放 service 层):
// pushed 项目必须用 pushed_source;git 项目必须用 git_source。
func validateSourceTypeForProject(stages []Stage, projectSourceType string) error {
	hasPushed := false
	hasGit := false
	for _, st := range stages {
		if st.Kind != KindSource {
			continue
		}
		for _, jb := range st.Jobs {
			switch strings.TrimSpace(jb.Type) {
			case JobTypePushedSource:
				hasPushed = true
			case JobTypeGitSource:
				hasGit = true
			}
		}
	}
	want := JobTypeGitSource
	if projectSourceType == sourceTypePushed {
		want = JobTypePushedSource
	}
	if want == JobTypePushedSource && !hasPushed {
		return fmt.Errorf("%w: 被推送源项目的源阶段必须使用 pushed_source 节点", ErrInvalidStage)
	}
	if want == JobTypeGitSource && hasPushed {
		return fmt.Errorf("%w: git 项目的源阶段不能使用 pushed_source 节点", ErrInvalidStage)
	}
	_ = hasGit
	return nil
}
