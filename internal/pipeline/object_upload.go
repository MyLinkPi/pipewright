package pipeline

import (
	"fmt"
	"strings"
)

// object_upload.go 校验 OSS 上传节点(config["artifactJob"])的产物来源节点引用。
//
// 背景:oss_upload 节点需要一个上游节点产出的文件类产物,artifactJob = 该节点**展示名**
// (与 deploy_ssh 的 artifactJob / notify 的 statusFrom 同一命名惯例)。保存期校验保证「上游」
// 成立、且引用非空且命中真实节点,避免配了却在上传时找不到产物。
//
// 上游判定与 notify_status.go 的 guaranteedBeforeNotify 同口径(跨阶段走 needs 传递闭包 /
// 线性退化按声明序;同阶段走 job needs 闭包)。
const (
	objectUploadType            = "oss_upload"
	objectUploadArtifactJobKey  = "artifactJob"
)

// validateObjectUpload 校验全部 oss_upload 节点的 artifactJob 引用(错误归一为 ErrInvalidJob)。
func validateObjectUpload(stages []Stage) error {
	anyStageNeeds := false
	for _, st := range stages {
		if len(st.Needs) > 0 {
			anyStageNeeds = true
			break
		}
	}

	for si := range stages {
		for ji := range stages[si].Jobs {
			n := stages[si].Jobs[ji]
			if strings.TrimSpace(n.Type) != objectUploadType {
				continue
			}
			raw, ok := n.Config[objectUploadArtifactJobKey]
			if !ok || raw == nil {
				return fmt.Errorf("%w: oss_upload job %q: artifactJob is required (select a source node)", ErrInvalidJob, n.Name)
			}
			name, isStr := raw.(string)
			if !isStr {
				return fmt.Errorf("%w: oss_upload job %q: artifactJob must be a string (node name)", ErrInvalidJob, n.Name)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("%w: oss_upload job %q: artifactJob is required (select a source node)", ErrInvalidJob, n.Name)
			}

			matched := 0
			for ti := range stages {
				for tj := range stages[ti].Jobs {
					m := stages[ti].Jobs[tj]
					if strings.TrimSpace(m.Name) != name {
						continue
					}
					matched++
					if !guaranteedBeforeNotify(stages, anyStageNeeds, si, n, ti, m) {
						return fmt.Errorf(
							"%w: oss_upload job %q: artifactJob %q must reference an upstream node (node %q in stage %q is not guaranteed to finish before it)",
							ErrInvalidJob, n.Name, name, m.Name, stages[ti].Name)
					}
				}
			}
			if matched == 0 {
				return fmt.Errorf("%w: oss_upload job %q: artifactJob %q references an unknown node", ErrInvalidJob, n.Name, name)
			}
		}
	}
	return nil
}
