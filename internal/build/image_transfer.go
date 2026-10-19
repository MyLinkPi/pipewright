// image_transfer.go 是集群推送两侧的镜像传输原语(控制机本地容器 CLI):
//   - Export:docker save 归档镜像到临时文件(发送侧;归档哈希在发送前可知);
//   - Import:docker load 归档 → tag 到 <内置registry>/<repo>:<tag> → push → 读 digest
//     (接收侧;归档可直接从制品库流式喂入 stdin)。
//
// 命令一律 array 化不经 shell(AC-SEC-02);二进制名复用 Driver 探测结果(docker/nerdctl/podman)。
package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// ErrImageTransfer 表示镜像导出/导入命令失败(错误体不含敏感数据)。
var ErrImageTransfer = errors.New("build: image transfer failed")

// ImageTransfer 是镜像导出/导入器。零值不可用(Binary 必填);线程安全(无状态)。
type ImageTransfer struct {
	// Binary 是容器 CLI 名(docker/nerdctl/podman),由装配层取自 Driver.Binary()。
	Binary string
	// BuiltinRegistry 解析内置本地 registry 地址(接收侧 tag/push 目标);nil/ok=false → 导入失败。
	BuiltinRegistry func(ctx context.Context) (addr string, ok bool)
	// OnLine 逐行喂日志(可空)。
	OnLine func(stream, line string)
}

func (t *ImageTransfer) log(stream, format string, args ...any) {
	if t.OnLine != nil {
		t.OnLine(stream, fmt.Sprintf(format, args...))
	}
}

func (t *ImageTransfer) binary() string {
	if t.Binary == "" {
		return "docker"
	}
	return t.Binary
}

// runLine 执行一条容器 CLI 嘉令并返回 stdout(失败 → ErrImageTransfer 包装)。
func (t *ImageTransfer) runLine(ctx context.Context, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, t.binary(), args...)
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.log(streamError, "  %s %v 失败:%s", t.binary(), args[:1], strings.TrimSpace(errBuf.String()))
		return "", fmt.Errorf("%w: %s %s", ErrImageTransfer, t.binary(), args[0])
	}
	return out.String(), nil
}

// Export 把镜像归档(docker save)写到 destPath(发送侧;调用方负责清理临时文件)。
func (t *ImageTransfer) Export(ctx context.Context, ref, destPath string) error {
	args := []string{"save", "-o", destPath, ref}
	if _, err := t.runLine(ctx, args); err != nil {
		return err
	}
	t.log("stdout", "  已导出镜像归档:%s → %s", ref, destPath)
	return nil
}

// loadedImageRe 匹配 docker load 输出的 "Loaded image: <ref>" 行(nerdctl/podman 同款语义)。
var loadedImageRe = regexp.MustCompile(`(?i)loaded\s+image(?:\s+id)?:\s*(\S+)`)

// Import 从归档流加载镜像并推入内置 registry(接收侧)。返回可 pull 引用与镜像 digest。
// 归档内的镜像按 load 输出解析(多镜像取首个);tag 目标 repo:tag 由调用方给出。
func (t *ImageTransfer) Import(ctx context.Context, archive io.Reader, repo, tag string) (string, string, error) {
	if t.BuiltinRegistry == nil {
		return "", "", fmt.Errorf("%w: builtin registry not configured", ErrImageTransfer)
	}
	addr, ok := t.BuiltinRegistry(ctx)
	if !ok || addr == "" {
		return "", "", fmt.Errorf("%w: builtin registry unavailable", ErrImageTransfer)
	}

	// docker load(无 -i,读 stdin):输出 "Loaded image: <ref>"。
	cmd := exec.CommandContext(ctx, t.binary(), "load")
	cmd.Stdin = archive
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", "", fmt.Errorf("%w: load (%s)", ErrImageTransfer, strings.TrimSpace(errBuf.String()))
	}
	m := loadedImageRe.FindStringSubmatch(out.String())
	if m == nil {
		return "", "", fmt.Errorf("%w: load output has no image ref", ErrImageTransfer)
	}
	loadedRef := strings.Trim(strings.TrimSpace(m[1]), `"`)
	target := strings.TrimSuffix(addr, "/") + "/" + strings.TrimPrefix(repo, "/") + ":" + tag

	if _, err := t.runLine(ctx, []string{"tag", loadedRef, target}); err != nil {
		return "", "", err
	}
	if _, err := t.runLine(ctx, []string{"push", target}); err != nil {
		return "", "", err
	}
	digestOut, err := t.runLine(ctx, []string{"inspect", "--format", "{{.Id}}", target})
	if err != nil {
		return target, "", nil // inspect 失败不致命:digest 缺省
	}
	digest := strings.TrimSpace(digestOut)
	t.log("stdout", "  镜像已导入内置 registry:%s(%s)", target, digest)
	return target, digest, nil
}

// exportImageArchiveToTemp 导出镜像到临时文件并返回路径(发送侧;调用方 defer os.Remove)。
func (t *ImageTransfer) exportImageArchiveToTemp(ctx context.Context, ref string) (string, error) {
	tmp, err := os.CreateTemp("", "pw-mesh-img-*.tar")
	if err != nil {
		return "", fmt.Errorf("build: create temp archive: %w", err)
	}
	name := tmp.Name()
	_ = tmp.Close()
	if err := t.Export(ctx, ref, name); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// EnsureLocal 确保镜像在本地 daemon 存在:先 image inspect 探测,不在则 pull
// (产物管理页「导出镜像 tar」前置步)。内置 registry 无鉴权可直拉;外置私有 registry
// 可能因无凭据 pull 失败 → 诚实返回 ErrImageTransfer 包装错误。命令 array 化不经 shell。
func (t *ImageTransfer) EnsureLocal(ctx context.Context, ref string) error {
	if strings.TrimSpace(ref) == "" {
		return fmt.Errorf("%w: empty image ref", ErrImageTransfer)
	}
	// image inspect:0 退出码 = 已在本地;-1(命令不存在/不存在该镜像)→ 尝试 pull。
	if _, err := t.runLine(ctx, []string{"image", "inspect", ref}); err == nil {
		return nil
	}
	if _, err := t.runLine(ctx, []string{"pull", ref}); err != nil {
		return fmt.Errorf("%w: pull %s(外置私有仓库需凭据时不可拉取)", ErrImageTransfer, ref)
	}
	return nil
}

// ExportStream 把镜像归档(docker save,不带 -o)流式写到 w(产物管理页镜像下载:
// stdout 直连 HTTP 响应,不落临时文件、不占额外磁盘)。失败 → ErrImageTransfer 包装。
func (t *ImageTransfer) ExportStream(ctx context.Context, ref string, w io.Writer) error {
	cmd := exec.CommandContext(ctx, t.binary(), "save", ref)
	cmd.Stdout = w
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		t.log(streamError, "  %s save 失败:%s", t.binary(), strings.TrimSpace(errBuf.String()))
		return fmt.Errorf("%w: save %s", ErrImageTransfer, ref)
	}
	return nil
}
