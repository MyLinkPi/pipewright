package opschat

import (
	"encoding/json"
	"github.com/huangchengsir/pipewright/internal/target"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Tool struct {
	ID       string          `json:"toolId"`
	Mutation bool            `json:"mutation"`
	Schema   json.RawMessage `json:"schema"`
}

var tools = []Tool{
	{"host_resources", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"host_processes", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"host_ports", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"docker_containers", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"docker_stats", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"docker_images", false, json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)},
	{"docker_inspect", false, json.RawMessage(`{"type":"object","properties":{"container":{"type":"string","maxLength":256}},"required":["container"],"additionalProperties":false}`)},
	{"docker_logs", false, json.RawMessage(`{"type":"object","properties":{"container":{"type":"string","maxLength":256},"lines":{"type":"integer","minimum":1,"maximum":1000,"default":200}},"required":["container"],"additionalProperties":false}`)},
	{"systemd_status", false, json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string","maxLength":256}},"required":["unit"],"additionalProperties":false}`)},
	{"systemd_logs", false, json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string","maxLength":256},"lines":{"type":"integer","minimum":1,"maximum":1000,"default":200}},"required":["unit"],"additionalProperties":false}`)},
	{"docker_action", true, json.RawMessage(`{"type":"object","properties":{"container":{"type":"string","maxLength":256},"action":{"type":"string","enum":["start","stop","restart","pause","unpause"]}},"required":["container","action"],"additionalProperties":false}`)},
	{"host_exec", true, json.RawMessage(`{"type":"object","properties":{"argv":{"type":"array","minItems":1,"maxItems":61,"items":{"type":"string","maxLength":2048}}},"required":["argv"],"additionalProperties":false}`)},
	{"docker_exec", true, json.RawMessage(`{"type":"object","properties":{"container":{"type":"string","maxLength":256},"argv":{"type":"array","minItems":1,"maxItems":61,"items":{"type":"string","maxLength":2048}}},"required":["container","argv"],"additionalProperties":false}`)},
	{"systemd_action", true, json.RawMessage(`{"type":"object","properties":{"unit":{"type":"string","maxLength":256},"action":{"type":"string","enum":["start","stop","restart"]}},"required":["unit","action"],"additionalProperties":false}`)},
}

func Tools() []Tool {
	out := make([]Tool, len(tools))
	copy(out, tools)
	for i := range out {
		out[i].Schema = append(json.RawMessage(nil), out[i].Schema...)
	}
	return out
}
func mutation(id string) bool {
	return id == "docker_action" || id == "systemd_action" || id == "docker_exec" || id == "host_exec"
}

// readOnlyPrograms 是可免确认自动放行的只读命令集合(按裸命令名匹配)。
// 收录标准:该程序不存在任何写文件/改状态/再执行别的程序的能力。
// 刻意排除:解释器与包装器(sh/bash/python/env/timeout/xargs/nohup/find 等,
// 它们能把后续 token 当命令执行)、带写副作用的变体(journalctl --vacuum-*、
// systemct、ip、dmesg -C、nvidia-smi -r、sort -o、tee、dd、hostname 改名)。
var readOnlyPrograms = map[string]bool{
	"cat": true, "date": true, "df": true, "du": true, "echo": true,
	"file": true, "findmnt": true, "free": true, "grep": true, "head": true,
	"id": true, "ls": true, "lscpu": true, "lsblk": true, "lsof": true,
	"md5sum": true, "nproc": true, "ps": true, "readlink": true, "realpath": true,
	"sha256sum": true, "ss": true, "stat": true, "tail": true, "uname": true,
	"uptime": true, "vmstat": true, "w": true, "wc": true, "who": true, "whoami": true,
}

// readOnlyExecToken 限定白名单命令每个 token 的字符集。任何 shell 元字符
// (; | & ` $ < > 引号、反斜杠、空白、控制字符)出现即整体 fail-closed 回退
// 确认,防止"白名单程序 + 拼接元字符"绕过(执行层虽有单引号转义,这里仍不
// 依赖它)。
var readOnlyExecToken = regexp.MustCompile(`^[A-Za-z0-9_./,=+:%@*?-]+$`)

// readOnlyExecArgv 判定 argv 是否为一条可自动放行的只读命令:
//  1. 1..16 个 token,总字节数 ≤4096(与 validExecArgv 对齐);
//  2. argv[0] 必须是不含 / 的裸命令名且在白名单内(拒绝 /tmp/evil/du 路径伪装,
//     也拒绝 /usr/bin/du 绝对路径——一律走确认);
//  3. 每个 token(含 argv[0])字符集受限;date 另拒绝 -s/--set(改系统时钟)。
func readOnlyExecArgv(argv []string) bool {
	if len(argv) < 1 || len(argv) > 16 {
		return false
	}
	total := 0
	for _, token := range argv {
		if len(token) > 2048 || !readOnlyExecToken.MatchString(token) {
			return false
		}
		total += len(token)
	}
	if total > 4096 {
		return false
	}
	if strings.Contains(argv[0], "/") || !readOnlyPrograms[argv[0]] {
		return false
	}
	if argv[0] == "date" {
		for _, token := range argv[1:] {
			// 拒绝一切含 s 的选项:GNU getopt 允许 -s@0、-us 等粘合短选项,
			// 只有 set 时钟的选项带 s;误伤(如 --iso)只会回退确认。
			if strings.HasPrefix(token, "-") && strings.Contains(token, "s") {
				return false
			}
		}
	}
	return true
}

// needsApproval 报告一个调用是否仍需用户确认:变更类工具中,host_exec /
// docker_exec 的 argv 命中只读白名单时免确认,其余(含一切非 exec 变更)
// 照旧走确认卡。判定只依赖入库的 Args,各门控点重算保持一致。
func needsApproval(v Call) bool {
	if !mutation(v.ToolID) {
		return false
	}
	if (v.ToolID == "host_exec" || v.ToolID == "docker_exec") && readOnlyExecArgv(v.Args.Argv) {
		return false
	}
	return true
}
func parseArgs(id string, raw json.RawMessage) (ToolArgs, error) {
	var a ToolArgs
	if !utf8.Valid(raw) {
		return a, ErrInvalid
	}
	var supplied map[string]json.RawMessage
	if id == "docker_exec" || id == "host_exec" {
		// A 4096-byte decoded argv can expand sixfold under JSON HTML escaping.
		if err := strictWithLimit(raw, &supplied, 32<<10); err != nil {
			return a, err
		}
		var tokens []json.RawMessage
		if json.Unmarshal(supplied["argv"], &tokens) != nil || len(tokens) < 1 || len(tokens) > 61 {
			return a, ErrInvalid
		}
		for _, token := range tokens {
			if !validJSONStringUnicode(token) {
				return a, ErrInvalid
			}
		}
		if err := strictWithLimit(raw, &a, 32<<10); err != nil {
			return a, err
		}
	} else {
		if err := strict(raw, &a); err != nil {
			return a, err
		}
		if len(raw) > 0 {
			_ = json.Unmarshal(raw, &supplied)
		}
	}
	allowed := map[string]bool{}
	found := false
	for _, t := range tools {
		if t.ID == id {
			found = true
		}
	}
	if !found {
		return a, ErrInvalid
	}
	switch id {
	case "docker_inspect", "docker_logs", "docker_action", "docker_exec":
		allowed["container"] = true
		if target.ValidateServiceParams("docker", a.Container, "start") != nil {
			return a, ErrInvalid
		}
	case "systemd_status", "systemd_logs", "systemd_action":
		allowed["unit"] = true
		if target.ValidateServiceParams("systemd", a.Unit, "start") != nil {
			return a, ErrInvalid
		}
	}
	if id == "docker_logs" || id == "systemd_logs" {
		allowed["lines"] = true
		if a.Lines == 0 {
			if _, ok := supplied["lines"]; ok {
				return a, ErrInvalid
			}
			a.Lines = 200
		}
		if a.Lines < 1 || a.Lines > 1000 {
			return a, ErrInvalid
		}
	}
	if id == "docker_exec" || id == "host_exec" {
		allowed["argv"] = true
		if !validExecArgv(a.Argv) {
			return a, ErrInvalid
		}
	}
	if id == "docker_action" || id == "systemd_action" {
		allowed["action"] = true
		typ, obj := "docker", a.Container
		if id == "systemd_action" {
			typ, obj = "systemd", a.Unit
		}
		if target.ValidateOpsAction(typ, obj, a.Action) != nil {
			return a, ErrInvalid
		}
	}
	for k := range supplied {
		if !allowed[k] {
			return a, ErrInvalid
		}
	}
	return a, nil
}

// encoding/json replaces unpaired surrogate escapes. Check already validated JSON
// string tokens before decoding values; escaped backslashes remain literal text.
func validJSONStringUnicode(token json.RawMessage) bool {
	if len(token) < 2 || token[0] != '"' || token[len(token)-1] != '"' {
		return false
	}
	for i := 1; i < len(token)-1; {
		if token[i] != '\\' {
			i++
			continue
		}
		i++
		if i >= len(token)-1 {
			return false
		}
		if token[i] != 'u' {
			i++
			continue
		}
		if i+5 > len(token)-1 {
			return false
		}
		code, err := strconv.ParseUint(string(token[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 5
		if code >= 0xd800 && code <= 0xdbff {
			if i+6 > len(token)-1 || token[i] != '\\' || token[i+1] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(token[i+2:i+6]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		} else if code >= 0xdc00 && code <= 0xdfff {
			return false
		}
	}
	return true
}

var immutableContainer = regexp.MustCompile(`^[a-f0-9]{64}$`)
var execProgram = regexp.MustCompile(`^[A-Za-z0-9_./][A-Za-z0-9_./+-]*$`)

func validExecArgv(argv []string) bool {
	// Reserve three tokens for docker, exec, and the frozen container ID.
	if len(argv) < 1 || len(argv) > 61 || !execProgram.MatchString(argv[0]) {
		return false
	}
	if base := path.Base(argv[0]); base == "." || base == ".." || base == "/" || argv[0][len(argv[0])-1] == '/' {
		return false
	}
	total := 0
	for _, arg := range argv {
		if !utf8.ValidString(arg) || len(arg) > 2048 {
			return false
		}
		total += len(arg)
		if total > 4096 {
			return false
		}
		for _, r := range arg {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

const inspectProjection = `{"id":{{json .Id}},"state":{{json .State}},"image":{{json .Image}},"ports":{{json .NetworkSettings.Ports}},"mounts":{{json .Mounts}}}`

func commands(id string, a ToolArgs) [][]string {
	switch id {
	case "host_resources":
		return [][]string{{"env", "LC_ALL=C", "df", "-P", "-k"}, {"env", "LC_ALL=C", "free", "-b"}, {"env", "LC_ALL=C", "uptime"}}
	case "host_processes":
		return [][]string{{"ps", "-eo", "pid,comm,pcpu,pmem", "--sort=-pcpu"}}
	case "host_ports":
		return [][]string{{"ss", "-lntu"}}
	case "docker_containers":
		return [][]string{{"docker", "ps", "-a", "--no-trunc", "--format", `{"id":{{json .ID}},"names":{{json .Names}},"image":{{json .Image}},"state":{{json .State}},"ports":{{json .Ports}}}`}}
	case "docker_stats":
		return [][]string{{"docker", "stats", "--no-stream", "--format", `{"id":{{json .ID}},"name":{{json .Name}},"cpu":{{json .CPUPerc}},"memory":{{json .MemUsage}},"network":{{json .NetIO}},"block":{{json .BlockIO}}}`}}
	case "docker_images":
		return [][]string{{"docker", "images", "--no-trunc", "--format", `{"id":{{json .ID}},"repository":{{json .Repository}},"tag":{{json .Tag}},"size":{{json .Size}}}`}}
	case "docker_inspect":
		return [][]string{{"docker", "inspect", "--type", "container", "--format", inspectProjection, a.Container}}
	case "docker_logs":
		return [][]string{{"docker", "logs", "--tail", strconv.Itoa(a.Lines), "--timestamps", a.Container}}
	case "systemd_status":
		return [][]string{{"systemctl", "show", a.Unit, "--property=Id,ActiveState,SubState,LoadState,MainPID"}}
	case "systemd_logs":
		return [][]string{{"journalctl", "-u", a.Unit, "-n", strconv.Itoa(a.Lines), "--no-pager", "-o", "short-iso"}}
	case "docker_action":
		return [][]string{target.BuildServiceCmd("docker", a.Action, a.Container)}
	case "docker_exec":
		if !immutableContainer.MatchString(a.Container) || !validExecArgv(a.Argv) {
			return nil
		}
		return [][]string{append([]string{"docker", "exec", a.Container}, a.Argv...)}
	case "host_exec":
		if !validExecArgv(a.Argv) {
			return nil
		}
		return [][]string{a.Argv}
	case "systemd_action":
		return [][]string{target.BuildServiceCmd("systemd", a.Action, a.Unit)}
	}
	return nil
}
