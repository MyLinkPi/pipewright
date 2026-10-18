package redisadmin

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ---- 管理操作结果类型(触网;每操作即建即弃短连接)----

// ServerInfo 是 Redis 服务端概览(INFO server/memory/clients/keyspace + DBSIZE)。
type ServerInfo struct {
	Version          string
	UptimeSeconds    int64
	UsedMemory       string // 人读形式(如 1.5M),服务端原文
	ConnectedClients int64
	DBSize           int64             // 当前逻辑库键数
	Keyspace         map[int]int64     // 各逻辑库键数(INFO keyspace)
	Raw              map[string]string // 扁平原文字段(前端按需取)
}

// ScanInput 是键浏览入参(SCAN 游标分页;键空间可能百万级,绝不 KEYS *)。
type ScanInput struct {
	Cursor uint64 // 0 = 从头开始
	Match  string // glob 模式(可空 = 全部;如 user:* )
	Count  int    // 本页键数(钳 50-500,默认 100)
}

// ScanResult 是键浏览单页结果。NextCursor 为 0 表示遍历完成(末页)。
type ScanResult struct {
	Keys       []string
	NextCursor uint64
}

// HashMember 是 hash 类型的一个字段预览。
type HashMember struct {
	Field string
	Value string
}

// KeyDetail 是单键详情:类型/TTL/内存 + 按类型的值预览(截断,大值不整读)。
type KeyDetail struct {
	Key         string
	Type        string // string | hash | list | set | zset | stream | ...
	TTLSeconds  int64  // -1 = 持久化
	MemoryBytes int64  // MEMORY USAGE;0 = 服务端不支持

	// string 预览(前 2KB,超出截断标记)。
	StrValue  string
	StrLen    int64
	Truncated bool

	// 集合类预览(前 100 成员);Len 为集合总长。
	Len         int64
	HashMembers []HashMember
	ListItems   []string
	SetItems    []string
	ZSetItems   []ZEntry
}

// 值预览上限(大值不整读,string 前 2KB、集合前 100 成员)。
const (
	stringPreviewBytes = 2048
	memberPreview      = 100
	scanCountDefault   = 100
	scanCountMin       = 50
	scanCountMax       = 500
)

// dialFor 按实例 + 目标逻辑库建一个即用即弃客户端(密码解封只在内存瞬时存在)。
func (s *service) dialFor(ctx context.Context, inst *Instance, dbNum int) (redisClient, error) {
	if dbNum < 0 || dbNum > 15 {
		return nil, fmt.Errorf("%w: db %d", ErrInvalidDB, dbNum)
	}
	password, err := s.open(s.passwordSealedOf(ctx, inst.ID))
	if err != nil {
		return nil, err
	}
	return s.dial(inst.Host, inst.Port, password, dbNum)
}

// ensureWritable 只读守卫:锁定的实例拒绝一切写操作。
func ensureWritable(inst *Instance) error {
	if inst.ReadOnly {
		return ErrReadonly
	}
	return nil
}

// ---- 连接测试 ----

// Test 测试已存实例连通性(PING 计时 + 取版本)。
func (s *service) Test(ctx context.Context, id string) (*TestResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	client, err := s.dialFor(ctx, inst, inst.DBNum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return pingClient(ctx, client)
}

// TestDirect 用未保存的凭据直连测试(密码用完即弃,绝不持久化)。
func (s *service) TestDirect(ctx context.Context, in ConnectInput) (*TestResult, error) {
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" {
		return nil, ErrEmptyHost
	}
	if in.Port < 1 || in.Port > 65535 {
		return nil, ErrInvalidPort
	}
	if in.DBNum < 0 || in.DBNum > 15 {
		return nil, ErrInvalidDB
	}
	client, err := s.dial(in.Host, in.Port, in.Password, in.DBNum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return pingClient(ctx, client)
}

// pingClient PING 计时 + INFO server 取版本(连接/认证失败渲染人话,绝不含密码)。
func pingClient(ctx context.Context, client redisClient) (*TestResult, error) {
	start := time.Now()
	if err := client.Ping(ctx); err != nil {
		return &TestResult{OK: false, LatencyMs: time.Since(start).Milliseconds(), Err: fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))}, nil
	}
	latency := time.Since(start).Milliseconds()
	version := ""
	if raw, err := client.InfoSection(ctx, "server"); err == nil {
		version = parseInfoFlat(raw)["redis_version"]
	}
	return &TestResult{OK: true, LatencyMs: latency, Version: version}, nil
}

// ---- Info ----

// Info 服务端概览:版本/uptime/内存/连接数 + 各逻辑库键数 + 当前库 DBSIZE。
func (s *service) Info(ctx context.Context, id string, dbNum int) (*ServerInfo, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	client, err := s.dialFor(ctx, inst, dbNum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	raw, err := client.InfoSection(ctx, "server", "memory", "clients", "keyspace")
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
	}
	flat := parseInfoFlat(raw)
	info := &ServerInfo{
		Version:    flat["redis_version"],
		UsedMemory: flat["used_memory_human"],
		Raw:        flat,
		Keyspace:   map[int]int64{},
	}
	info.UptimeSeconds, _ = strconv.ParseInt(flat["uptime_in_seconds"], 10, 64)
	info.ConnectedClients, _ = strconv.ParseInt(flat["connected_clients"], 10, 64)
	for db, keys := range parseKeyspace(raw) {
		info.Keyspace[db] = keys
	}
	if info.DBSize, err = client.DBSize(ctx); err != nil {
		return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
	}
	return info, nil
}

// parseInfoFlat 把 INFO 原文解析为扁平 k/v(# 节注释行剔除;keyspace 的 dbN 行也进入,
// 但其值形如 keys=10,expires=2,由 parseKeyspace 单独解析)。
func parseInfoFlat(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, ":"); i > 0 {
			out[line[:i]] = strings.TrimSpace(line[i+1:])
		}
	}
	return out
}

// parseKeyspace 解析 INFO keyspace 节(db0:keys=10,expires=2 → {0:10})。
func parseKeyspace(raw string) map[int]int64 {
	out := map[int]int64{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "db") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		db, err := strconv.Atoi(strings.TrimPrefix(parts[0], "db"))
		if err != nil {
			continue
		}
		for _, kv := range strings.Split(parts[1], ",") {
			if strings.HasPrefix(kv, "keys=") {
				if n, err := strconv.ParseInt(strings.TrimPrefix(kv, "keys="), 10, 64); err == nil {
					out[db] = n
				}
			}
		}
	}
	return out
}

// ---- 键浏览(SCAN 游标分页)----

// ScanKeys 单页浏览键:领域层循环 SCAN 补齐页大小(或游标归零),批量内去重保序;
// 绝不 KEYS *(键空间可能百万级)。NextCursor 为 0 表示遍历完成。
func (s *service) ScanKeys(ctx context.Context, id string, dbNum int, in ScanInput) (*ScanResult, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if in.Count <= 0 {
		in.Count = scanCountDefault
	}
	if in.Count < scanCountMin {
		in.Count = scanCountMin
	}
	if in.Count > scanCountMax {
		in.Count = scanCountMax
	}
	in.Match = strings.TrimSpace(in.Match)
	client, err := s.dialFor(ctx, inst, dbNum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	cursor := in.Cursor
	keys := make([]string, 0, in.Count)
	seen := make(map[string]struct{}, in.Count)
	for {
		next, batch, err := client.Scan(ctx, cursor, in.Match, int64(in.Count-len(keys)))
		if err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		for _, k := range batch {
			if _, dup := seen[k]; dup {
				continue // SCAN 允许重复返回同一键,页内去重保序
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
			if len(keys) >= in.Count {
				break
			}
		}
		cursor = next
		if cursor == 0 || len(keys) >= in.Count {
			break
		}
	}
	return &ScanResult{Keys: keys, NextCursor: cursor}, nil
}

// ---- 单键详情 ----

// KeyDetail 单键详情:TYPE → TTL → MEMORY USAGE(尽力而为)→ 按类型值预览(截断)。
// 键不存在 → ErrKeyNotFound。
func (s *service) KeyDetail(ctx context.Context, id string, dbNum int, key string) (*KeyDetail, error) {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if key == "" {
		return nil, fmt.Errorf("%w: empty key", ErrInvalidInput)
	}
	client, err := s.dialFor(ctx, inst, dbNum)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()
	return keyDetailOf(ctx, client, key)
}

// keyDetailOf 在已建立的客户端上取单键详情(与实例解耦,便于单测)。
func keyDetailOf(ctx context.Context, client redisClient, key string) (*KeyDetail, error) {
	typ, err := client.Type(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
	}
	if typ == "none" {
		return nil, ErrKeyNotFound
	}
	detail := &KeyDetail{Key: key, Type: typ}
	if d, err := client.TTL(ctx, key); err == nil {
		switch {
		case d == -2*time.Second:
			detail.TTLSeconds = -2 // 瞬时竞态:TYPE 后被删
		case d < 0:
			detail.TTLSeconds = -1 // 持久化
		default:
			detail.TTLSeconds = int64(d / time.Second)
		}
	}
	if mem, ok := client.MemoryUsage(ctx, key); ok {
		detail.MemoryBytes = mem
	}
	switch typ {
	case "string":
		detail.StrLen, err = client.StrLen(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		if detail.StrValue, err = client.GetRange(ctx, key, 0, stringPreviewBytes-1); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		detail.Truncated = detail.StrLen > stringPreviewBytes
	case "hash":
		if detail.Len, err = client.HLen(ctx, key); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		if _, flat, err := client.HScan(ctx, key, 0, memberPreview); err == nil {
			detail.HashMembers = flatToHash(flat)
		}
	case "list":
		if detail.Len, err = client.LLen(ctx, key); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		if detail.ListItems, err = client.LRange(ctx, key, 0, memberPreview-1); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
	case "set":
		if detail.Len, err = client.SCard(ctx, key); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		if _, items, err := client.SScan(ctx, key, 0, memberPreview); err == nil {
			detail.SetItems = items
		}
	case "zset":
		if detail.Len, err = client.ZCard(ctx, key); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
		if detail.ZSetItems, err = client.ZRangeWithScores(ctx, key, 0, memberPreview-1); err != nil {
			return nil, fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
		}
	}
	// stream / ReJSON-JSON 等扩展类型:只给类型/TTL/内存元信息,值不解析。
	return detail, nil
}

// flatToHash 把 HSCAN 的扁平 [field,value,...] 切片配对为成员列表。
func flatToHash(flat []string) []HashMember {
	out := make([]HashMember, 0, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		out = append(out, HashMember{Field: flat[i], Value: flat[i+1]})
	}
	return out
}

// ---- 写操作(须已解锁)----

// DeleteKey 删除键(须已解锁;键不存在 → ErrKeyNotFound)。
func (s *service) DeleteKey(ctx context.Context, id string, dbNum int, key string) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidInput)
	}
	client, err := s.dialFor(ctx, inst, dbNum)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	n, err := client.Del(ctx, key)
	if err != nil {
		return fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
	}
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

// SetTTL 设置/清除过期(ttlSeconds >= 0 = EXPIRE;-1 = PERSIST 持久化;须已解锁)。
func (s *service) SetTTL(ctx context.Context, id string, dbNum int, key string, ttlSeconds int64) error {
	inst, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if err := ensureWritable(inst); err != nil {
		return err
	}
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalidInput)
	}
	if ttlSeconds < -1 {
		return fmt.Errorf("%w: ttl %d", ErrInvalidInput, ttlSeconds)
	}
	client, err := s.dialFor(ctx, inst, dbNum)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	var ok bool
	if ttlSeconds >= 0 {
		ok, err = client.Expire(ctx, key, time.Duration(ttlSeconds)*time.Second)
	} else {
		ok, err = client.Persist(ctx, key)
	}
	if err != nil {
		return fmt.Errorf("%w:%s", ErrConnectFailed, describeRedisError(err))
	}
	if !ok {
		return ErrKeyNotFound
	}
	return nil
}
