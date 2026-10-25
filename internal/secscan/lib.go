package secscan

// 扫描器规则库(漏洞库/特征模板)的状态查询与手动更新。
//
// 三类实例的语义:
//   - trivy-server:显示「容器启动时间」(trivy 启动时加载最新漏洞库;长驻进程另有
//     24h 自刷新);手动更新 = docker restart 容器(重启即拉最新库,后台数分钟)。
//   - nuclei-runner:显示「模板目录最近变更时间」(nuclei 每次执行时 >24h 自动更新,
//     目录 mtime 即最近一次模板增删);手动更新 = docker exec 容器内 nuclei
//     -update-templates(同步等待,输出尾部作 note)。
//   - safeline:由雷池控制台自管理(自动升级 + 规则中心),本平台不做 → 明确拒绝。
//
// 依赖实例登记的 ServerID + StackName(应用市场部署联动自动带;手动登记的外部
// trivy-server 没有宿主信息 → 拒绝并提示在目标机自维护)。命令全部 argv 化(AC-SEC-02)。

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// libOpTimeout 是单次规则库操作(查询/更新)的预算;模板全量下载可达分钟级。
const libOpTimeout = 3 * time.Minute

// ScannerLibInfo 是一个实例的规则库状态(UpdatedAt 空 = 未知)。
type ScannerLibInfo struct {
	Kind      string `json:"kind"`
	UpdatedAt string `json:"updatedAt,omitempty"` // RFC3339(UTC)
	Note      string `json:"note,omitempty"`
}

// stackContainerPrefix 推导该栈内目标服务的容器名前缀(compose 命名 <栈>-<服务>-N)。
func stackContainerPrefix(stackName, service string) string {
	return stackName + "-" + service
}

// findStackContainer 在实例宿主机上按栈名+服务名定位运行中的容器名;未找到 → 人话错误。
func (s *service) findStackContainer(ctx context.Context, serverID, stackName, service string) (string, error) {
	res, err := s.runner.Exec(ctx, serverID, []string{
		"docker", "ps", "--filter", "name=" + stackContainerPrefix(stackName, service), "--format", "{{.Names}}"})
	if err != nil {
		return "", fmt.Errorf("连接宿主机失败:%v", err)
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("宿主机 docker 不可用:%s", tailClip(res.Stderr, 160))
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line, nil
		}
	}
	return "", fmt.Errorf("容器未在运行(栈 %s)", stackName)
}

// ScannerLibInfo 查询规则库状态(kind 分支见文件头)。
func (s *service) ScannerLibInfo(ctx context.Context, instanceID string) (*ScannerLibInfo, error) {
	inst, err := s.GetScanner(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if s.runner == nil {
		return nil, ErrRunnerUnavailable
	}
	switch inst.Kind {
	case KindTrivyServer:
		return s.trivyLibInfo(ctx, inst)
	case KindNucleiRunner:
		return s.nucleiLibInfo(ctx, inst)
	default:
		return nil, fmt.Errorf("%w: SafeLine 的规则与升级由其控制台自管理", ErrInvalidInput)
	}
}

// ScannerLibUpdate 手动触发规则库更新;返回更新后的状态(或触发结果的说明)。
func (s *service) ScannerLibUpdate(ctx context.Context, instanceID string) (*ScannerLibInfo, error) {
	inst, err := s.GetScanner(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if s.runner == nil {
		return nil, ErrRunnerUnavailable
	}
	octx, cancel := context.WithTimeout(ctx, libOpTimeout)
	defer cancel()
	switch inst.Kind {
	case KindTrivyServer:
		return s.trivyLibUpdate(octx, inst)
	case KindNucleiRunner:
		return s.nucleiLibUpdate(octx, inst)
	default:
		return nil, fmt.Errorf("%w: SafeLine 的规则与升级由其控制台自管理", ErrInvalidInput)
	}
}

// stackDeployed 校验实例具备宿主定位信息(应用市场联动登记自带)。
func stackDeployed(inst *Instance) error {
	if inst.ServerID == "" || inst.StackName == "" {
		return fmt.Errorf("%w:该实例未登记部署宿主(手动登记的外部实例请在目标机自维护)", ErrInvalidInput)
	}
	return nil
}

// trivyLibInfo:容器启动时间(启动即加载最新库,是"库新鲜度"的直观代理)。
func (s *service) trivyLibInfo(ctx context.Context, inst *Instance) (*ScannerLibInfo, error) {
	if err := stackDeployed(inst); err != nil {
		return nil, err
	}
	container, err := s.findStackContainer(ctx, inst.ServerID, inst.StackName, "trivy-server")
	if err != nil {
		return nil, err
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, []string{"docker", "inspect", "--format", "{{.State.StartedAt}}", container})
	if err != nil || res.ExitCode != 0 {
		return &ScannerLibInfo{Kind: inst.Kind}, nil // 查不到时间不算错:显示未知
	}
	started := strings.TrimSpace(res.Stdout)
	// docker 输出 RFC3339 Nano;归一为 RFC3339 展示。
	if t, perr := time.Parse(time.RFC3339Nano, started); perr == nil {
		started = t.UTC().Format(time.RFC3339)
	}
	return &ScannerLibInfo{Kind: inst.Kind, UpdatedAt: started, Note: "容器启动时间(启动时加载最新漏洞库;日常另有 24h 自动刷新)"}, nil
}

// trivyLibUpdate:重启容器触发立即拉最新库。
func (s *service) trivyLibUpdate(ctx context.Context, inst *Instance) (*ScannerLibInfo, error) {
	if err := stackDeployed(inst); err != nil {
		return nil, err
	}
	container, err := s.findStackContainer(ctx, inst.ServerID, inst.StackName, "trivy-server")
	if err != nil {
		return nil, err
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, []string{"docker", "restart", container})
	msg := ""
	if err != nil {
		msg = err.Error()
	} else if res != nil {
		msg = res.Stderr
	}
	if err != nil || res.ExitCode != 0 {
		return nil, fmt.Errorf("重启容器失败:%s", tailClip(msg, 200))
	}
	return &ScannerLibInfo{Kind: inst.Kind, Note: "已重启容器:启动即拉取最新漏洞库,视带宽数分钟内就绪"}, nil
}

// nucleiLibInfo:模板目录 mtime(最近一次模板增删的近似)。
func (s *service) nucleiLibInfo(ctx context.Context, inst *Instance) (*ScannerLibInfo, error) {
	if err := stackDeployed(inst); err != nil {
		return nil, err
	}
	container, err := s.findStackContainer(ctx, inst.ServerID, inst.StackName, "nuclei-runner")
	if err != nil {
		return nil, err
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, []string{
		"docker", "exec", container, "stat", "-c", "%Y", "/root/nuclei-templates"})
	if err != nil || res.ExitCode != 0 {
		return &ScannerLibInfo{Kind: inst.Kind}, nil // 未知不算错
	}
	epoch, perr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64)
	if perr != nil {
		return &ScannerLibInfo{Kind: inst.Kind}, nil
	}
	return &ScannerLibInfo{
		Kind:      inst.Kind,
		UpdatedAt: time.Unix(epoch, 0).UTC().Format(time.RFC3339),
		Note:      "模板目录最近变更时间(每次扫描距上次更新超 24h 自动更新)",
	}, nil
}

// nucleiLibUpdate:容器内执行 nuclei -update-templates(同步,分钟级)。
func (s *service) nucleiLibUpdate(ctx context.Context, inst *Instance) (*ScannerLibInfo, error) {
	if err := stackDeployed(inst); err != nil {
		return nil, err
	}
	container, err := s.findStackContainer(ctx, inst.ServerID, inst.StackName, "nuclei-runner")
	if err != nil {
		return nil, err
	}
	res, err := s.runner.Exec(ctx, inst.ServerID, []string{
		"docker", "exec", container, "nuclei", "-update-templates"})
	if err != nil {
		return nil, fmt.Errorf("执行模板更新失败:%v", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("模板更新失败:%s", tailClip(res.Stderr, 300))
	}
	info, ierr := s.nucleiLibInfo(ctx, inst)
	if ierr != nil || info == nil {
		info = &ScannerLibInfo{Kind: inst.Kind}
	}
	info.Note = "Nuclei 模板已更新" + outputTailNote(res.Stdout)
	return info, nil
}

// outputTailNote 把命令输出尾部并进 note(模板数量等有用信息;空输出 → 空)。
func outputTailNote(out string) string {
	t := tailClip(out, 120)
	if t == "" {
		return ""
	}
	return ":" + t
}
