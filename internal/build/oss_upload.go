package build

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/huangchengsir/pipewright/internal/dagrun"
	"github.com/huangchengsir/pipewright/internal/pipeline"
	"github.com/huangchengsir/pipewright/internal/run"
)

// oss_upload.go 实现「OSS 上传节点」:把一个上游节点产出的**文件类产物**上传到对象存储
// (本期仅阿里云 OSS,经 b.objectUploader = objectstore.Service 落地)。失败令阶段失败、阻断下游
// (强失败语义,对齐 deploy_ssh)。

// isObjectUploadJob 判断是否「OSS 上传」节点(画布 oss_upload 类型)。
func isObjectUploadJob(jobType string) bool {
	return strings.TrimSpace(jobType) == "oss_upload"
}

// ossPrefixPlaceholder 匹配 oss_upload 节点 prefix 里受支持的占位。
var ossPrefixPlaceholder = regexp.MustCompile(`\{\{\s*(runId|branch|commit|project)\s*\}\}`)

// renderOssPrefix 把 prefix 里的通用占位替换为本次运行的值:
// {{runId}} / {{branch}} / {{commit}} / {{project}}。未知占位原样保留(不报错)。
func renderOssPrefix(prefix string, r *run.Run) string {
	if prefix == "" || !strings.Contains(prefix, "{{") {
		return prefix
	}
	repl := map[string]string{
		"runId":   r.ID,
		"branch":  r.Trigger.Branch,
		"commit":  strings.TrimSpace(r.Trigger.Commit),
		"project": r.ProjectName,
	}
	return ossPrefixPlaceholder.ReplaceAllStringFunc(prefix, func(m string) string {
		return repl[ossPrefixPlaceholder.FindStringSubmatch(m)[1]]
	})
}

// joinObjectKey 拼接对象键:prefix 与 name 各去首尾斜杠,空侧省略分隔符。
func joinObjectKey(prefix, name string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	name = strings.Trim(strings.TrimSpace(name), "/")
	switch {
	case prefix == "":
		return name
	case name == "":
		return prefix
	default:
		return prefix + "/" + name
	}
}

// uploadableArtifact 报告产物是否为「有真字节可上传」的文件类产物(非镜像 + 已归档到制品库)。
func uploadableArtifact(a run.Artifact) bool {
	if a.Type == run.ArtifactImage {
		return false
	}
	stored, _ := a.Metadata["stored"].(bool)
	return stored && strings.TrimSpace(a.Reference) != ""
}

// artifactUploadName 解析产物上传用的文件名:metadata.filename 优先,缺省用产物逻辑名。
func artifactUploadName(a run.Artifact) string {
	if n, ok := a.Metadata["filename"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	return a.Name
}

// pickUploadArtifact 从本 run 产物里按「来源节点」精确挑一件可上传(文件类)产物。
// 与 deploy 的 pickStageArtifactExplicit 同口径(按 metadata.sourceJob 匹配 + artifactName glob 消歧),
// 但只接受文件类产物;命中镜像产物 → 明确报错(应改用「推送镜像」节点)。
func pickUploadArtifact(arts []run.Artifact, sourceJob, nameGlob string) (*run.Artifact, error) {
	var cands []run.Artifact
	for _, a := range arts {
		if strings.TrimSpace(fmt.Sprint(a.Metadata["sourceJob"])) != sourceJob {
			continue
		}
		cands = append(cands, a)
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("来源节点 %q 未产出产物(检查该节点 artifactPath / 上游阶段顺序)", sourceJob)
	}
	if nameGlob != "" {
		var matched []run.Artifact
		for _, a := range cands {
			base := ""
			if p, ok := a.Metadata["workspacePath"].(string); ok {
				base = path.Base(p)
			}
			if ok, _ := path.Match(nameGlob, a.Name); ok {
				matched = append(matched, a)
			} else if base != "" {
				if ok2, _ := path.Match(nameGlob, base); ok2 {
					matched = append(matched, a)
				}
			}
		}
		if len(matched) == 0 {
			names := make([]string, 0, len(cands))
			for _, a := range cands {
				names = append(names, a.Name)
			}
			return nil, fmt.Errorf("来源节点 %q 无产物匹配 %q(实际产物:%s)", sourceJob, nameGlob, strings.Join(names, ", "))
		}
		cands = matched
	}
	for i := range cands {
		if uploadableArtifact(cands[i]) {
			return &cands[i], nil
		}
	}
	return nil, fmt.Errorf("来源节点 %q 无可上传的文件类产物(镜像产物请改用「推送镜像」节点)", sourceJob)
}

// uploadArtifact 把一件产物上传到对象存储(prefix 为对象键前缀),返回上传对象数。
// format=files → 按 metadata 清单逐文件上传(保留相对路径目录结构);其余(format=file/tar.gz)
// 单流上传。任一步失败 → 错误(调用方令阶段失败)。
func (b *Builder) uploadArtifact(ctx context.Context, rep dagrun.StageReporter, storageID, prefix string, a run.Artifact) (int, error) {
	format, _ := a.Metadata["format"].(string)
	if format == "files" {
		files, err := decodeFilesManifest(a.Metadata)
		if err != nil {
			return 0, err
		}
		n := 0
		for _, fe := range files {
			key := joinObjectKey(prefix, fe.Path)
			rc, err := b.artStore.Open(fe.Key)
			if err != nil {
				return n, fmt.Errorf("取产物文件 %s 失败:%w", fe.Path, err)
			}
			uerr := b.objectUploader.Upload(ctx, storageID, key, rc)
			_ = rc.Close()
			if uerr != nil {
				return n, fmt.Errorf("上传 %s 失败:%w", key, uerr)
			}
			_ = rep.Log(ctx, streamStdout, "→ 已上传 "+key)
			n++
		}
		return n, nil
	}
	rc, err := b.artStore.Open(a.Reference)
	if err != nil {
		return 0, fmt.Errorf("取产物失败:%w", err)
	}
	defer func() { _ = rc.Close() }()
	key := joinObjectKey(prefix, artifactUploadName(a))
	if uerr := b.objectUploader.Upload(ctx, storageID, key, rc); uerr != nil {
		return 0, fmt.Errorf("上传 %s 失败:%w", key, uerr)
	}
	_ = rep.Log(ctx, streamStdout, "→ 已上传 "+key)
	return 1, nil
}

// runOssUploadJob 执行一个 oss_upload 节点:按 config(storageId/artifactJob/artifactName/prefix)
// 从本 run 产物里精确挑一件文件类产物,上传到对象存储。失败 → ErrBuildFailed(阻断下游)。
// 对象存储服务未注入(测试/未装配)→ 记日志跳过(向后兼容,与 deployer nil 同)。
func (b *Builder) runOssUploadJob(ctx context.Context, rep dagrun.StageReporter, jb pipeline.Job, r *run.Run) error {
	if b.objectUploader == nil {
		_ = rep.Log(ctx, streamStdout, "· OSS 上传节点:对象存储服务未注入,跳过")
		return nil
	}
	storageID := strings.TrimSpace(cfgString(jb.Config, "storageId"))
	if storageID == "" {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:未选择对象存储配置(storageId),无法上传")
		return ErrBuildFailed
	}
	artifactJob := strings.TrimSpace(cfgString(jb.Config, "artifactJob"))
	if artifactJob == "" {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:未指定产物来源节点(artifactJob)")
		return ErrBuildFailed
	}
	if b.artifactLister == nil || b.artStore == nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:制品库未装配,无法读取产物")
		return ErrBuildFailed
	}
	arts, err := b.artifactLister(ctx, r.ID)
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:读取产物列表失败:"+err.Error())
		return ErrBuildFailed
	}
	art, err := pickUploadArtifact(arts, artifactJob, strings.TrimSpace(cfgString(jb.Config, "artifactName")))
	if err != nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:"+err.Error())
		return ErrBuildFailed
	}
	prefix := renderOssPrefix(cfgString(jb.Config, "prefix"), r)
	n, uerr := b.uploadArtifact(ctx, rep, storageID, prefix, *art)
	if uerr != nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:上传失败:"+uerr.Error())
		return ErrBuildFailed
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· OSS 上传完成:共 %d 个对象(来源节点 %q)", n, artifactJob))
	return nil
}
