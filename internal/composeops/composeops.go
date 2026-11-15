// Package composeops 提供「把一段 docker compose 内容按受管 Stacks 语义部署到
// 目标服务器」的共享操作,供应用商店与 Stacks(httpapi)复用。全部经 target.Service
// 的 array 化 SSH 命令执行(不拼 shell,AC-SEC-02);远端受管目录与 Stacks 部署共用
// 同一布局。
package composeops

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 受管目录布局(与 httpapi Stacks/应用商店同一条链路,目录语义冻结)。
const (
	// BaseDir 是远端受管 compose 栈的根目录。
	BaseDir = "/opt/pipewright/stacks"
	// ComposeFileName 是栈目录内 compose 文件名。
	ComposeFileName = "docker-compose.yml"
)

// reName 白名单校验栈名/卷名输出(防异常输出注入;与 httpapi reDockerTgt 同纪律)。
var reName = regexp.MustCompile(`^[\w][\w.-]*$`)

// reContainerHexID 校验容器 ID 输出为纯十六进制。
var reContainerHexID = regexp.MustCompile(`^[a-f0-9]{6,64}$`)

// up 重试参数:镜像拉取常因瞬态网络中断(如 connection reset by peer)失败,docker
// 会保留已下载的分层,对可识别的瞬态错误自动重试即断点续传。
const maxUpAttempts = 3 // 首跑 + 2 次重试

// upRetryBackoffs 是第 n 次重试前的退避等待;var 便于测试改短。
var upRetryBackoffs = [maxUpAttempts - 1]time.Duration{3 * time.Second, 6 * time.Second}

// rePullTransient 匹配镜像拉取/层下载的常见瞬态网络错误(stderr);非瞬态错误
// (compose 配置、守护进程拒绝等)不重试,立即失败。
var rePullTransient = regexp.MustCompile(`(?i)connection reset|EOF|i/o timeout|timed out|handshake timeout|connection refused|no route to host|temporary failure`)

// upInterruptedMsg 是 up 被请求取消中断时的人读文案。
const upInterruptedMsg = "部署被中断(页面关闭或连接断开):已下载的镜像分层会保留,重试部署将断点续传"

// Result 是一次部署/拆除的结果:OK=false 时 Err 为人读错误(绝不含凭据/内部栈)。
type Result struct {
	OK bool
	// Out 是成功时的命令输出摘要(已截断)。
	Out string
	// Err 是失败时的人读错误。
	Err string
	// Dispatched 表示命令已真实投递到远端(即使退出码非零)——调用方据此失效
	// 容器清单缓存等本端派生状态。
	Dispatched bool
}

// humanSSHError 把 SSH 层错误映射为人读文案。
func humanSSHError(err error) string {
	switch {
	case errors.Is(err, target.ErrAuth):
		return "SSH 认证失败:密钥或口令无效,或无登录权限"
	case errors.Is(err, target.ErrUnreachable):
		return "无法连接服务器:端口未开放、主机不可达或超时"
	case errors.Is(err, target.ErrInvalidCredential):
		return "凭据不是可用的 SSH 私钥或口令"
	default:
		return "执行服务操作失败:连接或命令执行错误"
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…(已截断)"
}

// truncateTail 保留失败日志末尾的根因，并避免从 UTF-8 字符中间切开。
func truncateTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return "…(前文已截断，仅保留末尾日志)\n" + s[start:]
}

// DetectComposeBin 探测可用的 compose CLI:v2 `docker compose` 优先,回退 v1
// `docker-compose`。返回基础命令;都没有返回 nil。
func DetectComposeBin(ctx context.Context, svc target.Service, serverID string) []string {
	if res, err := svc.Exec(ctx, serverID, []string{"docker", "compose", "version"}); err == nil && res.ExitCode == 0 {
		return []string{"docker", "compose"}
	}
	if res, err := svc.Exec(ctx, serverID, []string{"docker-compose", "version"}); err == nil && res.ExitCode == 0 {
		return []string{"docker-compose"}
	}
	return nil
}

// Deploy 把一段 compose 内容部署为受管 Stacks 项目:建受管目录 → Upload compose
// 文件(字节流)→ 探测 compose CLI → `compose -p <name> up -d`。
func Deploy(ctx context.Context, svc target.Service, serverID, name, compose string) Result {
	dir := BaseDir + "/" + name
	composePath := dir + "/" + ComposeFileName

	if res, err := svc.Exec(ctx, serverID, []string{"mkdir", "-p", dir}); err != nil {
		return Result{Err: humanSSHError(err)}
	} else if res.ExitCode != 0 {
		return Result{Err: "创建受管目录失败:" + truncate(strings.TrimSpace(res.Stderr), 256)}
	}
	if err := svc.Upload(ctx, serverID, strings.NewReader(compose), composePath); err != nil {
		return Result{Err: "写入 compose 文件失败:" + humanSSHError(err)}
	}
	bin := DetectComposeBin(ctx, svc, serverID)
	if bin == nil {
		return Result{Err: "该主机未检测到 docker compose / docker-compose,无法部署"}
	}
	upCmd := append(append([]string{}, bin...), "-p", name, "-f", composePath, "up", "-d")
	// up 幂等(原位更新),docker 会保留已下载的镜像分层,对瞬态拉取错误自动重试即断点续传。
	var (
		lastPullErr string
		dispatched  bool
	)
	for attempt := 0; attempt < maxUpAttempts; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return Result{Err: upInterruptedMsg, Dispatched: dispatched}
			case <-time.After(upRetryBackoffs[attempt-1]):
			}
		}
		res, err := svc.Exec(ctx, serverID, upCmd)
		if err != nil {
			// 请求上下文已取消(页面关闭/连接断开):up 被中断,但已下载的镜像分层保留,重试续传。
			if ctx.Err() != nil {
				return Result{Err: upInterruptedMsg, Dispatched: dispatched}
			}
			// SSH 层错误(主机不可达等)非瞬态,重试无意义。
			return Result{Err: humanSSHError(err), Dispatched: dispatched}
		}
		dispatched = true
		if res.ExitCode == 0 {
			out := strings.TrimSpace(res.Stdout) + "\n" + strings.TrimSpace(res.Stderr)
			if attempt > 0 {
				out = fmt.Sprintf("(镜像拉取曾在中途失败,自动重试 %d 次成功)\n", attempt) + out
			}
			return Result{OK: true, Out: truncate(out, 2048), Dispatched: true}
		}
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = "docker compose up 以非零状态退出"
		}
		if !rePullTransient.MatchString(msg) {
			// 非瞬态错误(compose 配置/守护进程拒绝等):重试无意义,立即失败。
			return Result{Err: truncateTail(msg, 1024), Dispatched: true}
		}
		lastPullErr = msg
	}
	// 瞬态拉取错误重试耗尽。
	return Result{
		Err: fmt.Sprintf("已自动重试 %d 次仍失败(镜像拉取遭遇瞬态网络中断):\n%s\n提示:检查目标机到镜像仓库的网络;可为 docker daemon 配置 registry-mirrors 加速,重试会续传已下载的分层。",
			maxUpAttempts-1, truncateTail(lastPullErr, 1024)),
		Dispatched: true,
	}
}

// InjectLimits 把 CPU/内存上限注入 compose 每个 service 的 deploy.resources.limits
// (docker compose v2 非 swarm 模式同样生效;模板已写同名键时被本次部署选择覆盖)。
// 两者皆空 → 原样返回(零开销);compose 非法 YAML → 错误,不部署。
func InjectLimits(compose, cpus, memory string) (string, error) {
	if cpus == "" && memory == "" {
		return compose, nil
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(compose), &doc); err != nil {
		return "", fmt.Errorf("解析 compose YAML: %w", err)
	}
	services, _ := doc["services"].(map[string]any)
	for _, raw := range services {
		svcMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		deploy, _ := svcMap["deploy"].(map[string]any)
		if deploy == nil {
			deploy = map[string]any{}
		}
		resources, _ := deploy["resources"].(map[string]any)
		if resources == nil {
			resources = map[string]any{}
		}
		limits, _ := resources["limits"].(map[string]any)
		if limits == nil {
			limits = map[string]any{}
		}
		if cpus != "" {
			limits["cpus"] = cpus
		}
		if memory != "" {
			limits["memory"] = memory
		}
		resources["limits"] = limits
		deploy["resources"] = resources
		svcMap["deploy"] = deploy
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("重编码 compose YAML: %w", err)
	}
	return string(out), nil
}

// ProjectContainerIDs 取某 compose 项目(按标签)的所有容器 ID(校验为纯十六进制,防注入)。
func ProjectContainerIDs(ctx context.Context, svc target.Service, serverID, project string) ([]string, error) {
	res, err := svc.Exec(ctx, serverID, []string{"docker", "ps", "-aq", "--filter", "label=com.docker.compose.project=" + project})
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, ln := range strings.Fields(res.Stdout) {
		if reContainerHexID.MatchString(ln) {
			ids = append(ids, ln)
		}
	}
	return ids, nil
}

// ProjectVolumeNames 取某 compose 项目(按标签)的具名卷名(过白名单正则,防异常输出注入)。
func ProjectVolumeNames(ctx context.Context, svc target.Service, serverID, project string) ([]string, error) {
	res, err := svc.Exec(ctx, serverID, []string{"docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project=" + project})
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, ln := range strings.Fields(res.Stdout) {
		if reName.MatchString(ln) && len(ln) <= 128 {
			names = append(names, ln)
		}
	}
	return names, nil
}
