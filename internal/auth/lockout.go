package auth

import (
	"sync"
	"time"
)

// 锁定参数(AC3 / NFR-5):
//   - 按账户:同一用户名连续失败 LockoutThreshold 次 → 该账户锁定 LockoutDuration;
//   - 按 IP:同一来源 IP 累计失败 ipLockoutThreshold 次 → 该 IP 锁定 LockoutDuration,
//     防单 IP 轮换用户名绕过账户阈值。
//
// 分桶取代旧版单一全局计数器:垃圾用户名的失败只落在自己的桶里,不再锁死全平台
// (修复全局计数器可被任何人用 5 次失败触发的平台级登录 DoS)。
const (
	LockoutThreshold   = 5
	ipLockoutThreshold = 10
	LockoutDuration    = 15 * time.Minute

	// lockoutEntryTTL 是无新失败条目的最长保留时长,超时由惰性 GC 清除。
	// 仅释放内存,不改变锁定语义——计数在成功登录或锁定到期前不会自然衰减。
	lockoutEntryTTL = 24 * time.Hour
	// lockoutMaxEntries 是分桶数上限(防内存无界):超限先清过期条目,仍超则整体重置
	// (海量来源 IP 的极端情形;此时攻击者获得重新计数的机会,属容量兜底取舍)。
	lockoutMaxEntries = 100_000
)

// Clock 允许测试注入可控时钟(避免真实等待 15 分钟)。
type Clock interface {
	Now() time.Time
}

// RealClock 使用系统时间。
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

// lockoutKey 是一个锁定桶:name 唯一标识桶,threshold 为触发锁定的失败次数,
// resetOnSuccess 表示认证成功后是否清零(账户桶清零;IP 桶不清零,防攻击者用
// 已知账号穿插成功登录来重置自己 IP 的失败计数)。
type lockoutKey struct {
	name           string
	threshold      int
	resetOnSuccess bool
}

// userLockoutKey 构造账户桶:key 于提交的用户名(无论是否命中真实账户,无存在性侧信道)。
func userLockoutKey(username string) lockoutKey {
	return lockoutKey{name: "user:" + username, threshold: LockoutThreshold, resetOnSuccess: true}
}

// ipLockoutKey 构造来源 IP 桶。
func ipLockoutKey(ip string) lockoutKey {
	return lockoutKey{name: "ip:" + ip, threshold: ipLockoutThreshold, resetOnSuccess: false}
}

// pwChangeLockoutKey 构造改密节流桶(按角色+用户标识;管理员 userID 为空串)。
func pwChangeLockoutKey(role, userID string) lockoutKey {
	return lockoutKey{name: "pwchange:" + role + ":" + userID, threshold: LockoutThreshold, resetOnSuccess: true}
}

// GuardMFA 对 TOTP 验证码尝试做失败节流(供 mfa 包使用;桶按 owner 分隔,
// 阈值/锁定时长与登录口令一致,成功即清零)。
func (lm *LockoutManager) GuardMFA(owner string, authenticate func() (bool, error)) (ok bool, locked bool, remaining time.Duration, err error) {
	return lm.Guard([]lockoutKey{{name: "mfa:" + owner, threshold: LockoutThreshold, resetOnSuccess: true}}, authenticate)
}

// attemptRecord 是单个桶的失败计数与锁定状态。
type attemptRecord struct {
	failCount   int
	lockedUntil time.Time
	lastFailAt  time.Time
}

// LockoutManager 管理登录/改密的失败计数与锁定状态(按 key 分桶)。
// 单管理员 + 少量成员场景下桶数天然有限;IP 桶数由 lockoutMaxEntries 兜底。
//
// 注:计数为内存态,进程重启后清零(攻击者能重启进程则有更大攻击面,不做持久化)。
type LockoutManager struct {
	mu      sync.Mutex
	entries map[string]*attemptRecord
	clock   Clock
}

// NewLockoutManager 构造 LockoutManager;clock 为 nil 时使用 RealClock。
func NewLockoutManager(clock Clock) *LockoutManager {
	if clock == nil {
		clock = RealClock{}
	}
	return &LockoutManager{entries: make(map[string]*attemptRecord), clock: clock}
}

// Guard 在单次持锁内完成「检查锁定 → 认证 → 据结果记失败/重置」的复合操作,
// 消除检查与记录之间的 TOCTOU 窗口,保证并发下阈值语义可靠。
//
// authenticate 在持锁状态下被调用(含 argon2 重算),返回 (ok, err):
//   - 任一 key 当前锁定:不调用 authenticate,返回 locked=true(remaining 为最长剩余);
//   - err != nil:不计为失败(视为系统故障,原样返回);
//   - ok==true:重置所有 resetOnSuccess 的桶;
//   - ok==false:向所有 key 记一次失败(各自按自己的阈值判定是否触发锁定)。
func (lm *LockoutManager) Guard(keys []lockoutKey, authenticate func() (bool, error)) (ok bool, locked bool, remaining time.Duration, err error) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	now := lm.clock.Now()
	lm.gcLocked(now)

	// 检查任一桶是否锁定(取最长剩余);锁定已到期的桶就地清除(计数随之归零)。
	for _, k := range keys {
		rec := lm.entries[k.name]
		if rec == nil {
			continue
		}
		if rec.lockedUntil.IsZero() {
			continue
		}
		if now.Before(rec.lockedUntil) {
			if rem := rec.lockedUntil.Sub(now); rem > remaining {
				remaining = rem
			}
			continue
		}
		delete(lm.entries, k.name)
	}
	if remaining > 0 {
		return false, true, remaining, nil
	}

	ok, err = authenticate()
	if err != nil {
		return false, false, 0, err
	}
	if ok {
		for _, k := range keys {
			if k.resetOnSuccess {
				delete(lm.entries, k.name)
			}
		}
		return true, false, 0, nil
	}
	for _, k := range keys {
		lm.recordFailureLocked(k, now)
	}
	return false, false, 0, nil
}

// recordFailureLocked 向桶 k 记一次失败;达到阈值则锁定。要求调用方已持锁。
func (lm *LockoutManager) recordFailureLocked(k lockoutKey, now time.Time) {
	rec := lm.entries[k.name]
	if rec == nil {
		rec = &attemptRecord{}
		lm.entries[k.name] = rec
	}
	rec.failCount++
	rec.lastFailAt = now
	if rec.failCount >= k.threshold {
		rec.lockedUntil = now.Add(LockoutDuration)
	}
}

// gcLocked 惰性清理:未锁定且超过 lockoutEntryTTL 无新失败的条目。要求调用方已持锁。
func (lm *LockoutManager) gcLocked(now time.Time) {
	for name, rec := range lm.entries {
		unlocked := rec.lockedUntil.IsZero() || !now.Before(rec.lockedUntil)
		if unlocked && now.Sub(rec.lastFailAt) >= lockoutEntryTTL {
			delete(lm.entries, name)
		}
	}
	if len(lm.entries) >= lockoutMaxEntries {
		lm.entries = make(map[string]*attemptRecord)
	}
}
