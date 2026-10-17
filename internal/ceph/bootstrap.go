// bootstrap.go — cephadm 部署管线(后台任务 + 步骤/日志快照,srcupdate 同形)。
//
// 步骤:preflight(逐台预检:SSH/提权/容器运行时/systemd/python3/lvm2/设备可用/
// 时间同步告警)→ install(逐台安装 cephadm:发行版包优先,回退脚本下载)→
// bootstrap(首主机引导 mon/mgr + dashboard,回填 fsid,安装 ceph-common)→
// hosts(orch host add 其余主机并等待纳管)→ osds(逐盘 orch daemon add osd)→
// verify(轮询 ceph -s 至健康,记录 dashboard URL)。任一步失败即中止并置 failed。
package ceph

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangchengsir/pipewright/internal/target"
	"github.com/huangchengsir/pipewright/internal/vault"
)

// 部署步骤名(DeployStatus.Step 取值;前端按序展示)。
const (
	StepPreflight = "preflight"
	StepInstall   = "install"
	StepBootstrap = "bootstrap"
	StepHosts     = "hosts"
	StepOSDs      = "osds"
	StepVerify    = "verify"
)

// 各阶段超时:整条管线无总闸(各步独立限时,apt/引导为慢步骤单独放宽)。
const (
	aptTimeout     = 15 * time.Minute
	bootstrapTO    = 25 * time.Minute
	hostAddTO      = 5 * time.Minute
	osdAddTO       = 5 * time.Minute
	hostWaitTO     = 3 * time.Minute
	verifyTO       = 10 * time.Minute
	verifyInterval = 15 * time.Second
)

// maxDeployLogLines 是任务日志环形缓冲行数上限。
const maxDeployLogLines = 600

// deployJob 是一次部署任务的可变状态(快照与日志追加并发安全)。
type deployJob struct {
	mu      sync.Mutex
	running bool
	step    string
	message string
	done    bool
	errMsg  string
	log     []string
}

func (j *deployJob) set(step, msg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.step, j.message = step, msg
}

func (j *deployJob) appendLine(line string) {
	if line == "" {
		return
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	j.log = append(j.log, line)
	if len(j.log) > maxDeployLogLines {
		j.log = j.log[len(j.log)-maxDeployLogLines:]
	}
}

func (j *deployJob) finish(errMsg string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.running = false
	j.done = true
	j.errMsg = errMsg
}

// snapshot 返回进度快照(深拷贝日志,调用方可持有)。
func (j *deployJob) snapshot() DeployStatus {
	j.mu.Lock()
	defer j.mu.Unlock()
	st := DeployStatus{Running: j.running, Step: j.step, Message: j.message, Done: j.done, Error: j.errMsg}
	st.Log = append([]string(nil), j.log...)
	return st
}

// logf 向任务日志写一行(时间戳前缀)。
func (j *deployJob) logf(format string, args ...any) {
	j.appendLine(time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...))
}

// fsidRe 从 cephadm bootstrap 输出中提取 fsid。
var fsidRe = regexp.MustCompile(`fsid:?\s*([0-9a-fA-F-]{36})`)

// osdCreatedRe 从 orch daemon add osd 输出提取 osd 编号。
var osdCreatedRe = regexp.MustCompile(`osd\.osd\.(\d+)`)

// DeployStatus 返回部署任务快照;集群不存在返回 ErrNotFound,从未启动返回空快照。
func (s *service) DeployStatus(ctx context.Context, id string) (DeployStatus, error) {
	if _, err := s.getCluster(ctx, id); err != nil {
		return DeployStatus{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if j := s.jobs[id]; j != nil {
		return j.snapshot(), nil
	}
	return DeployStatus{}, nil
}

// StartDeploy 异步启动部署管线;已在跑返回 ErrBusy,状态置 deploying。
func (s *service) StartDeploy(ctx context.Context, id string) error {
	cl, err := s.getCluster(ctx, id)
	if err != nil {
		return err
	}
	hosts, err := s.listHostRows(ctx, id)
	if err != nil {
		return err
	}
	if len(hosts) == 0 {
		return fmt.Errorf("%w: 集群未登记任何主机", ErrInvalidInput)
	}

	s.mu.Lock()
	if j := s.jobs[id]; j != nil && j.snapshot().Running {
		s.mu.Unlock()
		return ErrBusy
	}
	j := &deployJob{running: true, step: StepPreflight, message: "预检目标主机…"}
	s.jobs[id] = j
	s.mu.Unlock()

	// dashboard 密码:自动生成,经 vault 保管(未配置 vault 时用临时口令,不落库不回显)。
	password := randPassword(20)
	if s.vault != nil && cl.DashboardCredentialID == "" {
		cred, err := s.vault.Create(vault.CreateInput{
			Name:   "ceph-" + cl.Name + "-dashboard",
			Type:   vault.TypeCephDashboard,
			Secret: password,
		})
		if err != nil {
			s.mu.Lock()
			s.jobs[id] = nil
			s.mu.Unlock()
			return fmt.Errorf("ceph: 保管 dashboard 密码失败: %w", err)
		}
		_ = s.updateClusterCredential(ctx, id, cred.ID)
		cl.DashboardCredentialID = cred.ID
	}

	if err := s.updateClusterState(ctx, id, StateDeploying); err != nil {
		return err
	}
	go s.runDeploy(j, cl, hosts, password)
	return nil
}

// runDeploy 是管线主体(后台 goroutine;bg 不随请求取消)。
func (s *service) runDeploy(j *deployJob, cl *Cluster, hosts []HostSpec, password string) {
	ctx := context.Background()
	err := s.deployPipeline(ctx, j, cl, hosts, password)
	if err != nil {
		j.logf("部署失败: %v", err)
		j.finish(err.Error())
		_ = s.updateClusterState(ctx, cl.ID, StateFailed)
		return
	}
	j.finish("")
	_ = s.updateClusterState(ctx, cl.ID, StateReady)
	s.mu.Lock()
	delete(s.stCache, cl.ID)
	s.mu.Unlock()
}

// deployPipeline 顺序执行各步骤;返回首个错误。
func (s *service) deployPipeline(ctx context.Context, j *deployJob, cl *Cluster, hosts []HostSpec, password string) error {
	info, err := s.stepPreflight(ctx, j, cl, hosts)
	if err != nil {
		return err
	}
	if err := s.stepInstall(ctx, j, cl, hosts); err != nil {
		return err
	}
	fsid, err := s.stepBootstrap(ctx, j, cl, info, password)
	if err != nil {
		return err
	}
	dashboardURL, err := s.stepHostsAndOSDs(ctx, j, cl, hosts, info)
	if err != nil {
		_ = s.updateClusterDeploy(ctx, cl.ID, fsid, "", StateFailed)
		return err
	}
	if err := s.stepVerify(ctx, j, cl); err != nil {
		return err
	}
	if dashboardURL == "" {
		dashboardURL = s.discoverDashboardURL(ctx, cl)
	}
	return s.updateClusterDeploy(ctx, cl.ID, fsid, dashboardURL, StateReady)
}

// deployHostInfo 是 preflight 阶段收集的每主机事实(hostname/addr/容器运行时)。
type deployHostInfo struct {
	hostname string
	addr     string
	runtime  string // docker | podman
}

// stepPreflight 逐台预检并回填 hostname/addr;返回主机事实表(serverID → info)。
func (s *service) stepPreflight(ctx context.Context, j *deployJob, cl *Cluster, hosts []HostSpec) (map[string]deployHostInfo, error) {
	j.set(StepPreflight, "预检目标主机…")
	j.logf("预检 %d 台主机(SSH 连通 / 提权 / 容器运行时 / systemd / python3 / 设备)", len(hosts))
	info := map[string]deployHostInfo{}
	seenHostname := map[string]string{}
	for _, h := range hosts {
		j.logf("── 主机 %s(server: %s)──", h.HostnameOr(cl), h.ServerID)
		sv, err := s.servers.Get(ctx, h.ServerID)
		if err != nil {
			return nil, fmt.Errorf("主机 %s 登记信息不可用: %w", h.ServerID, err)
		}
		hi := deployHostInfo{addr: sv.Host}

		// 提权:第一条经 runner 的命令即探测 root/免密 sudo,失败即 ErrNoPrivilege。
		res, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"id", "-u"})
		if err != nil {
			return nil, fmt.Errorf("主机 %s(%s) 提权检查失败: %w", sv.Name, sv.Host, err)
		}
		j.logf("提权 OK(uid=%s)", strings.TrimSpace(res.Stdout))

		if res, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"hostname"}); err == nil {
			hi.hostname = strings.TrimSpace(res.Stdout)
		}
		if hi.hostname == "" {
			return nil, fmt.Errorf("主机 %s 无法获取 hostname", sv.Name)
		}
		if prev, dup := seenHostname[hi.hostname]; dup {
			return nil, fmt.Errorf("%w: 主机名 %s 同时出现在 %s 与 %s(orch 以主机名唯一标识)", ErrInvalidInput, hi.hostname, prev, sv.Name)
		}
		seenHostname[hi.hostname] = sv.Name

		// 容器运行时:docker 或 podman 二选一。
		if _, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"docker", "--version"}); err == nil {
			hi.runtime = "docker"
		} else if _, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"podman", "--version"}); err == nil {
			hi.runtime = "podman"
		} else {
			return nil, fmt.Errorf("主机 %s 无 Docker/Podman:cephadm 需要容器运行时,请先安装(如 apt-get install -y docker.io)", sv.Name)
		}
		j.logf("容器运行时: %s", hi.runtime)

		if _, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"systemctl", "--version"}); err != nil {
			return nil, fmt.Errorf("主机 %s 无 systemd(cephadm 依赖): %w", sv.Name, err)
		}
		if _, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"python3", "--version"}); err != nil {
			return nil, fmt.Errorf("主机 %s 无 python3(cephadm 依赖): %w", sv.Name, err)
		}
		if len(h.Devices) > 0 {
			if _, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"lvs", "--version"}); err != nil {
				return nil, fmt.Errorf("主机 %s 无 lvm2(OSD 依赖): %w", sv.Name, err)
			}
			if err := s.checkDevices(ctx, j, h.ServerID, h.Devices); err != nil {
				return nil, err
			}
		}
		// 时间同步仅告警(NTP 未同步会导致 mon 选举异常,但不阻断)。
		if res, err := s.runHost(ctx, h.ServerID, cmdTimeout, []string{"timedatectl", "show", "-p", "NTPSynchronized", "--value"}); err == nil && strings.TrimSpace(res.Stdout) == "0" {
			j.logf("⚠ 时间同步未开启,建议启用 chrony/systemd-timesyncd")
		}

		_ = s.updateHostDeploy(ctx, cl.ID, h.ServerID, hi.hostname, hi.addr)
		_ = s.migrateOSDHost(ctx, cl.ID, h.ServerID, hi.hostname)
		info[h.ServerID] = hi
	}
	j.logf("预检通过")
	return info, nil
}

// HostnameOr 返回已知 hostname(重试部署时 DB 已回填),否则给 serverId 占位。
func (h HostSpec) HostnameOr(cl *Cluster) string {
	if h.Hostname != "" {
		return h.Hostname
	}
	if h.ServerID == cl.BootstrapServerID {
		return h.ServerID + "(bootstrap)"
	}
	return h.ServerID
}

// checkDevices 经 lsblk 校验设备存在、无文件系统、未挂载。
func (s *service) checkDevices(ctx context.Context, j *deployJob, serverID string, devices []string) error {
	res, err := s.runHost(ctx, serverID, cmdTimeout, []string{"lsblk", "-J", "-n", "-o", "PATH,TYPE,FSTYPE,MOUNTPOINT"})
	if err != nil {
		return fmt.Errorf("lsblk 失败: %w", err)
	}
	blocks := map[string]obj{}
	var walk func(v any)
	walk = func(v any) {
		if arr := asArr(v); arr != nil {
			for _, e := range arr {
				walk(e)
			}
			return
		}
		if o := asObj(v); o != nil {
			if p := pickStr(o, "path", "name"); p != "" {
				blocks[p] = o
			}
			walk(o["children"])
		}
	}
	if root := asObj(parseJSON(res.Stdout)); root != nil {
		walk(root["blockdevices"])
	}
	for _, d := range devices {
		o, ok := blocks[d]
		if !ok {
			return fmt.Errorf("%w: 设备 %s 不存在(lsblk 不可见)", ErrInvalidInput, d)
		}
		if fs := pickStr(o, "fstype"); fs != "" {
			return fmt.Errorf("%w: 设备 %s 已有文件系统(%s),Ceph OSD 需要空盘", ErrInvalidInput, d, fs)
		}
		if mnt := pickStr(o, "mountpoint"); mnt != "" {
			return fmt.Errorf("%w: 设备 %s 已挂载(%s)", ErrInvalidInput, d, mnt)
		}
		j.logf("设备 %s 可用", d)
	}
	return nil
}

// stepInstall 逐台安装 cephadm(发行版包优先;curl 来源直接落脚本)。
func (s *service) stepInstall(ctx context.Context, j *deployJob, cl *Cluster, hosts []HostSpec) error {
	j.set(StepInstall, "安装 cephadm…")
	for _, h := range hosts {
		j.logf("── 主机 %s:安装 cephadm ──", h.HostnameOr(cl))
		if err := s.installCephadm(ctx, j, cl, h.ServerID); err != nil {
			return err
		}
	}
	j.logf("cephadm 安装完成")
	return nil
}

// installCephadm 在单台主机安装 cephadm 并验证可执行。
func (s *service) installCephadm(ctx context.Context, j *deployJob, cl *Cluster, serverID string) error {
	source := cl.CephadmSource
	curlURL := cephadmCurlURL(source)

	if curlURL == "" {
		// auto/package:先探测发行版包管理器。
		if _, err := s.runHost(ctx, serverID, cmdTimeout, []string{"apt-get", "--version"}); err == nil {
			j.logf("apt-get update(可能需要几分钟)…")
			if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"apt-get", "update"}); err != nil {
				return fmt.Errorf("apt-get update 失败: %w", err)
			}
			if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"apt-get", "install", "-y", "cephadm"}); err != nil {
				return fmt.Errorf("apt 安装 cephadm 失败: %w", err)
			}
		} else if _, err := s.runHost(ctx, serverID, cmdTimeout, []string{"dnf", "--version"}); err == nil {
			if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"dnf", "install", "-y", "cephadm"}); err != nil {
				return fmt.Errorf("dnf 安装 cephadm 失败: %w", err)
			}
		} else if source == "auto" {
			// 无包管理器可用 → 回退官方脚本下载。
			if err := s.curlInstallCephadm(ctx, j, serverID, "https://download.ceph.com/cephadm/get"); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("主机未检出 apt-get/dnf,且安装来源为 package;请改用 auto 或 curl:<URL>")
		}
	} else if err := s.curlInstallCephadm(ctx, j, serverID, curlURL); err != nil {
		return err
	}

	res, err := s.runHost(ctx, serverID, cmdTimeout, []string{"cephadm", "--version"})
	if err != nil {
		return fmt.Errorf("cephadm 验证失败: %w", err)
	}
	j.logf("cephadm %s", strings.TrimSpace(res.Stdout))
	return nil
}

// curlInstallCephadm 下载 cephadm 脚本到 /usr/local/sbin(两条 argv,不拼 shell)。
func (s *service) curlInstallCephadm(ctx context.Context, j *deployJob, serverID, scriptURL string) error {
	j.logf("经 %s 下载 cephadm…", scriptURL)
	if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"curl", "-fsSL", "-o", "/tmp/pipewright-cephadm", scriptURL}); err != nil {
		return fmt.Errorf("下载 cephadm 失败: %w", err)
	}
	if _, err := s.runHost(ctx, serverID, cmdTimeout, []string{"chmod", "+x", "/tmp/pipewright-cephadm"}); err != nil {
		return fmt.Errorf("chmod cephadm 失败: %w", err)
	}
	if _, err := s.runHost(ctx, serverID, cmdTimeout, []string{"mv", "/tmp/pipewright-cephadm", "/usr/local/sbin/cephadm"}); err != nil {
		return fmt.Errorf("安装 cephadm 到 /usr/local/sbin 失败: %w", err)
	}
	return nil
}

// stepBootstrap 在 bootstrap 主机执行 cephadm bootstrap,返回 fsid。
func (s *service) stepBootstrap(ctx context.Context, j *deployJob, cl *Cluster, info map[string]deployHostInfo, password string) (string, error) {
	j.set(StepBootstrap, "引导集群(bootstrap mon/mgr/dashboard)…")
	boot := info[cl.BootstrapServerID]
	cmd := []string{"cephadm", "--image", cl.Image, "bootstrap",
		"--mon-ip", boot.addr,
		"--initial-dashboard-user", cl.DashboardUser,
		"--initial-dashboard-password", password,
		"--dashboard-password-noupdate",
	}
	if cl.ClusterNetwork != "" {
		cmd = append(cmd, "--cluster-network", cl.ClusterNetwork)
	}
	j.logf("cephadm bootstrap(mon-ip=%s)…预计 5-15 分钟", boot.addr)
	res, err := s.runHost(ctx, cl.BootstrapServerID, bootstrapTO, cmd)
	if err != nil {
		return "", fmt.Errorf("cephadm bootstrap 失败: %w", err)
	}
	out := res.Stdout + "\n" + res.Stderr
	fsid := ""
	if m := fsidRe.FindStringSubmatch(out); m != nil {
		fsid = m[1]
	}
	if fsid == "" {
		if res, err := s.runHost(ctx, cl.BootstrapServerID, cmdTimeout, []string{"awk", "/^fsid/{print $3}", "/etc/ceph/ceph.conf"}); err == nil {
			fsid = strings.TrimSpace(res.Stdout)
		}
	}
	if fsid == "" {
		return "", fmt.Errorf("无法解析集群 fsid(bootstrap 输出:\n%s)", tailString(out, 1500))
	}
	j.logf("集群 fsid: %s", fsid)

	// ceph-common 提供 ceph/rbd CLI(后续管理统一经本机)。
	if err := s.installCephCommon(ctx, j, cl.BootstrapServerID); err != nil {
		return "", err
	}
	res, err = s.runHost(ctx, cl.BootstrapServerID, cmdTimeout, []string{"ceph", "-v"})
	if err != nil {
		return "", fmt.Errorf("ceph CLI 验证失败: %w", err)
	}
	j.logf("ceph CLI 就绪(%s)", strings.TrimSpace(strings.ReplaceAll(res.Stdout, "\n", " ")))
	return fsid, nil
}

// installCephCommon 在指定主机安装 ceph-common(ceph/rbd CLI)。
func (s *service) installCephCommon(ctx context.Context, j *deployJob, serverID string) error {
	j.logf("安装 ceph-common(ceph/rbd CLI)…")
	if _, err := s.runHost(ctx, serverID, cmdTimeout, []string{"apt-get", "--version"}); err == nil {
		if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"apt-get", "install", "-y", "ceph-common"}); err != nil {
			return fmt.Errorf("apt 安装 ceph-common 失败: %w", err)
		}
		return nil
	}
	if _, err := s.runHost(ctx, serverID, aptTimeout, []string{"dnf", "install", "-y", "ceph-common"}); err != nil {
		return fmt.Errorf("dnf 安装 ceph-common 失败: %w", err)
	}
	return nil
}

// stepHostsAndOSDs 纳管其余主机并逐盘添加 OSD;返回 dashboard URL(尽力而为)。
func (s *service) stepHostsAndOSDs(ctx context.Context, j *deployJob, cl *Cluster, hosts []HostSpec, info map[string]deployHostInfo) (string, error) {
	rest := hosts[:0:0]
	for _, h := range hosts {
		if h.ServerID != cl.BootstrapServerID {
			rest = append(rest, h)
		}
	}
	if len(rest) > 0 {
		j.set(StepHosts, fmt.Sprintf("纳管 %d 台主机…", len(rest)))
	}
	for _, h := range rest {
		hi := info[h.ServerID]
		j.logf("ceph orch host add %s %s %s", hi.hostname, hi.addr, strings.Join(h.Labels, ","))
		cmd := []string{"ceph", "orch", "host", "add", hi.hostname, hi.addr}
		cmd = append(cmd, h.Labels...)
		if _, err := s.runHost(ctx, cl.BootstrapServerID, hostAddTO, cmd); err != nil {
			return "", fmt.Errorf("纳管主机 %s 失败: %w", hi.hostname, err)
		}
	}
	if len(rest) > 0 {
		if err := s.waitHostsInOrch(ctx, j, cl, rest, info); err != nil {
			return "", err
		}
	}

	j.set(StepOSDs, "添加 OSD…")
	for _, h := range hosts {
		hi := info[h.ServerID]
		for _, dev := range h.Devices {
			j.logf("ceph orch daemon add osd %s:%s", hi.hostname, dev)
			_ = s.upsertOSDRow(ctx, cl.ID, hi.hostname, dev)
			res, err := s.runHost(ctx, cl.BootstrapServerID, osdAddTO,
				[]string{"ceph", "orch", "daemon", "add", "osd", hi.hostname + ":" + dev})
			if err != nil {
				_ = s.setOSDResult(ctx, cl.ID, hi.hostname, dev, "failed", -1)
				return "", fmt.Errorf("添加 OSD %s:%s 失败: %w", hi.hostname, dev, err)
			}
			osdID := int64(-1)
			if m := osdCreatedRe.FindStringSubmatch(res.Stdout + res.Stderr); m != nil {
				osdID, _ = strconv.ParseInt(m[1], 10, 64)
			}
			_ = s.setOSDResult(ctx, cl.ID, hi.hostname, dev, "ok", osdID)
			j.logf("OSD %s:%s 已创建(osd.%d)", hi.hostname, dev, osdID)
		}
	}
	return s.discoverDashboardURL(ctx, cl), nil
}

// waitHostsInOrch 轮询 orch host ls 直到全部纳管可见。
func (s *service) waitHostsInOrch(ctx context.Context, j *deployJob, cl *Cluster, rest []HostSpec, info map[string]deployHostInfo) error {
	deadline := time.Now().Add(hostWaitTO)
	want := map[string]bool{}
	for _, h := range rest {
		want[info[h.ServerID].hostname] = true
	}
	for {
		res, err := s.runHost(ctx, cl.BootstrapServerID, cmdTimeout, []string{"ceph", "orch", "host", "ls", "-f", "json"})
		if err == nil {
			present := map[string]bool{}
			for _, oh := range parseOrchHosts(res.Stdout) {
				present[oh.Host] = true
			}
			all := true
			for h := range want {
				if !present[h] {
					all = false
					break
				}
			}
			if all {
				j.logf("全部主机已纳管")
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("等待主机纳管超时(%s)", hostWaitTO)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyInterval):
		}
	}
}

// stepVerify 轮询 ceph -s 至健康(OK 即过;WARN 5 分钟后放行;ERR 失败)。
func (s *service) stepVerify(ctx context.Context, j *deployJob, cl *Cluster) error {
	j.set(StepVerify, "等待集群健康…")
	deadline := time.Now().Add(verifyTO)
	warnAt := time.Now().Add(5 * time.Minute)
	for {
		res, err := s.runHost(ctx, cl.BootstrapServerID, cmdTimeout, []string{"ceph", "-s", "-f", "json"})
		if err == nil {
			st := parseClusterStatus(res.Stdout)
			switch st.Health {
			case "HEALTH_OK":
				j.logf("集群健康:HEALTH_OK(osd %d/%d up)", st.OsdUp, st.OsdIn)
				return nil
			case "HEALTH_ERR":
				return fmt.Errorf("集群健康检查失败(HEALTH_ERR):%s", strings.Join(st.HealthDetail, "; "))
			case "HEALTH_WARN":
				if time.Now().After(warnAt) {
					j.logf("⚠ 集群 HEALTH_WARN(常见于 PG 重建中),继续交付:%s", strings.Join(st.HealthDetail, "; "))
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			j.logf("⚠ 健康检查等待超时(集群仍在收敛,状态页可持续观察)")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(verifyInterval):
		}
	}
}

// discoverDashboardURL 从 ceph mgr services 读 dashboard 地址(尽力而为,失败返回空)。
func (s *service) discoverDashboardURL(ctx context.Context, cl *Cluster) string {
	res, err := s.runHost(ctx, cl.BootstrapServerID, cmdTimeout, []string{"ceph", "mgr", "services", "-f", "json"})
	if err != nil {
		return ""
	}
	if o := asObj(parseJSON(res.Stdout)); o != nil {
		return pickStr(o, "dashboard")
	}
	return ""
}

// runHost 在指定主机执行命令(带独立超时;失败行写任务日志便于排障)。
func (s *service) runHost(ctx context.Context, serverID string, timeout time.Duration, cmd []string) (*target.ExecResult, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return s.runner.Run(ctx, serverID, cmd)
}

// tailString 截取字符串尾部(≤n 字符)。
func tailString(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}
