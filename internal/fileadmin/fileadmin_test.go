// fileadmin_test.go — 离线单测:fake Runner 注入(不触网),覆盖路径校验、find 输出
// 解析、目录列表排序、小文本读取(超限/二进制判定)、保存(备份+stdin 写入)与
// 流式下载的命令构造。
package fileadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/huangchengsir/pipewright/internal/target"
)

// ── 测试夹具 ─────────────────────────────────────────────────────────────────

// fakeRunner 记录全部命令;outputs 按完整命令键返回响应,streams 按键返回流,
// fails 按键、failPrefixes 按前缀返回错误(时间戳后缀命令用);未配置的命令默认
// 成功空输出。
type fakeRunner struct {
	mu           sync.Mutex
	calls        []string
	stdins       []string
	outputs      map[string]*target.ExecResult
	fails        map[string]error
	failPrefixes []string
	streams      map[string]string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		outputs: map[string]*target.ExecResult{},
		fails:   map[string]error{},
		streams: map[string]string{},
	}
}

// failWithRes 注入「非零退出 + stderr」类失败(模拟生产 Runner 对退出码的包装:
// res 与错误一并返回,stat 靠此识别 No such file)。
func (f *fakeRunner) failWithRes(key, stderr string) {
	f.outputs[key] = &target.ExecResult{ExitCode: 1, Stderr: stderr}
	f.fails[key] = fmt.Errorf("%w: 退出码 1\n%s", ErrExec, stderr)
}

func cmdKey(cmd []string) string { return strings.Join(cmd, " ") }

// dispatch 按键/前缀查错误,否则查响应,默认成功空输出。
func (f *fakeRunner) dispatch(key string) (*target.ExecResult, error) {
	if err, ok := f.fails[key]; ok {
		return f.outputs[key], err
	}
	for _, p := range f.failPrefixes {
		if strings.HasPrefix(key, p) {
			return nil, fmt.Errorf("fake fail: %s", key)
		}
	}
	if res, ok := f.outputs[key]; ok {
		return res, nil
	}
	return &target.ExecResult{}, nil
}

func (f *fakeRunner) Run(_ context.Context, _ string, cmd []string) (*target.ExecResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmdKey(cmd))
	return f.dispatch(cmdKey(cmd))
}

func (f *fakeRunner) RunWithStdin(_ context.Context, _ string, cmd []string, stdin io.Reader) (*target.ExecResult, error) {
	b, _ := io.ReadAll(stdin)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmdKey(cmd))
	f.stdins = append(f.stdins, string(b))
	return f.dispatch(cmdKey(cmd))
}

func (f *fakeRunner) Stream(_ context.Context, _ string, cmd []string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, cmdKey(cmd))
	if data, ok := f.streams[cmdKey(cmd)]; ok {
		return io.NopCloser(strings.NewReader(data)), nil
	}
	return io.NopCloser(strings.NewReader("")), nil
}

func (f *fakeRunner) hasPrefix(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func (f *fakeRunner) lastStdin() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.stdins) == 0 {
		return ""
	}
	return f.stdins[len(f.stdins)-1]
}

func newTestService(fr *fakeRunner) Service { return New(nil, WithRunner(fr)) }

// statKey 构造判型命令键(与 statCmd 输出一致)。
func statKey(path string) string {
	return cmdKey(statCmd(path))
}

// ── validatePath ─────────────────────────────────────────────────────────────

func TestValidatePath(t *testing.T) {
	cases := []struct {
		path string
		ok   bool
	}{
		{"/var/log", true},
		{"/", true},
		{"/etc/nginx/nginx.conf", true},
		{"/opt/my app/data file.txt", true}, // 空格合法(argv 转义保证安全)
		{"var/log", false},                  // 相对路径
		{"", false},
		{"/a/../b", false}, // ..
		{"/..", false},
		{"/a\x00b", false}, // 控制字符
		{"/a\nb", false},
		{"/a\tb", false},
		{"/" + strings.Repeat("a", 5000), false}, // 过长
	}
	for _, c := range cases {
		err := validatePath(c.path)
		if c.ok && err != nil {
			t.Errorf("validatePath(%q) 应通过,得到 %v", c.path, err)
		}
		if !c.ok && err == nil {
			t.Errorf("validatePath(%q) 应拒绝", c.path)
		}
		if !c.ok && err != nil && !errors.Is(err, ErrInvalidInput) {
			t.Errorf("validatePath(%q) 应映射 ErrInvalidInput,得到 %v", c.path, err)
		}
	}
}

// ── find 输出解析 ────────────────────────────────────────────────────────────

func TestParseFindOutput(t *testing.T) {
	out := "d\t4096\t1710000000.5000000000\t755\troot\troot\tlogs\n" +
		"f\t1024\t1710000001\t644\troot\troot\tapp.conf\n" +
		"l\t1\t1710000002\t777\troot\troot\tcurrent\n" +
		"f\t7\t1710000003\t600\twww\twww\thas\ttab\tname\n" + // 名称含 tab:多余字段并入
		"garbage-line\n" + // 字段不足:跳过
		"f\tnotanumber\t1710000004\t644\troot\troot\tbad\n" + // 大小解析失败:跳过
		"d\t4096\t1710000005\t755\troot\troot\tzz-dir\n"
	entries := parseFindOutput("/var", out)
	if len(entries) != 5 {
		t.Fatalf("应解析出 5 个条目,得到 %d: %+v", len(entries), entries)
	}
	// 排序:目录在前、同层名称升序。
	if entries[0].Type != "dir" || entries[0].Name != "logs" {
		t.Errorf("首个条目应为目录 logs,得到 %+v", entries[0])
	}
	if entries[1].Type != "dir" || entries[1].Name != "zz-dir" {
		t.Errorf("第二条目应为目录 zz-dir,得到 %+v", entries[1])
	}
	byName := map[string]DirEntry{}
	for _, e := range entries {
		byName[e.Name] = e
	}
	if e := byName["has\ttab\tname"]; e.Size != 7 || e.Owner != "www" {
		t.Errorf("含 tab 名称条目解析错误: %+v", e)
	}
	if e, ok := byName["bad"]; ok {
		t.Errorf("大小不可解析的行应跳过,却得到 %+v", e)
	}
	if e := byName["current"]; e.Type != "symlink" || e.Path != "/var/current" {
		t.Errorf("symlink 条目错误: %+v", e)
	}
	if e := byName["app.conf"]; e.Path != "/var/app.conf" || e.Mode != "644" || e.Mtime != 1710000001 {
		t.Errorf("file 条目错误: %+v", e)
	}
}

func TestParseFindOutputRootParent(t *testing.T) {
	entries := parseFindOutput("/", "d\t4096\t1710000000\t755\troot\troot\tetc\n")
	if len(entries) != 1 || entries[0].Path != "/etc" {
		t.Fatalf("根目录拼接应为 /etc,得到 %+v", entries)
	}
}

// ── List ─────────────────────────────────────────────────────────────────────

func TestListHappyPath(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/var/log")] = &target.ExecResult{Stdout: "directory|4096\n"}
	findKey := cmdKey([]string{"find", "-H", "/var/log", "-mindepth", "1", "-maxdepth", "1", "-printf", findPrintFmt})
	fr.outputs[findKey] = &target.ExecResult{Stdout: "f\t2048\t1710000000\t644\troot\troot\tsyslog\n"}

	svc := newTestService(fr)
	listing, err := svc.List(context.Background(), "s1", "/var/log")
	if err != nil {
		t.Fatalf("List 失败: %v", err)
	}
	if listing.Path != "/var/log" || len(listing.Entries) != 1 {
		t.Fatalf("列表错误: %+v", listing)
	}
	e := listing.Entries[0]
	if e.Name != "syslog" || e.Path != "/var/log/syslog" || e.Type != "file" || e.Size != 2048 {
		t.Errorf("条目错误: %+v", e)
	}
}

func TestListNotDirectory(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/etc/hosts")] = &target.ExecResult{Stdout: "regular file|24\n"}
	svc := newTestService(fr)
	_, err := svc.List(context.Background(), "s1", "/etc/hosts")
	if !errors.Is(err, ErrNotDir) {
		t.Fatalf("应返回 ErrNotDir,得到 %v", err)
	}
}

func TestListNotFound(t *testing.T) {
	fr := newFakeRunner()
	fr.failWithRes(statKey("/no/such"), "stat: cannot statx '/no/such': No such file or directory")
	svc := newTestService(fr)
	_, err := svc.List(context.Background(), "s1", "/no/such")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("应返回 ErrNotFound,得到 %v", err)
	}
}

func TestListInvalidPath(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr)
	_, err := svc.List(context.Background(), "s1", "relative/path")
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("应返回 ErrInvalidInput,得到 %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("非法路径不应触达远端命令,得到 %v", fr.calls)
	}
}

// ── Read ─────────────────────────────────────────────────────────────────────

func TestReadTextFile(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/etc/app.conf")] = &target.ExecResult{Stdout: "regular file|11\n"}
	headKey := cmdKey([]string{"sh", "-c", `head -c 65536 -- "$0"`, "/etc/app.conf"})
	fr.outputs[headKey] = &target.ExecResult{Stdout: "hello\nworld"}

	svc := newTestService(fr)
	tf, err := svc.Read(context.Background(), "s1", "/etc/app.conf")
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if !tf.Editable || tf.Binary || tf.TooLarge || tf.Content != "hello\nworld" || tf.Size != 11 {
		t.Errorf("读取结果错误: %+v", tf)
	}
}

func TestReadTooLarge(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/var/log/big.log")] = &target.ExecResult{Stdout: "regular file|65537\n"}
	svc := newTestService(fr)
	tf, err := svc.Read(context.Background(), "s1", "/var/log/big.log")
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if !tf.TooLarge || tf.Editable || tf.Content != "" {
		t.Errorf("超限文件应 TooLarge 且无内容: %+v", tf)
	}
	if len(fr.calls) != 1 { // 只有 stat,不应拉内容
		t.Errorf("超限文件只应执行 stat,得到 %v", fr.calls)
	}
}

func TestReadBinaryNUL(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/bin/true")] = &target.ExecResult{Stdout: "regular file|100\n"}
	headKey := cmdKey([]string{"sh", "-c", `head -c 65536 -- "$0"`, "/bin/true"})
	fr.outputs[headKey] = &target.ExecResult{Stdout: "ab\x00cd"}
	svc := newTestService(fr)
	tf, err := svc.Read(context.Background(), "s1", "/bin/true")
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if !tf.Binary || tf.Editable || tf.Content != "" {
		t.Errorf("NUL 字节应判二进制: %+v", tf)
	}
}

func TestReadBinaryInvalidUTF8(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/data/latin1.txt")] = &target.ExecResult{Stdout: "regular file|10\n"}
	headKey := cmdKey([]string{"sh", "-c", `head -c 65536 -- "$0"`, "/data/latin1.txt"})
	fr.outputs[headKey] = &target.ExecResult{Stdout: "caf\xe9"} // Latin-1 字节
	svc := newTestService(fr)
	tf, err := svc.Read(context.Background(), "s1", "/data/latin1.txt")
	if err != nil {
		t.Fatalf("Read 失败: %v", err)
	}
	if !tf.Binary {
		t.Errorf("非 UTF-8 应判二进制: %+v", tf)
	}
}

func TestReadNotRegularFile(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/var/log")] = &target.ExecResult{Stdout: "directory|4096\n"}
	svc := newTestService(fr)
	_, err := svc.Read(context.Background(), "s1", "/var/log")
	if !errors.Is(err, ErrNotFile) {
		t.Fatalf("应返回 ErrNotFile,得到 %v", err)
	}
}

// ── Write ────────────────────────────────────────────────────────────────────

func TestWriteBacksUpThenWrites(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/etc/app.conf")] = &target.ExecResult{Stdout: "regular file|11\n"}
	svc := newTestService(fr)
	backup, err := svc.Write(context.Background(), "s1", "/etc/app.conf", "new content")
	if err != nil {
		t.Fatalf("Write 失败: %v", err)
	}
	if !strings.HasPrefix(backup, "/etc/app.conf.pipewright-bak.") {
		t.Errorf("备份路径格式错误: %s", backup)
	}
	// 先备份(cp -a,时间戳后缀)再经 stdin 覆盖写入。
	if !fr.hasPrefix(`sh -c cp -a -- "$0" "$1" /etc/app.conf /etc/app.conf.pipewright-bak.`) {
		t.Errorf("缺少备份命令,调用序列: %v", fr.calls)
	}
	if !fr.hasPrefix(`sh -c cat > "$0" /etc/app.conf`) {
		t.Errorf("缺少写入命令,调用序列: %v", fr.calls)
	}
	if fr.lastStdin() != "new content" {
		t.Errorf("stdin 内容错误: %q", fr.lastStdin())
	}
}

func TestWriteOversizedContent(t *testing.T) {
	fr := newFakeRunner()
	svc := newTestService(fr)
	_, err := svc.Write(context.Background(), "s1", "/tmp/x", strings.Repeat("a", maxTextBytes+1))
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("应返回 ErrInvalidInput,得到 %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("超限内容不应触达远端,得到 %v", fr.calls)
	}
}

func TestWriteRequiresExistingRegularFile(t *testing.T) {
	fr := newFakeRunner()
	fr.failWithRes(statKey("/etc/new.conf"), "stat: cannot statx '/etc/new.conf': No such file or directory")
	svc := newTestService(fr)
	if _, err := svc.Write(context.Background(), "s1", "/etc/new.conf", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("不存在的文件应 ErrNotFound(防误建新文件),得到 %v", err)
	}

	fr2 := newFakeRunner()
	fr2.outputs[statKey("/var/log")] = &target.ExecResult{Stdout: "directory|4096\n"}
	svc2 := newTestService(fr2)
	if _, err := svc2.Write(context.Background(), "s1", "/var/log", "x"); !errors.Is(err, ErrNotFile) {
		t.Fatalf("目录应 ErrNotFile,得到 %v", err)
	}
}

func TestWriteBackupFailureAborts(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/etc/app.conf")] = &target.ExecResult{Stdout: "regular file|11\n"}
	// cp 命令带时间戳后缀,按前缀注入失败。
	fr.failPrefixes = []string{`sh -c cp -a -- "$0" "$1"`}
	svc := newTestService(fr)
	_, err := svc.Write(context.Background(), "s1", "/etc/app.conf", "new")
	if !errors.Is(err, ErrExec) {
		t.Fatalf("备份失败应中止并返回 ErrExec,得到 %v", err)
	}
	if fr.hasPrefix(`sh -c cat > "$0" /etc/app.conf`) {
		t.Errorf("备份失败后不应再写入,调用序列: %v", fr.calls)
	}
}

// ── Download ─────────────────────────────────────────────────────────────────

func TestDownloadStreamsFile(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/var/log/syslog")] = &target.ExecResult{Stdout: "regular file|25\n"}
	fr.streams[cmdKey([]string{"cat", "--", "/var/log/syslog"})] = "line1\nline2\n..."
	svc := newTestService(fr)
	rc, size, err := svc.Download(context.Background(), "s1", "/var/log/syslog")
	if err != nil {
		t.Fatalf("Download 失败: %v", err)
	}
	defer rc.Close()
	if size != 25 {
		t.Errorf("大小应取自 stat(25),得到 %d", size)
	}
	data, _ := io.ReadAll(rc)
	if string(data) != "line1\nline2\n..." {
		t.Errorf("流内容错误: %q", data)
	}
}

func TestDownloadNotRegularFile(t *testing.T) {
	fr := newFakeRunner()
	fr.outputs[statKey("/var/log")] = &target.ExecResult{Stdout: "directory|4096\n"}
	svc := newTestService(fr)
	_, _, err := svc.Download(context.Background(), "s1", "/var/log")
	if !errors.Is(err, ErrNotFile) {
		t.Fatalf("应返回 ErrNotFile,得到 %v", err)
	}
}
