package build

import (
	"context"
	"errors"
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

// runPlaceholder 匹配节点 config 里受支持的本次运行占位;date 支持 {{date}} 与
// {{date:格式}} 两种形态(格式字符集宽匹配,合法性由 imageDateLayout 严格校验)。
var runPlaceholder = regexp.MustCompile(`\{\{\s*(runId|branch|commit|project|date(?::[^{}]*)?)\s*\}\}`)

// renderRunPlaceholders 把字符串里的通用占位替换为本次运行的值:
// {{runId}} / {{branch}} / {{commit}} / {{project}} / {{date}} / {{date:格式}}。
// 未知占位原样保留(不报错,既有语义);date 语法/格式非法则显式报错(移植上游
// expandImageTag 的严格性)。日期用 run 创建时刻(已持久化)按 UTC 渲染——同一
// run 并行/先后执行的多个节点得到相同日期(确定性,不随任务执行时刻漂移)。
// 由 oss_upload 的 prefix 与 push_image 的 tag 共用。
func renderRunPlaceholders(s string, r *run.Run) (string, error) {
	if s == "" || !strings.Contains(s, "{{") {
		return s, nil
	}
	repl := map[string]string{
		"runId":   r.ID,
		"branch":  r.Trigger.Branch,
		"commit":  strings.TrimSpace(r.Trigger.Commit),
		"project": r.ProjectName,
	}
	var renderErr error
	out := runPlaceholder.ReplaceAllStringFunc(s, func(m string) string {
		name := runPlaceholder.FindStringSubmatch(m)[1]
		if !strings.HasPrefix(name, "date") {
			if v, ok := repl[name]; ok {
				return v
			}
			return m
		}
		if renderErr != nil {
			return m
		}
		format := "yyyy-MM-dd-HH-mm"
		if suffix, ok := strings.CutPrefix(name, "date:"); ok && strings.TrimSpace(suffix) != "" {
			format = strings.TrimSpace(suffix)
		}
		layout, err := imageDateLayout(format)
		if err != nil {
			renderErr = fmt.Errorf("日期占位格式非法(%q):%w", format, err)
			return m
		}
		if r.CreatedAt.IsZero() {
			renderErr = errors.New("run 创建时间不可用,无法渲染日期占位")
			return m
		}
		return r.CreatedAt.UTC().Format(layout)
	})
	if renderErr != nil {
		return "", renderErr
	}
	return out, nil
}

// imageDateLayout 把日期格式模板翻译为 Go layout。仅允许 yyyy/MM/dd/HH/mm/ss
// 六个 token 与 - _ . 三种分隔符(移植上游 89ed90d,防注入 Go layout 语义)。
func imageDateLayout(format string) (string, error) {
	bad := errors.New("仅支持 yyyy、MM、dd、HH、mm、ss 与 - _ . 分隔符")
	if format == "" || len(format) > 128 {
		return "", bad
	}
	var layout strings.Builder
	tokens := 0
	for format != "" {
		matched := false
		for _, token := range []struct{ name, layout string }{
			{"yyyy", "2006"}, {"MM", "01"}, {"dd", "02"},
			{"HH", "15"}, {"mm", "04"}, {"ss", "05"},
		} {
			if strings.HasPrefix(format, token.name) {
				layout.WriteString(token.layout)
				format = format[len(token.name):]
				tokens++
				matched = true
				break
			}
		}
		if matched {
			continue
		}
		if !strings.ContainsRune("-_.", rune(format[0])) {
			return "", bad
		}
		layout.WriteByte(format[0])
		format = format[1:]
	}
	if tokens == 0 {
		return "", bad
	}
	return layout.String(), nil
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
	prefix, perr := renderRunPlaceholders(cfgString(jb.Config, "prefix"), r)
	if perr != nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:prefix 占位渲染失败:"+perr.Error())
		return ErrBuildFailed
	}
	n, uerr := b.uploadArtifact(ctx, rep, storageID, prefix, *art)
	if uerr != nil {
		_ = rep.Log(ctx, streamStderr, "OSS 上传节点:上传失败:"+uerr.Error())
		return ErrBuildFailed
	}
	_ = rep.Log(ctx, streamStdout, fmt.Sprintf("· OSS 上传完成:共 %d 个对象(来源节点 %q)", n, artifactJob))
	return nil
}
