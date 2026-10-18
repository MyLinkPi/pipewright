// Package fileadmin 是「服务器文件查看」的领域层(目录浏览 + 文件下载 + 小文本
// 在线查看/编辑)。
//
// 所有操作经 target.Service 以 SSH 在登记服务器上执行:命令一律 []string argv
// (AC-SEC-02);`sh -c` 场景路径作位置参数 $0/$1,绝不拼进脚本体。提权与
// diskadmin 不同:浏览类操作不应因无 sudo 整体不可用,故 root 直跑、非 root 探测
// 免密 sudo(结果按服务器缓存 10 分钟)通过则前缀 sudo -n,**探测失败回退登录
// 用户身份执行**;写受限文件时自然得到权限错误。
//
// 在线编辑纪律:仅限已存在的常规文件且 ≤64KiB(防误建新文件/防大文件撑爆请求与
// 编辑器);保存前强制 cp -a 同目录备份(<路径>.pipewright-bak.<UTC 时间戳>),
// 备份失败中止保存;全部保存留审计(HTTP 层)。下载走流式 cat(io.Copy,不占内存)。
package fileadmin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/huangchengsir/pipewright/internal/target"
)

// 领域错误(人话由 httpapi 层映射状态码;错误体不含凭据,可带远端 stderr 尾部)。
var (
	// ErrInvalidInput 表示入参非法(路径不合规/内容超限等)。
	ErrInvalidInput = errors.New("fileadmin: invalid input")
	// ErrServerNotFound 表示目标服务器不存在。
	ErrServerNotFound = errors.New("fileadmin: server not found")
	// ErrNotFound 表示远端路径不存在。
	ErrNotFound = errors.New("fileadmin: path not found")
	// ErrNotDir 表示目标不是目录(列表操作要求目录)。
	ErrNotDir = errors.New("fileadmin: not a directory")
	// ErrNotFile 表示目标不是常规文件(读取/下载/编辑操作要求文件)。
	ErrNotFile = errors.New("fileadmin: not a regular file")
	// ErrExec 表示远端命令执行失败(非零退出/不可达;错误体带 stderr 尾部)。
	ErrExec = errors.New("fileadmin: remote command failed")
)

// maxTextBytes 是在线查看/编辑的大小上限(64KiB;超过仅可下载)。
const maxTextBytes = 64 << 10

// maxPathLen 是路径长度上限(远端 PATH_MAX 4096,提前拒绝)。
const maxPathLen = 4096

// DirEntry 是目录里的一个条目。Path 为含父目录的全路径(前端免拼接)。
type DirEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Type  string `json:"type"` // dir|file|symlink|other
	Size  int64  `json:"size"`
	Mode  string `json:"mode"` // 八进制权限,如 "644"
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mtime int64  `json:"mtime"` // epoch 秒
}

// DirListing 是一次目录列表结果(目录在前、名称升序)。
type DirListing struct {
	Path    string     `json:"path"`
	Entries []DirEntry `json:"entries"`
}

// TextFile 是小文本读取结果:Binary/TooLarge 时不携带内容,前端引导下载;
// Editable = 可在线编辑(纯文本且未超限)。
type TextFile struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Binary   bool   `json:"binary"`
	TooLarge bool   `json:"tooLarge"`
	Editable bool   `json:"editable"`
	Content  string `json:"content"`
}

// Service 定义服务器文件查看对外接口(HTTP 面契约)。
type Service interface {
	// List 返回 path 的目录条目(须为目录)。
	List(ctx context.Context, serverID, path string) (*DirListing, error)
	// Read 返回小文本文件内容与元数据(常规文件;>64KiB 或二进制时仅元数据)。
	Read(ctx context.Context, serverID, path string) (*TextFile, error)
	// Write 以 content 覆盖已存在的常规文件(先同目录备份;备份失败中止)。
	// 返回备份文件路径(供审计与前端提示)。
	Write(ctx context.Context, serverID, path, content string) (backupPath string, err error)
	// Download 流式读远端常规文件,返回内容流与字节大小(Content-Length 用)。
	Download(ctx context.Context, serverID, path string) (io.ReadCloser, int64, error)
}

// Option 配置 New(测试注入 fake Runner 等)。
type Option func(*service)

// WithRunner 覆盖 SSH 执行器(单测注入 fake,不触网)。
func WithRunner(r Runner) Option { return func(s *service) { s.runner = r } }

// service 是 Runner 支撑的 Service 实现(操作无状态,无持久层)。
type service struct {
	servers target.Service
	runner  Runner
}

// New 构造文件查看服务。
func New(servers target.Service, opts ...Option) Service {
	s := &service{servers: servers, runner: newSSHRunner(servers)}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// validatePath 校验远端绝对路径:必须 "/" 开头、无 ".." 段、无控制字符、长度 ≤4096。
// 命令本就 argv 化(shellQuote 转义),这里是纵深防御 + 保证 find -printf 输出可解析
// (名称含换行会破坏行协议)。空格等合法文件名字符允许。
func validatePath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("%w: 路径须为绝对路径", ErrInvalidInput)
	}
	if len(p) > maxPathLen {
		return fmt.Errorf("%w: 路径过长(>4096)", ErrInvalidInput)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("%w: 路径不得含 ..", ErrInvalidInput)
		}
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: 路径含控制字符", ErrInvalidInput)
		}
	}
	return nil
}

// joinEntry 拼接父目录与条目名("/" 根目录不加重复斜杠)。
func joinEntry(dir, name string) string {
	if dir == "/" {
		return "/" + name
	}
	return dir + "/" + name
}
