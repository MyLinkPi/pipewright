package deploy

// ports.go 实现容器宿主端口的「范围探测自动分配」:
//
//   - cfg["ports"] 三种形态:显式映射 "8080:80"(宿主:容器)、仅容器端口 "8080"、显式自动
//     "auto:8080" —— 后两者宿主端口在目标机自动分配;不配置 ports = 不发布宿主端口。
//   - cfg["autoPortRange"] 自动分配端口段(默认 "20000-30000";格式 lo-hi)。
//   - 分配 = 在目标机用 bash /dev/tcp 逐候选探测(连接成功 = 端口占用,非零 = 空闲),取首个
//     空闲;docker run 仍是最终仲裁 —— 报 "port is already allocated" 时换下一候选重试
//     (探测与 docker 判定之间的竞态、bash 缺失(127≠0 误判空闲)都由此兜底)。
//
// 安全不变量:探测命令的端口是本层生成的整数(非用户输入),bash -c 脚本无注入面;
// 其余 docker 命令保持 array 化。

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 自动分配端口段的默认值与上限。
const (
	defaultAutoPortLo = 20000
	defaultAutoPortHi = 30000
	// maxAutoPortSpan 是端口段最大跨度(防误配 1-65535 导致极端场景下逐个探测过万端口)。
	maxAutoPortSpan = 10000
	// portAllocRetries 是「docker 报端口占用 → 换候选重试」的最大轮数。
	portAllocRetries = 3
)

// portSpec 是一条端口映射意图(cfg["ports"] 的一项)。
type portSpec struct {
	host      int // 显式宿主端口;0 = 自动分配
	container int // 容器端口(= 服务端口,健康检查 / 注册实例用)
}

// portBinding 是已解析的端口映射(spec + 生效宿主端口)。
type portBinding struct {
	spec portSpec
	host int // 生效宿主端口(显式或分配结果)
}

// hostPort 返回首个映射的宿主端口(健康检查自动推导用);无映射 → 0。
func firstHostPort(bindings []portBinding) int {
	if len(bindings) == 0 {
		return 0
	}
	return bindings[0].host
}

// firstContainerPort 返回首个映射的容器端口(注册实例的 service port 推导用);无映射 → 0。
func firstContainerPort(specs []portSpec) int {
	if len(specs) == 0 {
		return 0
	}
	return specs[0].container
}

// parsePortSpecs 解析 cfg["ports"]:逗号 / 空白分隔;每项:
//
//	"8080:80"  → 宿主 8080 → 容器 80(显式,兼容既有)
//	"8080"     → 容器 8080,宿主自动分配
//	"auto:8080"/"0:8080" → 同上的显式写法
//
// 空串 → 空 slice(不发布宿主端口);非法项 → 人读错误。
func parsePortSpecs(raw string) ([]portSpec, error) {
	items := splitImageList(raw)
	if len(items) == 0 {
		return nil, nil
	}
	specs := make([]portSpec, 0, len(items))
	for _, item := range items {
		spec, err := parsePortSpec(item)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// parsePortSpec 解析单条端口映射(格式见 parsePortSpecs)。
func parsePortSpec(item string) (portSpec, error) {
	item = strings.ToLower(strings.TrimSpace(item))
	auto := false
	hostPart, containerPart := "", item
	if i := strings.IndexByte(item, ':'); i >= 0 {
		hostPart, containerPart = item[:i], item[i+1:]
	}
	if hostPart == "auto" || hostPart == "0" {
		auto = true
		hostPart = ""
	}
	container, err := parsePortNumber(containerPart)
	if err != nil {
		return portSpec{}, fmt.Errorf("端口映射 %q 非法(期望 宿主:容器 / 容器端口 / auto:容器端口):%v", item, err)
	}
	if auto {
		return portSpec{host: 0, container: container}, nil
	}
	if hostPart == "" {
		return portSpec{host: 0, container: container}, nil
	}
	host, err := parsePortNumber(hostPart)
	if err != nil {
		return portSpec{}, fmt.Errorf("端口映射 %q 的宿主端口非法:%v", item, err)
	}
	return portSpec{host: host, container: container}, nil
}

// parsePortNumber 解析 1..65535 端口(空串/越界/非数字 → 人读错误)。
func parsePortNumber(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("端口为空")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("端口 %q 不在 1..65535", s)
	}
	return n, nil
}

// autoPortBounds 解析 cfg["autoPortRange"]("lo-hi");缺省 / 非法 → 默认段。
// hi<=lo 或跨度超限 → 夹紧。
func autoPortBounds(cfg map[string]string) (int, int) {
	lo, hi := defaultAutoPortLo, defaultAutoPortHi
	raw := strings.TrimSpace(cfg["autoPortRange"])
	if i := strings.IndexByte(raw, '-'); i > 0 {
		if v, err := strconv.Atoi(strings.TrimSpace(raw[:i])); err == nil && v >= 1 && v <= 65535 {
			lo = v
		}
		if v, err := strconv.Atoi(strings.TrimSpace(raw[i+1:])); err == nil && v >= 1 && v <= 65535 {
			hi = v
		}
	}
	if hi <= lo {
		lo, hi = defaultAutoPortLo, defaultAutoPortHi
	}
	if hi-lo > maxAutoPortSpan {
		hi = lo + maxAutoPortSpan
	}
	return lo, hi
}

// portFlagArgs 把绑定展开为 docker run 的 -p 参数序列(如 -p 20001:8080)。
func portFlagArgs(bindings []portBinding) []string {
	args := make([]string, 0, len(bindings)*2)
	for _, b := range bindings {
		args = append(args, "-p", strconv.Itoa(b.host)+":"+strconv.Itoa(b.spec.container))
	}
	return args
}

// portBindingSummary 返回人读端口摘要(如 "20001:8080, 9000:9000");空 → ""。
func portBindingSummary(bindings []portBinding) string {
	parts := make([]string, 0, len(bindings))
	for _, b := range bindings {
		parts = append(parts, strconv.Itoa(b.host)+":"+strconv.Itoa(b.spec.container))
	}
	return strings.Join(parts, ",")
}

// resolvePortBindings 解析全部 spec 为绑定:显式项原样;自动项经 allocFreePort 探测分配
// (avoid 内端口跳过 —— 重试轮把已失败候选排除)。全段无空闲 → 人读错误。
func (s *service) resolvePortBindings(ctx context.Context, serverID string, cfg map[string]string, specs []portSpec, avoid map[int]bool) ([]portBinding, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	lo, hi := autoPortBounds(cfg)
	used := make(map[int]bool, len(specs))
	for _, sp := range specs {
		if sp.host > 0 {
			used[sp.host] = true
		}
	}
	bindings := make([]portBinding, 0, len(specs))
	for _, sp := range specs {
		b := portBinding{spec: sp}
		switch {
		case sp.host > 0:
			b.host = sp.host
		default:
			skip := make(map[int]bool, len(used)+len(avoid))
			for k, v := range used {
				skip[k] = v
			}
			for k, v := range avoid {
				skip[k] = v
			}
			p, err := s.allocFreePort(ctx, serverID, lo, hi, skip)
			if err != nil {
				return nil, err
			}
			b.host = p
			used[p] = true
		}
		bindings = append(bindings, b)
	}
	return bindings, nil
}

// allocFreePort 在目标机 lo..hi 内探测首个空闲宿主端口(跳过 skip)。
// 探测:`bash -c "exec 3<>/dev/tcp/127.0.0.1/<p>"` —— 退出码 0 = 端口被占,非 0 = 空闲。
// 端口为本层生成的整数,bash 脚本无注入面;bash 缺失(127≠0 误判空闲)由 docker run 报错重试兜底。
func (s *service) allocFreePort(ctx context.Context, serverID string, lo, hi int, skip map[int]bool) (int, error) {
	for p := lo; p <= hi; p++ {
		if skip[p] {
			continue
		}
		out, err := s.exec(ctx, serverID, []string{
			"bash", "-c", fmt.Sprintf("exec 3<>/dev/tcp/127.0.0.1/%d && exit 0 || exit 1", p),
		})
		if err != nil {
			// 传输层失败:无法探测,直接给候选交 docker 仲裁。
			return p, nil
		}
		if out != nil && out.ExitCode == 0 {
			continue // 连接成功 = 占用
		}
		return p, nil // 连接失败 = 空闲
	}
	return 0, fmt.Errorf("自动端口段 %d-%d 内未探测到空闲端口(全部被占用或段过小)", lo, hi)
}

// isPortAllocError 报告 docker 输出是否为宿主端口占用(换候选重试的判定)。
func isPortAllocError(out *target.ExecResult) bool {
	if out == nil {
		return false
	}
	msg := strings.ToLower(out.Stderr + out.Stdout)
	return strings.Contains(msg, "port is already allocated") ||
		strings.Contains(msg, "address already in use") ||
		(strings.Contains(msg, "bind:") && strings.Contains(msg, "failed"))
}

// runContainerWithPorts 起容器并兜底端口竞态:每轮 resolvePortBindings(避开上一轮候选)→
// docker run;失败且为端口占用 → 换候选重试(至多 portAllocRetries 轮)。成功返回实际绑定
// (回显日志「端口映射 → host:container」);非端口类失败返回该轮的人读 message。
//
// 命令保持 array 化(-p 独立元素);分配结果写部署日志供运维核对。
func (s *service) runContainerWithPorts(ctx context.Context, serverID, name string, baseArgs []string, specs []portSpec, cfg map[string]string, ref string) ([]portBinding, string, bool) {
	if len(specs) == 0 {
		// 无端口映射:与既有 docker run 完全一致,零探测。
		if failMsg, ok := s.runStep(ctx, serverID, [][]string{dockerRunCmd(name, baseArgs, ref)}); !ok {
			return nil, failMsg, false
		} else {
			return nil, "", true
		}
	}
	tried := make(map[int]bool)
	hasAuto := false
	for _, sp := range specs {
		if sp.host == 0 {
			hasAuto = true
		}
	}
	var lastMsg string
	for round := 0; round <= portAllocRetries; round++ {
		bindings, err := s.resolvePortBindings(ctx, serverID, cfg, specs, tried)
		if err != nil {
			return nil, err.Error(), false
		}
		args := append(portFlagArgs(bindings), baseArgs...)
		out, eerr := s.exec(ctx, serverID, dockerRunCmd(name, args, ref))
		if eerr != nil {
			return nil, humanExecError(eerr), false
		}
		if out != nil && out.ExitCode != 0 {
			lastMsg = fmt.Sprintf("docker run 退出码 %d:%s", out.ExitCode, truncate(strings.TrimSpace(out.Stderr)))
			// 仅自动项存在时换候选重试才有意义(显式映射是用户指定的固定口,重试必然同因)。
			if isPortAllocError(out) && hasAuto && round < portAllocRetries {
				for _, b := range bindings {
					if b.spec.host == 0 {
						tried[b.host] = true
					}
				}
				continue
			}
			return nil, lastMsg, false
		}
		lg := cmdLogFrom(ctx)
		lg(cmdStreamStdout, "", "  → 容器 "+name+" 端口映射:"+portBindingSummary(bindings))
		return bindings, "", true
	}
	return nil, lastMsg, false
}
