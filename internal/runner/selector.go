package runner

// selector.go 是「标签选择器」的纯函数层(FR-8-19 多节点构建机池):
// 把"项目/stage 要在什么样的机器上构建"表达为一小段声明式表达式,由调度器(scheduler.go)
// 拿它去匹配 servers.labels 打了标签的构建机池。
//
// 语法(刻意极简,AND 语义,无 OR/取反/正则):
//   - 空串          = 本地构建(不是选择器;调度器不介入)。
//   - `server:<id>` = 钉死单机(id 为服务器 uuid)。旧 project_runners.runner_server_id
//                     的规范形式,迁移 0052 回填的就是它。
//   - `peer:<name>` = 集群共享构建机限定项(0086;≤1 个):其余项在对端「name 或 nodeID
//                     命中的环境」的共享机构建机目录里匹配,如 `peer:prod,linux`。
//                     可与 `server:<id>` 组合钉死对端某台共享机。含 peer: 项的选择器由
//                     执行层先行分流(走 mesh 共享构建路径),本地调度器永不接收;
//                     本地标签匹配对 `peer:x` 项天然不命中(labels 字符集无 ':'),作双保险。
//   - 标签项列表     = 逗号分隔,全部项命中才匹配,如 `linux,arch=arm64,gpu`。
//       · 纯 tag 项:`linux` —— 只匹配服务器标签里的字面 `linux`(不匹配 `os=linux`,
//         严格逐项相等,不猜语义)。
//       · k=v 项:`arch=arm64` —— 匹配服务器标签里的字面 `arch=arm64`。
//   - 项字符集:字母数字开头,可含 `.` `_` `-`;k、v 同字符集(peer 名放宽:非空、无
//     逗号/空白、≤64)。项数 ≤16,总长 ≤255。
//     边界在 ParseSelector 里统一裁决,API 层据此回 422,阶段日志据此报"选择器非法"。

import (
	"errors"
	"strings"
)

// ErrInvalidSelector 表示选择器表达式语法非法(供 API 层映射 422 / 阶段日志明示)。
var ErrInvalidSelector = errors.New("runner: invalid selector")

// selectorTermMax / selectorLenMax 是选择器的规模上限(防误贴长文本;标签匹配是精确比较,无需更长)。
const (
	selectorTermMax = 16
	selectorLenMax  = 255
	labelLenMax     = 512 // servers.labels 列宽(mysql VARCHAR(512))对齐
	// peerNameMax 是 `peer:<name>` 项里 name 的长度上限(对端环境名/nodeID 均远短于此)。
	peerNameMax = 64
)

// termOK 判断一个标签项(tag 或 k=v 的键/值)是否合法:字母数字开头,可含 . _ -。
func termOK(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// PinnedServer 识别 `server:<id>` 钉死单机形式:是则返回 (id, true)。
// 注意:含 peer: 项的选择器(如 `peer:prod,server:<id>`)不是纯钉死形式,本函数返回
// false —— 钉死对端共享机的解析归 SplitPeerSelector。
func PinnedServer(selector string) (string, bool) {
	s := strings.TrimSpace(selector)
	id, ok := strings.CutPrefix(s, "server:")
	if !ok || !termOK(id) {
		return "", false
	}
	return id, true
}

// peerTermPrefix 是集群共享构建机限定项的前缀。
const peerTermPrefix = "peer:"

// peerNameOK 校验 `peer:<name>` 的 name 段:非空、≤64、不含逗号/空白(逗号是项分隔符;
// 空白在项级被 Trim,内部空白会与对端名的精确匹配产生歧义,直接拒绝)。
func peerNameOK(s string) bool {
	if s == "" || len(s) > peerNameMax {
		return false
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return false
	}
	return true
}

// SplitPeerSelector 拆解含 `peer:<环境名|nodeID>` 项的选择器(集群共享构建机,0086):
// 返回 (peer 名, 其余项重组的选择器串, 是否含 peer 项, 错误)。选择器非法或含 >1 个
// peer 项 → ErrInvalidSelector。不含 peer 项时 ok=false,rest 为规范化后的原选择器
// (项序保持;非法选择器同样报错,调用方不必再跑一次 ParseSelector)。
func SplitPeerSelector(selector string) (peer, rest string, ok bool, err error) {
	terms, perr := ParseSelector(selector)
	if perr != nil {
		return "", "", false, perr
	}
	others := make([]string, 0, len(terms))
	for _, t := range terms {
		if name, isPeer := strings.CutPrefix(t, peerTermPrefix); isPeer {
			if ok {
				return "", "", false, ErrInvalidSelector // 两个 peer: 项 = 环境限定歧义
			}
			peer, ok = name, true
			continue
		}
		others = append(others, t)
	}
	return peer, strings.Join(others, ","), ok, nil
}

// ParseSelector 解析选择器表达式为规范化项集合(保序去重)。项有三类:普通标签项
// (tag/k=v)、`peer:<name>` 集群共享构建机限定项(≤1 个,0086)、组合形式里的
// `server:<id>` 钉死项(`peer:prod,server:<id>`;纯钉死选择器仍由 PinnedServer 先行
// 处理,不经本函数的返回值消费)。空串返回 (nil, nil) = 本地构建。
// 非法项(字符集/规模/双 peer)→ ErrInvalidSelector。
func ParseSelector(selector string) ([]string, error) {
	s := strings.TrimSpace(selector)
	if s == "" {
		return nil, nil
	}
	if len(s) > selectorLenMax {
		return nil, ErrInvalidSelector
	}
	parts := strings.Split(s, ",")
	seen := make(map[string]struct{}, len(parts))
	terms := make([]string, 0, len(parts))
	peerSeen := false
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, ErrInvalidSelector // `a,,b` 视为笔误而非静默忽略
		}
		switch {
		case strings.HasPrefix(p, peerTermPrefix):
			if peerSeen || !peerNameOK(strings.TrimPrefix(p, peerTermPrefix)) {
				return nil, ErrInvalidSelector
			}
			peerSeen = true
		case strings.HasPrefix(p, "server:"):
			if !termOK(strings.TrimPrefix(p, "server:")) {
				return nil, ErrInvalidSelector
			}
		default:
			if k, v, isKV := strings.Cut(p, "="); isKV {
				if !termOK(k) || !termOK(v) {
					return nil, ErrInvalidSelector
				}
			} else if !termOK(p) {
				return nil, ErrInvalidSelector
			}
		}
		if _, dup := seen[p]; !dup {
			seen[p] = struct{}{}
			terms = append(terms, p)
		}
	}
	if len(terms) > selectorTermMax {
		return nil, ErrInvalidSelector
	}
	return terms, nil
}

// ValidateSelector 校验选择器整体合法(标签项形式或 server:<id> 钉死形式);空串合法(=本地)。
func ValidateSelector(selector string) error {
	if _, ok := PinnedServer(selector); ok {
		return nil
	}
	_, err := ParseSelector(selector)
	return err
}

// ValidateTerm 校验**单个**标签项(标签登记处建标签用):与选择器项同一字符集与长度约束。
// 纯 tag(`linux`)与 k=v(`arch=arm64`)均合法;空白/非法字符/超长 → ErrInvalidSelector。
func ValidateTerm(term string) error {
	term = strings.TrimSpace(term)
	if term == "" || len(term) > labelLenMax {
		return ErrInvalidSelector
	}
	if k, v, isKV := strings.Cut(term, "="); isKV {
		if !termOK(k) || !termOK(v) {
			return ErrInvalidSelector
		}
		return nil
	}
	if !termOK(term) {
		return ErrInvalidSelector
	}
	return nil
}

// ValidateLabels 校验 servers.labels 原始串(写入侧用):逐项与选择器同一字符集规则,
// 规模上限 32 项 / 512 字符(与 mysql 列宽对齐)。空串合法(= 不参与构建机池)。坏项 → ErrInvalidSelector。
func ValidateLabels(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if len(raw) > labelLenMax {
		return ErrInvalidSelector
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 32 {
		return ErrInvalidSelector
	}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if k, v, isKV := strings.Cut(p, "="); isKV {
			if !termOK(k) || !termOK(v) {
				return ErrInvalidSelector
			}
			continue
		}
		if !termOK(p) {
			return ErrInvalidSelector
		}
	}
	return nil
}

// ParseLabels 解析 servers.labels 原始串(逗号分隔)为标签集合;坏项静默剔出
// (labels 是管理员手填的存量数据,不该让一个坏项打死整池调度;写入侧 httpapi 已校验)。
// 超过 labelLenMax 由列宽兜底,此处不重复裁决。
func ParseLabels(raw string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			set[p] = struct{}{}
		}
	}
	return set
}

// MatchSelector 判断服务器标签集是否命中选择器全部项(AND 语义)。
// 空标签集不匹配任何非空选择器 → 未打标签的服务器永远不会被选为构建机。
func MatchSelector(labelsRaw string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	set := ParseLabels(labelsRaw)
	for _, t := range terms {
		if _, ok := set[t]; !ok {
			return false
		}
	}
	return true
}

// MatchSelectorAny 判断服务器标签集是否命中选择器**任一**项(OR 语义)。
// 供部署/健康检查节点的「匹配方式 = 满足任一条件」消费;构建机池仍只用 AND 的 MatchSelector。
// 与 MatchSelector 同约束:空标签集不匹配任何非空选择器。
func MatchSelectorAny(labelsRaw string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	set := ParseLabels(labelsRaw)
	for _, t := range terms {
		if _, ok := set[t]; ok {
			return true
		}
	}
	return false
}
