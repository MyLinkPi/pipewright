package runner

// local_roles_test.go 覆盖本机(控制节点)角色的 runner 侧语义(specs/local-node-roles.md 4.6/5.1):
//   - 选择器豁免:`server:__local__` 合法(纯钉死与组合形式),其余非法 id/标签项规则不变;
//   - SaveSelector 存在性转译:target.ErrLocalRoleDisabled → ErrLocalRoleDisabled(不再伪装
//     「服务器不存在」),false+nil → 既有 ErrServerNotFound;
//   - 调度器本机候选:恒注册提供者的吞噬规则(标签路径静默、钉死本机上抛)、判定顺序
//     (角色检查先于 pve/shared)、启用态全套调度语义。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/huangchengsir/pipewright/internal/store"
	"github.com/huangchengsir/pipewright/internal/storetest"
	"github.com/huangchengsir/pipewright/internal/target"
)

// --- 选择器豁免 ---

func TestPinnedServerLocalExemption(t *testing.T) {
	if id, ok := PinnedServer("server:" + target.LocalHostID); !ok || id != target.LocalHostID {
		t.Fatalf("server:__local__ 应为合法钉死形式,得 (%q, %v)", id, ok)
	}
	if _, ok := PinnedServer("server:__bad__"); ok {
		t.Fatal("其他下划线开头 id 不应被豁免")
	}
	if _, ok := PinnedServer("server:hello"); !ok {
		t.Fatal("常规 id 应照常合法")
	}
	// 组合形式:ParseSelector 对 server: 项同样豁免;标签项规则不变。
	terms, err := ParseSelector("linux,server:" + target.LocalHostID)
	if err != nil || len(terms) != 2 {
		t.Fatalf("组合豁免失败:%v, %v", terms, err)
	}
	if _, err := ParseSelector("linux,_bad"); !errors.Is(err, ErrInvalidSelector) {
		t.Fatal("标签项字符集规则不得放宽")
	}
	if err := ValidateSelector("server:" + target.LocalHostID); err != nil {
		t.Fatalf("ValidateSelector 应放行本机钉死:%v", err)
	}
}

// --- SaveSelector 转译(决策 B)---

type fakeErrExister struct{ err error }

func (f fakeErrExister) Exists(context.Context, string) (bool, error) { return false, f.err }

func TestSaveSelectorTranslatesLocalRoleDisabled(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx := context.Background()
		pid := seedProject(t, st)

		// target.ErrLocalRoleDisabled → runner.ErrLocalRoleDisabled(可读报错)。
		svc := New(st.DB, fakeErrExister{err: target.ErrLocalRoleDisabled})
		_, err := svc.SaveSelector(ctx, pid, "server:"+target.LocalHostID)
		if !errors.Is(err, ErrLocalRoleDisabled) {
			t.Fatalf("应转译为 ErrLocalRoleDisabled,得 %v", err)
		}
		if errors.Is(err, ErrServerNotFound) {
			t.Fatal("不得伪装成「服务器不存在」")
		}

		// false+nil → 既有 ErrServerNotFound(真实不存在机,回归)。
		svc2 := New(st.DB, fakeErrExister{err: nil})
		if _, err := svc2.SaveSelector(ctx, pid, "server:gone-1"); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("无因缺失应 ErrServerNotFound,得 %v", err)
		}
	})
}

// --- 调度器本机候选(恒注册提供者)---

func disabledProvider(ctx context.Context) (*LocalCandidate, error) {
	return nil, ErrLocalRoleDisabled
}

func enabledProvider(ctx context.Context) (*LocalCandidate, error) {
	return &LocalCandidate{ID: target.LocalHostID, Name: "本机(控制节点)", Labels: "local,arch=arm64", MaxBuilds: 2, Priority: 9}, nil
}

func seedServerRow(t *testing.T, st *store.Store, id, labels, kind string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	// servers.credential_id 有 FK:先落一条最小凭据(与 seedProject 同口径;重复插入容忍)。
	_, _ = st.DB.Exec(`INSERT INTO credentials (id,name,type,scope,ciphertext,masked_value,created_at,updated_at) VALUES ('c','c','ssh_key','',X'00','m',?,?)`, now, now)
	if _, err := st.DB.Exec(`INSERT OR IGNORE INTO servers (id,name,host,port,user,credential_id,labels,kind,created_at,updated_at) VALUES (?,?,'h',22,'u','c',?,?,?,?)`,
		id, "srv-"+id, labels, kind, now, now); err != nil {
		t.Fatalf("seed server: %v", err)
	}
}

func TestSchedulerLocalCandidateSwallowRules(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		seedServerRow(t, st, "s1", "linux", "standard")

		// 禁用 + 标签选择器:provider 错误静默吞掉,登记机照常命中(候选集与未注册一致)。
		sched := NewScheduler(st.DB, nil, 1)
		sched.SetLocalCandidateProvider(disabledProvider)
		id, name, release, err := sched.Acquire(ctx, "p1", "linux", nil)
		if err != nil || id != "s1" || name == "" {
			t.Fatalf("禁用态标签选择器应照常调度,得 (%q,%q), %v", id, name, err)
		}
		release()

		// 禁用 + 钉死本机:带因失败(非泛化无命中)。
		_, _, _, err = sched.Acquire(ctx, "p1", "server:"+target.LocalHostID, nil)
		if !errors.Is(err, ErrLocalRoleDisabled) {
			t.Fatalf("钉死本机(禁用)应 ErrLocalRoleDisabled,得 %v", err)
		}

		// 禁用 + 钉死 pve 机:pve 拦截照旧(角色检查只对钉死本机短路)。
		seedServerRow(t, st, "pve1", "", "pve_node")
		if _, _, _, err := sched.Acquire(ctx, "p1", "server:pve1", nil); !errors.Is(err, target.ErrPVENotTarget) {
			t.Fatalf("钉死 pve 机应 ErrPVENotTarget,得 %v", err)
		}

		// 禁用 + AcquireShared 钉死本机:判定顺序 → ErrLocalRoleDisabled(先于 shared 检查)。
		if _, _, _, err := sched.AcquireShared(ctx, "p1", "server:"+target.LocalHostID, nil); !errors.Is(err, ErrLocalRoleDisabled) {
			t.Fatalf("AcquireShared 钉死本机(禁用)应 ErrLocalRoleDisabled,得 %v", err)
		}
	})
}

func TestSchedulerLocalCandidateEnabled(t *testing.T) {
	storetest.ForEachDialect(t, func(t *testing.T, st *store.Store) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()

		// 启用:标签圈选可命中本机;空标签机不被纯标签命中(与登记机同语义)。
		sched := NewScheduler(st.DB, nil, 1)
		sched.SetLocalCandidateProvider(enabledProvider)
		id, name, release, err := sched.Acquire(ctx, "p1", "arch=arm64", nil)
		if err != nil || id != target.LocalHostID || !strings.Contains(name, "本机") {
			t.Fatalf("标签圈选应命中本机,得 (%q,%q), %v", id, name, err)
		}
		release()

		// 启用 + 钉死本机:可用。
		id2, _, release2, err := sched.Acquire(ctx, "p1", "server:"+target.LocalHostID, nil)
		if err != nil || id2 != target.LocalHostID {
			t.Fatalf("钉死本机(启用)应可用,得 (%q), %v", id2, err)
		}
		release2()

		// 启用 + AcquireShared 标签路径:本机未开共享 → 静默排除(零命中)。
		if _, _, _, err := sched.AcquireShared(ctx, "p1", "arch=arm64", nil); !errors.Is(err, ErrNoRunnerMatch) {
			t.Fatalf("AcquireShared 标签路径应零命中(本机未共享),得 %v", err)
		}
		// 启用 + AcquireShared 钉死本机:shared 检查 → ErrServerNotShared(启用态的准确报错)。
		if _, _, _, err := sched.AcquireShared(ctx, "p1", "server:"+target.LocalHostID, nil); !errors.Is(err, ErrServerNotShared) {
			t.Fatalf("AcquireShared 钉死本机(启用)应 ErrServerNotShared,得 %v", err)
		}

		// 槽位语义:本机 max_builds=2,第三次并发 Acquire(另一流水线)应排队至超时。
		idA, _, relA, err := sched.Acquire(ctx, "pA", "local", nil)
		if err != nil || idA != target.LocalHostID {
			t.Fatalf("槽位 A:%v", err)
		}
		defer relA()
		idB, _, relB, err := sched.Acquire(ctx, "pB", "local", nil)
		if err != nil || idB != target.LocalHostID {
			t.Fatalf("槽位 B:%v", err)
		}
		defer relB()
		qctx, qcancel := context.WithTimeout(ctx, 150*time.Millisecond)
		defer qcancel()
		if _, _, _, err := sched.Acquire(qctx, "pC", "local", nil); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("超过本机槽位应排队至超时,得 %v", err)
		}
	})
}
