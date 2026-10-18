package redisadmin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisClient 抽象键管理所需的最小 Redis 命令面(注入便于单测,不触真网络)。
// 返回值均为已解包的普通值(把 go-redis 的 *xCmd 收口在 liveClient 一处)。
type redisClient interface {
	Ping(ctx context.Context) error
	// InfoSection 返回 INFO 指定节的扁平 k/v(# 注释行剔除;keyspace 节由调用方单独解析)。
	InfoSection(ctx context.Context, sections ...string) (string, error)
	DBSize(ctx context.Context) (int64, error)
	Scan(ctx context.Context, cursor uint64, match string, count int64) (uint64, []string, error)
	Type(ctx context.Context, key string) (string, error)
	TTL(ctx context.Context, key string) (time.Duration, error)
	// MemoryUsage 返回 MEMORY USAGE 字节;第二返回值 false = 服务端不支持/键不存在。
	MemoryUsage(ctx context.Context, key string) (int64, bool)
	GetRange(ctx context.Context, key string, start, end int64) (string, error)
	StrLen(ctx context.Context, key string) (int64, error)
	HLen(ctx context.Context, key string) (int64, error)
	// HScan 返回一页 field/value 扁平切片(长度为偶数)。
	HScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error)
	LLen(ctx context.Context, key string) (int64, error)
	LRange(ctx context.Context, key string, start, stop int64) ([]string, error)
	SCard(ctx context.Context, key string) (int64, error)
	SScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error)
	ZCard(ctx context.Context, key string) (int64, error)
	// ZRangeWithScores 返回有序集成员+分数(升序)。
	ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]ZEntry, error)
	Del(ctx context.Context, key string) (int64, error)
	// Expire 设置过期;键不存在返回 false。
	Expire(ctx context.Context, key string, ttl time.Duration) (bool, error)
	Persist(ctx context.Context, key string) (bool, error)
	Close() error
}

// ZEntry 是有序集的一个成员(成员 + 分数)。
type ZEntry struct {
	Member string
	Score  float64
}

// 连接时限:管理面快速失败,不长时间占用请求。
const (
	dialTimeout = 3 * time.Second
	ioTimeout   = 5 * time.Second
)

// dialFunc 按接入参数造一个 redisClient(注入便于单测;生产用 prodDial)。
type dialFunc func(host string, port int, password string, dbNum int) (redisClient, error)

// prodDial 构造真实 go-redis 客户端(不重试:管理操作快速失败,错误原样给人话映射)。
func prodDial(host string, port int, password string, dbNum int) (redisClient, error) {
	cli := redis.NewClient(&redis.Options{
		Addr:         hostPort(host, port),
		Password:     password,
		DB:           dbNum,
		DialTimeout:  dialTimeout,
		ReadTimeout:  ioTimeout,
		WriteTimeout: ioTimeout,
		MaxRetries:   -1,
	})
	return &liveClient{cli: cli}, nil
}

// liveClient 把 *redis.Client 适配为 redisClient(cmd 解包收口于此)。
type liveClient struct {
	cli *redis.Client
}

func (c *liveClient) Ping(ctx context.Context) error {
	return c.cli.Ping(ctx).Err()
}

func (c *liveClient) InfoSection(ctx context.Context, sections ...string) (string, error) {
	return c.cli.Info(ctx, sections...).Result()
}

func (c *liveClient) DBSize(ctx context.Context) (int64, error) {
	return c.cli.DBSize(ctx).Result()
}

func (c *liveClient) Scan(ctx context.Context, cursor uint64, match string, count int64) (uint64, []string, error) {
	keys, next, err := c.cli.Scan(ctx, cursor, match, count).Result()
	return next, keys, err
}

func (c *liveClient) Type(ctx context.Context, key string) (string, error) {
	return c.cli.Type(ctx, key).Result()
}

func (c *liveClient) TTL(ctx context.Context, key string) (time.Duration, error) {
	return c.cli.TTL(ctx, key).Result()
}

func (c *liveClient) MemoryUsage(ctx context.Context, key string) (int64, bool) {
	v, err := c.cli.MemoryUsage(ctx, key).Result()
	if err != nil {
		return 0, false // 不支持(<7.0)或键刚被删:尽力而为,不阻断详情
	}
	return v, true
}

func (c *liveClient) GetRange(ctx context.Context, key string, start, end int64) (string, error) {
	return c.cli.GetRange(ctx, key, start, end).Result()
}

func (c *liveClient) StrLen(ctx context.Context, key string) (int64, error) {
	return c.cli.StrLen(ctx, key).Result()
}

func (c *liveClient) HLen(ctx context.Context, key string) (int64, error) {
	return c.cli.HLen(ctx, key).Result()
}

func (c *liveClient) HScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error) {
	keys, next, err := c.cli.HScan(ctx, key, cursor, "", count).Result()
	return next, keys, err
}

func (c *liveClient) LLen(ctx context.Context, key string) (int64, error) {
	return c.cli.LLen(ctx, key).Result()
}

func (c *liveClient) LRange(ctx context.Context, key string, start, stop int64) ([]string, error) {
	return c.cli.LRange(ctx, key, start, stop).Result()
}

func (c *liveClient) SCard(ctx context.Context, key string) (int64, error) {
	return c.cli.SCard(ctx, key).Result()
}

func (c *liveClient) SScan(ctx context.Context, key string, cursor uint64, count int64) (uint64, []string, error) {
	keys, next, err := c.cli.SScan(ctx, key, cursor, "", count).Result()
	return next, keys, err
}

func (c *liveClient) ZCard(ctx context.Context, key string) (int64, error) {
	return c.cli.ZCard(ctx, key).Result()
}

func (c *liveClient) ZRangeWithScores(ctx context.Context, key string, start, stop int64) ([]ZEntry, error) {
	zs, err := c.cli.ZRangeWithScores(ctx, key, start, stop).Result()
	if err != nil {
		return nil, err
	}
	out := make([]ZEntry, 0, len(zs))
	for _, z := range zs {
		out = append(out, ZEntry{Member: toString(z.Member), Score: z.Score})
	}
	return out, nil
}

func (c *liveClient) Del(ctx context.Context, key string) (int64, error) {
	return c.cli.Del(ctx, key).Result()
}

func (c *liveClient) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	return c.cli.Expire(ctx, key, ttl).Result()
}

func (c *liveClient) Persist(ctx context.Context, key string) (bool, error) {
	return c.cli.Persist(ctx, key).Result()
}

func (c *liveClient) Close() error {
	return c.cli.Close()
}

// toString 把 redis 命令返回的成员(interface{})渲染为字符串(string 直取,数字按字面)。
func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", x)
	}
}

// describeRedisError 把 go-redis / 网络错误渲染为人话(绝不含密码)。
func describeRedisError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "连接 Redis 超时"
	}
	var nerr net.Error
	if errors.As(err, &nerr) {
		return "无法连接 Redis(网络异常或地址不可达)"
	}
	var rerr redis.Error // 服务端返回的原文(如 WRONGPASS/NOAUTH),已无密码信息
	if errors.As(err, &rerr) {
		return rerr.Error()
	}
	msg := err.Error()
	if strings.Contains(msg, "WRONGPASS") || strings.Contains(msg, "NOAUTH") {
		return "认证失败:密码错误或服务端要求密码"
	}
	if strings.Contains(msg, "connection refused") {
		return "无法连接 Redis(连接被拒绝)"
	}
	return "Redis 操作失败"
}
