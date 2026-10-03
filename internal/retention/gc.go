// gc.go 实现制品库磁盘 blob 的孤儿回收:retention 删过期 run 时会连带删 run_artifacts **元数据行**,
// 但制品库(内容寻址磁盘库)里的 blob 文件此前**零清理**,孤儿永久累积。本文件补 GC:
//
//	周期扫描 artifacts/ 全部 blob 句柄 → 与 run_artifacts 的「引用集合」做差 → 无引用的 blob 删除。
//	引用集合同时取 reference 列与 metadata_json 里出现的 64 位 hex 串(format=files 产物的清单文件
//	句柄只存在于 metadata,不取就会误删)。
//
// 竞态防护:构建侧先 Put 落盘、后写 run_artifacts 行,存在短暂「无引用」窗口 → GC 只删
// 「无引用且 mtime 超过宽限期(默认 24h)」的 blob,双重判断防误删进行中构建的产物。
// 删除失败仅记日志(best-effort);GC 与 run 保留期天然对齐:run 过期 → 元数据行删 → blob 下轮回收。
package retention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultGCGracePeriod 是无引用 blob 的宽限期:落盘后多久仍无引用才判孤儿
// (构建侧 Put 与 run_artifacts 写行之间的窗口远小于此,防误删进行中构建)。
const defaultGCGracePeriod = 24 * time.Hour

// BlobStore 是制品库磁盘面的最小接口(由 artifactstore 满足;便于测试注入临时目录)。
type BlobStore interface {
	// Keys 返回库内全部 blob 句柄(64 位 hex)。
	Keys() ([]string, error)
	// Remove 删除指定句柄的 blob;不存在视为已删(返回 nil)。
	Remove(key string) error
	// ModTime 返回 blob 的修改时间(宽限期判定用);不存在 → 错误。
	ModTime(key string) (time.Time, error)
}

// GCOrphans 是 Sweeper.GCAble 的适配:用 Service 的引用集合 + 磁盘存储面执行一轮孤儿回收。
func (s *Service) GCOrphans(ctx context.Context) (int, error) {
	if s.gcStore == nil {
		return 0, nil
	}
	return GC(ctx, s.gcStore, s.referencedKeys, time.Now())
}

// WithBlobStore 注入制品库磁盘面(nil = 不做 GC)。main 装配用。
func (s *Service) WithBlobStore(bs BlobStore) *Service { s.gcStore = bs; return s }

// GC 执行一轮制品孤儿回收,返回删除的 blob 数。listRefs 返回当前全部「被引用句柄」
// (实现见 referencedKeys:reference 列 + metadata_json 内 hex 串)。全程 best-effort。
func GC(ctx context.Context, store BlobStore, listRefs func(ctx context.Context) (map[string]struct{}, error), now time.Time) (int, error) {
	keys, err := store.Keys()
	if err != nil {
		return 0, fmt.Errorf("retention: gc list blobs: %w", err)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	refs, err := listRefs(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, k := range keys {
		if _, ok := refs[k]; ok {
			continue // 有引用:活 blob(run 未过期 / format=files 清单成员)
		}
		mt, merr := store.ModTime(k)
		if merr != nil {
			continue // 已消失 / stat 失败:跳过,下轮再看
		}
		if now.Sub(mt) < defaultGCGracePeriod {
			continue // 宽限期内:可能是正在写入、尚未登记的进行中构建产物
		}
		if rerr := store.Remove(k); rerr != nil {
			log.Printf("[retention] 制品 GC:删除孤儿 blob %s 失败(下轮重试):%v", k[:12], rerr)
			continue
		}
		removed++
	}
	return removed, nil
}

// referencedKeys 汇总当前被引用的 blob 句柄集合:run_artifacts.reference 列 +
// metadata_json 文本中出现的所有 64 位 hex 串(format=files 的清单句柄只存在于 metadata)。
func (s *Service) referencedKeys(ctx context.Context) (map[string]struct{}, error) {
	refs := make(map[string]struct{}, 64)
	rows, err := s.db.QueryContext(ctx, `SELECT reference, IFNULL(metadata_json, '') FROM run_artifacts`)
	if err != nil {
		return nil, fmt.Errorf("retention: gc list refs: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var ref, meta string
		if err := rows.Scan(&ref, &meta); err != nil {
			return nil, fmt.Errorf("retention: gc scan refs: %w", err)
		}
		if isSHA256(ref) {
			refs[strings.ToLower(ref)] = struct{}{}
		}
		for _, tok := range hexTokens(meta) {
			refs[tok] = struct{}{}
		}
	}
	return refs, rows.Err()
}

// hexTokens 抽取 s 中全部 64 位小写 hex 串(粗暴按非 hex 字符切分后筛长度;metadata 体积有限,足够)。
func hexTokens(s string) []string {
	const hexchars = "0123456789abcdefABCDEF"
	out := make([]string, 0, 4)
	start := -1
	for i := 0; i <= len(s); i++ {
		inHex := i < len(s) && strings.IndexByte(hexchars, s[i]) >= 0
		if inHex && start < 0 {
			start = i
		} else if !inHex && start >= 0 {
			if i-start == sha256.Size*2 {
				if k := strings.ToLower(s[start:i]); isSHA256(k) {
					out = append(out, k)
				}
			}
			start = -1
		}
	}
	return out
}

// isSHA256 判断 s 是否为 64 位小写 hex(与 artifactstore.validKey 同口径,杜绝误把普通词当句柄)。
func isSHA256(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// gcStore 把 artifactstore.Store 适配为 BlobStore(路径布局 <root>/<前2位>/<hash> 由 store 自己管理,
// 这里只经 Keys 面操作;ModTime 走 os.Stat)。
type gcStore struct {
	root string
}

// NewBlobStore 构造面向指定制品库根目录的 GC 存储面(main 装配用)。
func NewBlobStore(root string) BlobStore { return gcStore{root: root} }

func (g gcStore) Keys() ([]string, error) {
	out := make([]string, 0, 64)
	shards, err := os.ReadDir(g.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	for _, sh := range shards {
		if !sh.IsDir() || len(sh.Name()) != 2 {
			continue
		}
		files, err := os.ReadDir(filepath.Join(g.root, sh.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.IsDir() && len(f.Name()) == sha256.Size*2 && isSHA256(f.Name()) {
				out = append(out, f.Name())
			}
		}
	}
	return out, nil
}

func (g gcStore) Remove(key string) error {
	if !isSHA256(key) {
		return fmt.Errorf("retention: gc remove: invalid key")
	}
	err := os.Remove(filepath.Join(g.root, key[:2], key))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

func (g gcStore) ModTime(key string) (time.Time, error) {
	fi, err := os.Stat(filepath.Join(g.root, key[:2], key))
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}
