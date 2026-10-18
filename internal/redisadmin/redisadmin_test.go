// redisadmin_test.go 是 Redis 管理领域的存取/键操作回归测试(不依赖真实 Redis;
// 连接层经注入 fake redisClient 覆盖 SCAN 补齐分页/键详情/只读守卫等语义)。
package redisadmin

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/vault"
)

func testKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 7)
	}
	return &k
}

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// fakeRedisClient 是 redisClient 的内存实现(可控 SCAN 批次/键数据)。
type fakeRedisClient struct {
	pingErr error
	infoRaw string
	dbSize  int64

	// scanBatches 按起始 cursor 返回 (nextCursor, keys)(模拟 SCAN 分批;缺省键返回空批+归零)。
	scanBatches map[uint64]fakeScanBatch
	scanCalls   []int64 // 每轮请求的 COUNT(断言补齐行为)

	types    map[string]string
	ttls     map[string]time.Duration
	memUsage map[string]int64
	strs     map[string]string
	hashes   map[string][]string // 扁平 [field,value,...]
	lists    map[string][]string
	sets     map[string][]string
	zsets    map[string][]ZEntry

	deleted []string
	expires map[string]time.Duration
}

type fakeScanBatch struct {
	next uint64
	keys []string
}

func (c *fakeRedisClient) Ping(ctx context.Context) error { return c.pingErr }

func (c *fakeRedisClient) InfoSection(ctx context.Context, sections ...string) (string, error) {
	return c.infoRaw, nil
}

func (c *fakeRedisClient) DBSize(ctx context.Context) (int64, error) { return c.dbSize, nil }

func (c *fakeRedisClient) Scan(ctx context.Context, cursor uint64, match string, count int64) (uint64, []string, error) {
	c.scanCalls = append(c.scanCalls, count)
	b, ok := c.scanBatches[cursor]
	if !ok {
		return 0, nil, nil
	}
	return b.next, b.keys, nil
}

func (c *fakeRedisClient) Type(ctx context.Context, key string) (string, error) {
	if t, ok := c.types[key]; ok {
		return t, nil
	}
	return "none", nil
}

func (c *fakeRedisClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.ttls[key], nil // 缺省零值 = 无 TTL 条目(详情对零值按 <0 → 持久化)
}

func (c *fakeRedisClient) MemoryUsage(ctx context.Context, key string) (int64, bool) {
	v, ok := c.memUsage[key]
	return v, ok
}

func (c *fakeRedisClient) GetRange(ctx context.Context, key string, start, end int64) (string, error) {
	s := c.strs[key]
	if int64(len(s)) <= end {
		return s, nil
	}
	return s[start : end+1], nil
}

func (c *fakeRedisClient) StrLen(ctx context.Context, key string) (int64, error) {
	return int64(len(c.strs[key])), nil
}

func (c *fakeRedisClient) HLen(ctx context.Context, key string) (int64, error) {
	return int64(len(c.hashes[key]) / 2), nil
}

func (c *fakeRedisClient) HScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error) {
	flat := c.hashes[key]
	if int64(len(flat)/2) > count {
		return 1, flat[:count*2], nil
	}
	return 0, flat, nil
}

func (c *fakeRedisClient) LLen(ctx context.Context, key string) (int64, error) {
	return int64(len(c.lists[key])), nil
}

func (c *fakeRedisClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	items := c.lists[key]
	if int64(len(items)) > stop+1 {
		return items[start : stop+1], nil
	}
	return items, nil
}

func (c *fakeRedisClient) SCard(ctx context.Context, key string) (int64, error) {
	return int64(len(c.sets[key])), nil
}

func (c *fakeRedisClient) SScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error) {
	items := c.sets[key]
	if int64(len(items)) > count {
		return 1, items[:count], nil
	}
	return 0, items, nil
}

func (c *fakeRedisClient) ZCard(ctx context.Context, key string) (int64, error) {
	return int64(len(c.zsets[key])), nil
}

func (c *fakeRedisClient) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]ZEntry, error) {
	items := c.zsets[key]
	if int64(len(items)) > stop+1 {
		return items[start : stop+1], nil
	}
	return items, nil
}

func (c *fakeRedisClient) Del(ctx context.Context, key string) (int64, error) {
	if _, ok := c.types[key]; !ok {
		return 0, nil
	}
	delete(c.types, key)
	c.deleted = append(c.deleted, key)
	return 1, nil
}

func (c *fakeRedisClient) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	if _, ok := c.types[key]; !ok {
		return false, nil
	}
	if c.expires == nil {
		c.expires = map[string]time.Duration{}
	}
	c.expires[key] = ttl
	return true, nil
}

func (c *fakeRedisClient) Persist(ctx context.Context, key string) (bool, error) {
	if _, ok := c.types[key]; !ok {
		return false, nil
	}
	delete(c.ttls, key)
	return true, nil
}

func (c *fakeRedisClient) Close() error { return nil }

// newTestService 构造注入 fake 客户端的 Service(不触真网络)。
func newTestService(t *testing.T, client redisClient) Service {
	t.Helper()
	st := storetest.Open(t)
	s := New(st.DB, vault.New(st.DB, testKey())).(*service)
	s.dial = func(host string, port int, password string, dbNum int) (redisClient, error) {
		return client, nil
	}
	return s
}

// TestInstanceCRUD 覆盖实例登记/编辑/只读开关/删除,含无密码实例与同地址冲突。
func TestInstanceCRUD(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, &fakeRedisClient{})

	created, err := svc.Create(ctx, CreateInput{Name: "缓存", Host: "10.0.0.9", Port: 6379, Password: "s3cret"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created.ReadOnly || created.Source != SourceManual {
		t.Fatalf("新实例应默认只读+manual: %+v", created)
	}
	got, _ := svc.Get(ctx, created.ID)
	if !got.PasswordConfigured {
		t.Fatalf("带密码实例 PasswordConfigured 应为 true")
	}

	// 无密码实例合法(内网自建)。
	plain, err := svc.Create(ctx, CreateInput{Name: "内网", Host: "10.0.0.10", Port: 6379})
	if err != nil {
		t.Fatalf("create(无密码): %v", err)
	}
	if plain.PasswordConfigured {
		t.Fatalf("无密码实例 PasswordConfigured 应为 false")
	}

	// 同地址重复 → ErrEndpointTaken。
	if _, err := svc.Create(ctx, CreateInput{Name: "重复", Host: "10.0.0.9", Port: 6379}); err != ErrEndpointTaken {
		t.Fatalf("duplicate err = %v, want ErrEndpointTaken", err)
	}

	upd, err := svc.Update(ctx, created.ID, UpdateInput{Name: strp("缓存-新"), DBNum: intp(2), Password: strp("")})
	if err != nil || upd.Name != "缓存-新" || upd.DBNum != 2 {
		t.Fatalf("update: %v %+v", err, upd)
	}
	if upd.PasswordConfigured {
		t.Fatalf("密码轮换为空串 = 清除密码,PasswordConfigured 应为 false")
	}

	if _, err := svc.SetReadOnly(ctx, created.ID, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	list, _ := svc.List(ctx)
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if err := svc.Delete(ctx, plain.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := svc.Get(ctx, plain.ID); err != ErrNotFound {
		t.Fatalf("get after delete err = %v, want ErrNotFound", err)
	}
}

// TestRegisterIdempotentRotate 应用市场联动登记幂等:同 host+port 命中 → 原地轮换密码;
// 重新登记不产生重复条目;密码必填。
func TestRegisterIdempotentRotate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t, &fakeRedisClient{})

	first, err := svc.Register(ctx, RegisterInput{
		Name: "Redis · build-1", Host: "192.168.1.20", Port: 6379,
		Password: "gen-secret", ServerID: "srv-1", StackName: "redis",
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if first.Updated || first.Instance.Source != SourceAppStore || !first.Instance.PasswordConfigured {
		t.Fatalf("首次登记应 Updated=false/appstore: %+v", first)
	}

	// 空密码拒绝。
	if _, err := svc.Register(ctx, RegisterInput{Name: "x", Host: "h", Port: 6379, Password: ""}); err != ErrEmptyPassword {
		t.Fatalf("空密码应 ErrEmptyPassword, got %v", err)
	}

	second, err := svc.Register(ctx, RegisterInput{
		Name: "Redis · build-1", Host: "192.168.1.20", Port: 6379,
		Password: "rotated", ServerID: "srv-1", StackName: "redis-2",
	})
	if err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if !second.Updated || second.Instance.ID != first.Instance.ID {
		t.Fatalf("重复登记应原地轮换: %+v", second)
	}
	list, _ := svc.List(ctx)
	if len(list) != 1 {
		t.Fatalf("不应产生重复条目, len=%d", len(list))
	}
	if list[0].StackName != "redis-2" || !list[0].PasswordConfigured {
		t.Fatalf("轮换后 stack/密码状态不符: %+v", list[0])
	}
}

// TestScanPagination SCAN 游标分页契约:服务端小批量返回时领域层循环补齐页大小;
// 页内重复键去重;游标归零即末页;count 钳制(<=0 默认 100)。
func TestScanPagination(t *testing.T) {
	ctx := context.Background()
	fc := &fakeRedisClient{
		// 三轮各 60 键(第 2 轮故意重复 5 个第 1 轮的键),第 3 轮后归零。
		scanBatches: map[uint64]fakeScanBatch{
			0: {next: 11, keys: genKeys("k", 0, 60)},
			11: {next: 22, keys: append(genKeys("k", 55, 60), genKeys("k", 60, 55)...)}, // 前 5 个重复
			22: {next: 0, keys: genKeys("z", 0, 40)},
		},
	}
	svc := newTestService(t, fc)
	inst, _ := svc.Create(ctx, CreateInput{Name: "x", Host: "h1", Port: 6379, Password: "p"})

	// 请求 100:60 + 55(去重 5)→ 115 ≥ 100 截断;nextCursor 停在补齐处之后的游标。
	res, err := svc.ScanKeys(ctx, inst.ID, 0, ScanInput{Cursor: 0, Count: 100})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(res.Keys) != 100 {
		t.Fatalf("应补齐到页大小 100, got %d", len(res.Keys))
	}
	seen := map[string]bool{}
	for _, k := range res.Keys {
		if seen[k] {
			t.Fatalf("页内不应重复: %s", k)
		}
		seen[k] = true
	}

	// 末页:起始游标 22 → 40 键后归零。
	res, err = svc.ScanKeys(ctx, inst.ID, 0, ScanInput{Cursor: 22, Count: 100})
	if err != nil || len(res.Keys) != 40 || res.NextCursor != 0 {
		t.Fatalf("末页应为 40 键+游标归零: %d/%d err=%v", len(res.Keys), res.NextCursor, err)
	}

	// count 钳制:<=0 → 默认 100。
	if _, err := svc.ScanKeys(ctx, inst.ID, 0, ScanInput{Count: 0}); err != nil {
		t.Fatalf("count 默认: %v", err)
	}
	// count 低于下限 → 50。
	res, _ = svc.ScanKeys(ctx, inst.ID, 0, ScanInput{Cursor: 0, Count: 10})
	if len(res.Keys) != 50 {
		t.Fatalf("count 下限应为 50, got %d", len(res.Keys))
	}
}

// genKeys 生成 [prefix-i] 形式的键切片 [from, from+n)。
func genKeys(prefix string, from, n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, prefix+"-"+strconv.Itoa(from+i))
	}
	return out
}

// TestKeyDetailTypes 键详情按类型的值预览:string 截断、hash/list/set/zset 前 N 成员、
// 不存在的键 → ErrKeyNotFound、TTL 语义(-1 持久化/正值秒)。
func TestKeyDetailTypes(t *testing.T) {
	ctx := context.Background()
	fc := &fakeRedisClient{
		types: map[string]string{
			"s": "string", "h": "hash", "l": "list", "e": "set", "z": "zset", "x": "stream",
		},
		ttls:     map[string]time.Duration{"s": -1 * time.Second, "h": 90 * time.Second},
		memUsage: map[string]int64{"s": 120},
		strs:     map[string]string{"s": strings.Repeat("a", 5000)},
		hashes:   map[string][]string{"h": {"f1", "v1", "f2", "v2"}},
		lists:    map[string][]string{"l": {"a", "b", "c"}},
		sets:     map[string][]string{"e": {"m1", "m2"}},
		zsets:    map[string][]ZEntry{"z": {{Member: "m", Score: 1.5}}},
	}
	svc := newTestService(t, fc)
	inst, _ := svc.Create(ctx, CreateInput{Name: "x", Host: "h1", Port: 6379, Password: "p"})

	d, err := svc.KeyDetail(ctx, inst.ID, 0, "s")
	if err != nil || d.Type != "string" || d.StrLen != 5000 || !d.Truncated || len(d.StrValue) != stringPreviewBytes {
		t.Fatalf("string 详情不符: %+v err=%v", d, err)
	}
	if d.TTLSeconds != -1 || d.MemoryBytes != 120 {
		t.Fatalf("string TTL/内存不符: %+v", d)
	}

	d, err = svc.KeyDetail(ctx, inst.ID, 0, "h")
	if err != nil || d.Len != 2 || len(d.HashMembers) != 2 || d.HashMembers[0].Field != "f1" {
		t.Fatalf("hash 详情不符: %+v err=%v", d, err)
	}
	if d.TTLSeconds != 90 {
		t.Fatalf("hash TTL 应 90s: %+v", d)
	}

	d, _ = svc.KeyDetail(ctx, inst.ID, 0, "l")
	if d.Type != "list" || d.Len != 3 || len(d.ListItems) != 3 {
		t.Fatalf("list 详情不符: %+v", d)
	}
	d, _ = svc.KeyDetail(ctx, inst.ID, 0, "e")
	if d.Type != "set" || len(d.SetItems) != 2 {
		t.Fatalf("set 详情不符: %+v", d)
	}
	d, _ = svc.KeyDetail(ctx, inst.ID, 0, "z")
	if d.Type != "zset" || len(d.ZSetItems) != 1 || d.ZSetItems[0].Score != 1.5 {
		t.Fatalf("zset 详情不符: %+v", d)
	}
	// stream 等扩展类型:只元信息,不炸。
	d, _ = svc.KeyDetail(ctx, inst.ID, 0, "x")
	if d.Type != "stream" || d.Len != 0 {
		t.Fatalf("stream 详情应仅元信息: %+v", d)
	}

	if _, err := svc.KeyDetail(ctx, inst.ID, 0, "missing"); err != ErrKeyNotFound {
		t.Fatalf("缺失键应 ErrKeyNotFound, got %v", err)
	}
}

// TestWriteGuards 只读守卫与写操作:锁定 → 删键/改 TTL 拒绝(ErrReadonly);
// 解锁后生效;键不存在 → ErrKeyNotFound;TTL -1 = PERSIST。
func TestWriteGuards(t *testing.T) {
	ctx := context.Background()
	fc := &fakeRedisClient{
		types: map[string]string{"k1": "string"},
		ttls:  map[string]time.Duration{"k1": 60 * time.Second},
	}
	svc := newTestService(t, fc)
	inst, _ := svc.Create(ctx, CreateInput{Name: "x", Host: "h1", Port: 6379, Password: "p"})

	if err := svc.DeleteKey(ctx, inst.ID, 0, "k1"); err != ErrReadonly {
		t.Fatalf("锁定删键应 ErrReadonly, got %v", err)
	}
	if err := svc.SetTTL(ctx, inst.ID, 0, "k1", 30); err != ErrReadonly {
		t.Fatalf("锁定改 TTL 应 ErrReadonly, got %v", err)
	}

	if _, err := svc.SetReadOnly(ctx, inst.ID, false); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if err := svc.DeleteKey(ctx, inst.ID, 0, "k1"); err != nil {
		t.Fatalf("解锁后删键: %v", err)
	}
	if len(fc.deleted) != 1 || fc.deleted[0] != "k1" {
		t.Fatalf("删键应生效: %v", fc.deleted)
	}
	// 再删同键 → ErrKeyNotFound(fake 已移除)。
	if err := svc.DeleteKey(ctx, inst.ID, 0, "k1"); err != ErrKeyNotFound {
		t.Fatalf("二次删键应 ErrKeyNotFound, got %v", err)
	}

	// TTL:先补回键,验证 EXPIRE 与 PERSIST(-1)。
	fc.types["k2"] = "string"
	if err := svc.SetTTL(ctx, inst.ID, 0, "k2", 30); err != nil {
		t.Fatalf("SetTTL: %v", err)
	}
	if fc.expires["k2"] != 30*time.Second {
		t.Fatalf("EXPIRE 应为 30s: %v", fc.expires["k2"])
	}
	if err := svc.SetTTL(ctx, inst.ID, 0, "k2", -1); err != nil {
		t.Fatalf("PERSIST: %v", err)
	}
	// 非法 TTL(<-1)拒绝。
	if err := svc.SetTTL(ctx, inst.ID, 0, "k2", -5); err == nil {
		t.Fatalf("TTL<-1 应拒绝")
	}
}

// TestInfo 概览解析:INFO 原文 → 版本/uptime/内存/连接数/keyspace + DBSIZE。
func TestInfo(t *testing.T) {
	ctx := context.Background()
	fc := &fakeRedisClient{
		infoRaw: "# Server\r\nredis_version:7.4.1\r\nuptime_in_seconds:3661\r\n" +
			"# Memory\r\nused_memory_human:1.5M\r\n" +
			"# Clients\r\nconnected_clients:8\r\n" +
			"# Keyspace\r\ndb0:keys=120,expires=3\r\ndb2:keys=7\r\n",
		dbSize: 120,
	}
	svc := newTestService(t, fc)
	inst, _ := svc.Create(ctx, CreateInput{Name: "x", Host: "h1", Port: 6379, Password: "p"})

	info, err := svc.Info(ctx, inst.ID, 0)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Version != "7.4.1" || info.UptimeSeconds != 3661 || info.UsedMemory != "1.5M" || info.ConnectedClients != 8 {
		t.Fatalf("info 字段不符: %+v", info)
	}
	if info.DBSize != 120 || info.Keyspace[0] != 120 || info.Keyspace[2] != 7 || len(info.Keyspace) != 2 {
		t.Fatalf("keyspace 不符: %+v", info.Keyspace)
	}
}

// TestConnectFailed 连接失败(PING 报错)渲染人话 TestResult(Err 含 ErrConnectFailed)。
func TestConnectFailed(t *testing.T) {
	ctx := context.Background()
	fc := &fakeRedisClient{pingErr: context.DeadlineExceeded}
	svc := newTestService(t, fc)
	inst, _ := svc.Create(ctx, CreateInput{Name: "x", Host: "h1", Port: 6379, Password: "p"})

	res, err := svc.Test(ctx, inst.ID)
	if err != nil || res.OK || res.Err == nil {
		t.Fatalf("连接失败应为 OK=false 的 result: %+v err=%v", res, err)
	}
}
