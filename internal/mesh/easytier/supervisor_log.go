// 本文件负责把 easytier-core 的完整命令行与子进程 stdout/stderr 透传到平台日志
// (pwclient log / 控制台日志),密钥与凭据在落日志前打码。

package easytier

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
)

// maskArgFlags 是值需打码的敏感旗子(小写比较):组网密钥、临时凭据。
var maskArgFlags = map[string]bool{
	"--network-secret": true,
	"--credential":     true,
}

const maskPlaceholder = "***"

// sensitive 返回配置中不得明文落日志的敏感值(密钥/凭据)。
func (c Config) sensitive() []string {
	var out []string
	for _, s := range []string{c.NetworkSecret, c.Credential} {
		if len(s) >= 4 { // 过短值参与全文替换易误伤普通输出
			out = append(out, s)
		}
	}
	return out
}

// maskArgs 生成打码后的命令行(逐参数,含二进制路径):敏感旗子的值替换为 ***;
// 同时把值串内残留的敏感明文(如凭据内嵌于其他参数值)一并替换。
func maskArgs(cfg Config, args []string) []string {
	secrets := cfg.sensitive()
	out := make([]string, 0, len(args))
	maskNext := false
	for _, a := range args {
		if maskNext {
			out = append(out, maskPlaceholder)
			maskNext = false
			continue
		}
		if name, _, ok := strings.Cut(a, "="); ok && maskArgFlags[strings.ToLower(name)] {
			out = append(out, name+"="+maskPlaceholder)
			continue
		}
		if maskArgFlags[strings.ToLower(a)] {
			out = append(out, a)
			maskNext = true
			continue
		}
		out = append(out, maskValue(a, secrets))
	}
	return out
}

// maskValue 把字符串里出现的敏感明文整体替换为 ***。
func maskValue(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, maskPlaceholder)
		}
	}
	return s
}

// stdioPipe 是一路子进程输出的日志管道:write 已交给 cmd 作其继承副本,closeWrite
// 关闭本进程持的那份(必须在 Start 之后调);读端由 scanOutput 在 EOF 后关闭。
// 零值(管道创建失败)安全。
type stdioPipe struct {
	write *os.File
}

func (p stdioPipe) closeWrite() {
	if p.write != nil {
		_ = p.write.Close()
	}
}

// logPipe 为 cmd 的一路输出(stdout/stderr)建立打码日志管道并挂到 cmd 上;失败时
// 返回错误且该路输出保持丢弃(行为等同改造前)。
func logPipe(cmd *exec.Cmd, desc string, cfg Config) (stdioPipe, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return stdioPipe{}, fmt.Errorf("easytier: 创建 %s 日志管道:%w", desc, err)
	}
	go scanOutput(pr, desc, cfg.sensitive())
	if desc == "stderr" {
		cmd.Stderr = pw
	} else {
		cmd.Stdout = pw
	}
	return stdioPipe{write: pw}, nil
}

// scanOutput 扫读管道并逐行打码落日志(不截断,保留完整输出;行尾换行由 log
// 自带)。写端的全部副本(子进程继承的那份由 OS 随其退出关闭)关闭后读到 EOF
// 退出;自己持的读端在退出时关闭。超 64KB 默认上限的长行经 Buffer 放宽到 1MB。
func scanOutput(pr *os.File, desc string, secrets []string) {
	defer pr.Close()
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			log.Printf("easytier-core(%s): %s", desc, maskValue(line, secrets))
		}
	}
}
