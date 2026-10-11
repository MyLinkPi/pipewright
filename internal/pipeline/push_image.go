package pipeline

import (
	"fmt"
	"strings"
)

// push_image.go 校验推送镜像节点(config["artifactJob"])的产物来源节点引用。
//
// 背景:push_image 节点把某个 build_image 节点产出的镜像推送到镜像仓库,artifactJob = 该节点
// **展示名**(与 deploy_ssh / oss_upload 的 artifactJob 同一命名惯例)。
//
// 与前者的差异:镜像推送在**来源 build_image 节点内**完成 —— 它持有本地镜像 tag,且产物引用必须在
// 登记(emit)前改写为远端引用,下游部署节点才能 pull(详见 internal/build/push_image.go)。因此
// 来源必须是**同阶段**的 build_image 节点:跨阶段时来源阶段已执行完,无法再按本节点声明改推。
const (
	pushImageType           = "push_image"
	pushImageArtifactJobKey = "artifactJob"
	buildImageType          = "build_image"
	buildArtifactTypeKey    = "artifactType"
)

// validatePushImage 校验全部 push_image 节点的 artifactJob 引用(错误归一为 ErrInvalidJob):
// 必填、命中同阶段节点、且该节点是 build_image 且产物类型为镜像。
func validatePushImage(stages []Stage) error {
	for si := range stages {
		stage := stages[si]
		byName := make(map[string]Job, len(stage.Jobs))
		for _, jb := range stage.Jobs {
			if name := strings.TrimSpace(jb.Name); name != "" {
				byName[name] = jb
			}
		}
		for _, jb := range stage.Jobs {
			if strings.TrimSpace(jb.Type) != pushImageType {
				continue
			}
			raw, ok := jb.Config[pushImageArtifactJobKey]
			if !ok || raw == nil {
				return fmt.Errorf("%w: push_image job %q: artifactJob is required (select a build_image node in the same stage)", ErrInvalidJob, jb.Name)
			}
			name, isStr := raw.(string)
			if !isStr {
				return fmt.Errorf("%w: push_image job %q: artifactJob must be a string (node name)", ErrInvalidJob, jb.Name)
			}
			name = strings.TrimSpace(name)
			if name == "" {
				return fmt.Errorf("%w: push_image job %q: artifactJob is required (select a build_image node in the same stage)", ErrInvalidJob, jb.Name)
			}
			src, exists := byName[name]
			if !exists {
				return fmt.Errorf(
					"%w: push_image job %q: artifactJob %q must reference a build_image node in the SAME stage %q (image push runs inside the source node)",
					ErrInvalidJob, jb.Name, name, stage.Name)
			}
			if strings.TrimSpace(src.Type) != buildImageType {
				return fmt.Errorf("%w: push_image job %q: artifactJob %q must reference a build_image node (got %q)", ErrInvalidJob, jb.Name, name, src.Type)
			}
			if at := strings.TrimSpace(jobConfigString(src.Config, buildArtifactTypeKey)); at != "" && at != ArtifactImage {
				return fmt.Errorf("%w: push_image job %q: artifactJob %q builds a %q artifact, not an image", ErrInvalidJob, jb.Name, name, at)
			}
		}
	}
	return nil
}
