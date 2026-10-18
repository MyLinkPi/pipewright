// browse.go — 目录列表 / 小文本读取 / 覆盖保存 / 流式下载的远端命令与解析。
//
// 命令纪律(AC-SEC-02):一律 []string argv,经 target 层单引号转义;`sh -c` 场景
// 路径作位置参数 $0/$1,绝不拼进脚本体。判型用 stat -L(跟随符号链接;LC_ALL=C
// 消除本地化输出),列表用 find -H(仅跟随命令行参数上的链接,条目本身如实显示
// symlink 类型)。目标机为 Linux(GNU stat/find,与 lsblk 先例一致)。
package fileadmin

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// findPrintFmt 是目录列表的 find -printf 输出格式:类型/字节/epoch 修改时间/
// 八进制权限/属主/属组/文件名,以 tab 分隔(解析端对超字段宽容合并)。
const findPrintFmt = "%y\t%s\t%T@\t%m\t%u\t%g\t%f\n"

// statCmd 构造判型+大小命令:LC_ALL=C 消除 %F 的本地化,-L 跟随符号链接,
// "--" 防御以 "-" 开头的路径被当 flag(校验层已拒绝非 / 开头,纵深防御)。
func statCmd(path string) []string {
	return []string{"sh", "-c", `LC_ALL=C stat -Lc "%F|%s" -- "$0"`, path}
}

// fileInfo 是 stat 的解析结果。
type fileInfo struct {
	IsDir  bool
	IsFile bool
	Size   int64
}

// parseStat 解析 `stat -Lc "%F|%s"` 输出("regular file|123" / "directory|4096")。
func parseStat(out string) (fileInfo, error) {
	s := strings.TrimSpace(out)
	idx := strings.LastIndex(s, "|")
	if idx < 0 {
		return fileInfo{}, fmt.Errorf("%w: stat 输出无法解析: %q", ErrExec, truncateFor(s, 200))
	}
	kind, sizeStr := s[:idx], s[idx+1:]
	size, err := strconv.ParseInt(strings.TrimSpace(sizeStr), 10, 64)
	if err != nil {
		return fileInfo{}, fmt.Errorf("%w: stat 大小无法解析: %q", ErrExec, truncateFor(s, 200))
	}
	return fileInfo{IsDir: kind == "directory", IsFile: kind == "regular file", Size: size}, nil
}

// stat 远端路径判型+大小(经 Runner,提权可用则提权)。
func (s *service) stat(ctx context.Context, serverID, path string) (fileInfo, error) {
	res, err := s.runner.Run(ctx, serverID, statCmd(path))
	if err != nil {
		if res != nil && res.ExitCode != 0 && isNoEnt(res.Stderr) {
			return fileInfo{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return fileInfo{}, err
	}
	return parseStat(res.Stdout)
}

// isNoEnt 报告 stderr 是否为「文件不存在」类错误(映射 404 而非 502)。
func isNoEnt(stderr string) bool {
	return strings.Contains(stderr, "No such file or directory")
}

// List 返回 path 的目录条目(目录在前、名称升序)。
func (s *service) List(ctx context.Context, serverID, path string) (*DirListing, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	st, err := s.stat(ctx, serverID, path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir {
		return nil, fmt.Errorf("%w: %s", ErrNotDir, path)
	}
	res, err := s.runner.Run(ctx, serverID, []string{
		"find", "-H", path, "-mindepth", "1", "-maxdepth", "1", "-printf", findPrintFmt,
	})
	if err != nil {
		return nil, err
	}
	entries := parseFindOutput(path, res.Stdout)
	out := DirListing{Path: path, Entries: entries}
	return &out, nil
}

// parseFindOutput 解析 find -printf 输出为条目列表(含父目录拼接与排序)。
// 宽容策略:字段多于 7 个时视为文件名含 tab,多余字段并入名称;字段不足或大小/
// 时间解析失败的行跳过(文件名含换行会破坏行协议,此类条目被自然丢弃)。
func parseFindOutput(dir, out string) []DirEntry {
	typeRank := map[string]int{"dir": 0, "file": 1, "symlink": 2, "other": 3}
	entries := make([]DirEntry, 0, 16)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 7 {
			continue
		}
		name := strings.Join(parts[6:], "\t")
		if name == "" || name == "." || name == ".." {
			continue
		}
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		mtime := int64(0)
		if f, err := strconv.ParseFloat(strings.TrimSpace(parts[2]), 64); err == nil {
			mtime = int64(f)
		}
		typ := "other"
		switch parts[0] {
		case "d":
			typ = "dir"
		case "f":
			typ = "file"
		case "l":
			typ = "symlink"
		}
		entries = append(entries, DirEntry{
			Name: name, Path: joinEntry(dir, name), Type: typ,
			Size: size, Mode: parts[3], Owner: parts[4], Group: parts[5], Mtime: mtime,
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		ri, rj := typeRank[entries[i].Type], typeRank[entries[j].Type]
		if ri != rj {
			return ri < rj
		}
		return entries[i].Name < entries[j].Name
	})
	return entries
}

// Read 返回小文本文件内容与元数据(>64KiB 或二进制时仅元数据,不报错)。
func (s *service) Read(ctx context.Context, serverID, path string) (*TextFile, error) {
	if err := validatePath(path); err != nil {
		return nil, err
	}
	st, err := s.stat(ctx, serverID, path)
	if err != nil {
		return nil, err
	}
	if !st.IsFile {
		return nil, fmt.Errorf("%w: %s", ErrNotFile, path)
	}
	out := TextFile{Path: path, Size: st.Size}
	if st.Size > maxTextBytes {
		out.TooLarge = true
		return &out, nil
	}
	res, err := s.runner.Run(ctx, serverID, []string{"sh", "-c", `head -c ` + strconv.Itoa(maxTextBytes) + ` -- "$0"`, path})
	if err != nil {
		return nil, err
	}
	content := res.Stdout
	if strings.ContainsRune(content, 0) || !utf8.ValidString(content) {
		// NUL 字节或非 UTF-8 视为二进制(source.go blob 先例;纯文本契约要求)。
		out.Binary = true
		return &out, nil
	}
	out.Content = content
	out.Editable = true
	return &out, nil
}

// Write 以 content 覆盖已存在的常规文件:先 cp -a 同目录备份(失败中止,原文件
// 未动),再经 stdin 流式写入(cat > 覆盖保留 inode 权限/属主)。返回备份路径。
func (s *service) Write(ctx context.Context, serverID, path, content string) (string, error) {
	if err := validatePath(path); err != nil {
		return "", err
	}
	if int64(len(content)) > maxTextBytes {
		return "", fmt.Errorf("%w: 内容超过 64KiB 上限", ErrInvalidInput)
	}
	st, err := s.stat(ctx, serverID, path)
	if err != nil {
		return "", err
	}
	if !st.IsFile {
		return "", fmt.Errorf("%w: %s", ErrNotFile, path)
	}

	backup := path + ".pipewright-bak." + time.Now().UTC().Format("20060102T150405Z")
	if _, err := s.runner.Run(ctx, serverID, []string{"sh", "-c", `cp -a -- "$0" "$1"`, path, backup}); err != nil {
		return "", fmt.Errorf("%w: 备份失败,已中止保存: %v", ErrExec, err)
	}
	if _, err := s.runner.RunWithStdin(ctx, serverID, []string{"sh", "-c", `cat > "$0"`, path}, strings.NewReader(content)); err != nil {
		return backup, fmt.Errorf("%w: 写入失败(备份保留在 %s): %v", ErrExec, backup, err)
	}
	return backup, nil
}

// Download 流式读远端常规文件:先 stat 取大小(Content-Length 用),再 cat 流式
// 输出。调用方读到底或关闭 ReadCloser 即释放 SSH session(ctx 取消亦然)。
func (s *service) Download(ctx context.Context, serverID, path string) (io.ReadCloser, int64, error) {
	if err := validatePath(path); err != nil {
		return nil, 0, err
	}
	st, err := s.stat(ctx, serverID, path)
	if err != nil {
		return nil, 0, err
	}
	if !st.IsFile {
		return nil, 0, fmt.Errorf("%w: %s", ErrNotFile, path)
	}
	rc, err := s.runner.Stream(ctx, serverID, []string{"cat", "--", path})
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrExec, err)
	}
	return rc, st.Size, nil
}

// truncateFor 截断字符串用于错误信息展示。
func truncateFor(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
