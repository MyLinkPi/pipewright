// receiver.go 是集群推送接收侧编排:清单校验 → blob 入库(内容寻址去重)→ pushed 项目
// 幂等定位/自动创建 → 镜像归档导入本机镜像面 → 修订落库。
//
// 依赖均以最小能力面注入(便于单测 fake):项目创建(project.Service 原样满足其子集)、
// 制品库(artifactstore.Store 原样满足)、镜像导入器(build 侧实现:docker load + 推内置
// registry)、修订存储(pushsource.Store)。
package mesh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/huangchengsir/pipewright/internal/project"
	"github.com/huangchengsir/pipewright/internal/pushsource"
)

// BlobPort 抽象「内容寻址制品库」(artifactstore.Store 原样满足)。
type BlobPort interface {
	Put(r io.Reader) (key string, size int64, err error)
	Has(key string) bool
	Open(key string) (io.ReadCloser, error)
}

// ImageImporter 抽象「把 docker save 归档导入本机可用镜像面」(build 侧实现:
// docker load → tag → 推内置 registry)。返回导入后的可 pull 引用与 digest。
type ImageImporter interface {
	Import(ctx context.Context, archive io.Reader, repo, tag string) (ref, digest string, err error)
}

// Receiver 是推送接收编排服务。零值不可用,经 NewReceiver 构造;线程安全(无共享可变态)。
type Receiver struct {
	projects  project.Service
	blobs     BlobPort
	images    ImageImporter
	revisions pushsource.Store
}

// NewReceiver 构造接收器。images 为 nil 时镜像条目入库但**不导入**(引用标记 pending,
// 物化时按归档现场导入的兜底路径本版不启用,导入失败即推送失败)。
func NewReceiver(p project.Service, b BlobPort, img ImageImporter, revs pushsource.Store) *Receiver {
	return &Receiver{projects: p, blobs: b, images: img, revisions: revs}
}

// PushIngest 是一次推送的接收会话:Begin(清单) → 逐 Blob(流式)→ Commit(原子收尾)。
type PushIngest struct {
	manifest PushManifest
	stored   map[string]bool // 已确认入库的 blob sha
}

// tagSafeRe 收敛修订标签/镜像 repo:tag 的字符集(小写字母数字、-、_、.)。
var tagSafeRe = regexp.MustCompile(`[^a-z0-9_.-]+`)

func sanitizeTagPart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = tagSafeRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-.")
}

// Begin 校验清单并开始接收(项目创建与镜像导入推迟到 Commit,失败不留半套数据)。
func (r *Receiver) Begin(manifest PushManifest) (*PushIngest, error) {
	manifest.OriginNodeID = strings.TrimSpace(manifest.OriginNodeID)
	manifest.OriginProjectRef = strings.TrimSpace(manifest.OriginProjectRef)
	manifest.OriginProjectName = strings.TrimSpace(manifest.OriginProjectName)
	if manifest.OriginNodeID == "" || manifest.OriginProjectRef == "" {
		return nil, fmt.Errorf("%w: manifest missing origin", ErrInvalidInput)
	}
	if len(manifest.Items) == 0 {
		return nil, fmt.Errorf("%w: manifest has no items", ErrInvalidInput)
	}
	seen := make(map[string]struct{}, len(manifest.Items))
	for i := range manifest.Items {
		it := &manifest.Items[i]
		it.Name = strings.TrimSpace(it.Name)
		it.SHA256 = strings.TrimSpace(strings.ToLower(it.SHA256))
		if it.Name == "" || it.SHA256 == "" || (it.Kind != "file" && it.Kind != "image") {
			return nil, fmt.Errorf("%w: item %d invalid", ErrInvalidInput, i)
		}
		if _, dup := seen[it.Name]; dup {
			return nil, fmt.Errorf("%w: duplicate item name %q", ErrInvalidInput, it.Name)
		}
		seen[it.Name] = struct{}{}
	}
	return &PushIngest{manifest: manifest, stored: make(map[string]bool, len(manifest.Items))}, nil
}

// MissingBlobs 报告清单中接收方尚未持有的 blob(推送方 preflight 后可只传缺失段)。
func (r *Receiver) MissingBlobs(manifest PushManifest) []string {
	missing := []string{}
	seen := map[string]struct{}{}
	for _, it := range manifest.Items {
		if _, dup := seen[it.SHA256]; dup {
			continue
		}
		seen[it.SHA256] = struct{}{}
		if !r.blobs.Has(it.SHA256) {
			missing = append(missing, it.SHA256)
		}
	}
	return missing
}

// Blob 接收一段 blob 流:已持有(内容寻址去重)直接丢弃确认;否则入库并校验内容哈希
// 与清单声明一致(不一致 → 错误,整次推送失败)。
func (ing *PushIngest) Blob(r *Receiver, sha string, rd io.Reader) error {
	sha = strings.TrimSpace(strings.ToLower(sha))
	if sha == "" {
		return fmt.Errorf("%w: empty blob key", ErrInvalidInput)
	}
	if r.blobs.Has(sha) {
		_, _ = io.Copy(io.Discard, rd)
		ing.stored[sha] = true
		return nil
	}
	key, _, err := r.blobs.Put(rd)
	if err != nil {
		return fmt.Errorf("mesh: store blob: %w", err)
	}
	if key != sha {
		// 内容与声明不符:拒绝(防传输损坏/篡改);已写 blob 由内容寻址自然无害。
		return fmt.Errorf("%w: blob hash mismatch (declared %s got %s)", ErrInvalidInput, sha, key)
	}
	ing.stored[sha] = true
	return nil
}

// Commit 收尾:幂等定位/创建 pushed 项目 → 镜像导入 → 修订落库。
// blob 缺失(发送方漏传且接收方没有)→ 错误;已写 blob 内容寻址幂等,重推只补缺。
func (r *Receiver) Commit(ctx context.Context, ing *PushIngest) (*PushResult, error) {
	m := ing.manifest
	for _, it := range m.Items {
		if !ing.stored[it.SHA256] && !r.blobs.Has(it.SHA256) {
			return nil, fmt.Errorf("%w: blob %s not received", ErrInvalidInput, it.SHA256[:min(12, len(it.SHA256))])
		}
	}

	created := false
	p, err := r.projects.GetByOrigin(ctx, m.OriginNodeID, m.OriginProjectRef)
	if errors.Is(err, project.ErrNotFound) {
		created = true
		p, err = r.projects.CreatePushed(ctx, project.PushedCreateInput{
			OriginNodeID:     m.OriginNodeID,
			OriginProjectRef: m.OriginProjectRef,
			Name:             m.OriginProjectName,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("mesh: ensure pushed project: %w", err)
	}

	label := sanitizeTagPart(m.RevisionLabel)
	if label == "" {
		label = fmt.Sprintf("rev-%s", m.OriginRunID)
		if len(label) > 40 {
			label = label[:40]
		}
	}

	items := make([]pushsource.Item, 0, len(m.Items))
	for _, it := range m.Items {
		item := pushsource.Item{
			Name:     it.Name,
			Kind:     it.Kind,
			SHA256:   it.SHA256,
			Size:     it.Size,
			DestName: sanitizeRelPath(it.DestName),
		}
		if it.Kind == pushsource.KindImage {
			// 跨推送去重:镜像 digest 稳定(归档哈希每次导出可能不同),优先按 digest 复用;
			// 无 digest 时回退 (name, sha) 精确匹配。命中 → 复用已导入引用,免重复 load/push。
			if ref := r.findImportedRef(ctx, p.ID, it.Name, it.SHA256, it.Digest); ref != "" {
				item.ImageRef = ref
			} else if r.images != nil {
				arc, oerr := r.blobs.Open(it.SHA256)
				if oerr != nil {
					return nil, fmt.Errorf("mesh: open image archive: %w", oerr)
				}
				repo := "pipewright/" + sanitizeTagPart(p.Name+"-"+it.Name)
				tag := label + "-" + it.SHA256[min(7, len(it.SHA256)):min(14, len(it.SHA256))]
				ref, digest, ierr := r.images.Import(ctx, arc, repo, tag)
				_ = arc.Close()
				if ierr != nil {
					return nil, fmt.Errorf("mesh: import image %s: %w", it.Name, ierr)
				}
				item.ImageRef = ref
				if digest != "" {
					item.Digest = digest
				}
			}
		}
		items = append(items, item)
	}

	rev, err := r.revisions.Create(ctx, pushsource.Revision{
		ProjectID:    p.ID,
		Label:        label,
		OriginNodeID: m.OriginNodeID,
		OriginRunID:  m.OriginRunID,
		Items:        items,
	})
	if err != nil {
		return nil, fmt.Errorf("mesh: create revision: %w", err)
	}
	return &PushResult{ProjectID: p.ID, RevisionID: rev.ID, Created: created}, nil
}

// findImportedRef 在既有修订里找已导入条目,复用其引用:digest 匹配优先(稳定),
// 回退 (name, sha) 精确匹配。
func (r *Receiver) findImportedRef(ctx context.Context, projectID, name, sha, digest string) string {
	revs, err := r.revisions.List(ctx, projectID, 100)
	if err != nil {
		return ""
	}
	if digest != "" {
		for _, rev := range revs {
			for _, it := range rev.Items {
				if it.Digest == digest && it.ImageRef != "" {
					return it.ImageRef
				}
			}
		}
	}
	for _, rev := range revs {
		for _, it := range rev.Items {
			if it.Name == name && it.SHA256 == sha && it.ImageRef != "" {
				return it.ImageRef
			}
		}
	}
	return ""
}

// sanitizeRelPath 收敛 file 条目恢复进工作区的相对路径:去 ..、去盘符/前导斜杠,
// 反斜杠归一为 /(与平台工作区相对路径约定一致)。
func sanitizeRelPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	parts := make([]string, 0)
	for _, seg := range strings.Split(p, "/") {
		seg = strings.TrimSpace(seg)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "/")
}
