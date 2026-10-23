// compact.go 实现上下文预算管理:发送前全量 token 估算 → 达阈值把旧会话历史
// 压缩为摘要 checkpoint(模型生成,保留最近 KeepRecentTokens 原样)→ run 内
// 工具轮次按 token 预算确定性裁剪 → provider 溢出错误触发被动压缩重试一次。
//
// 参考 OpenCode summary 模式:checkpoint 之后的历史才进模型上下文(contextMessages),
// 展示层永远保留完整记录;摘要失败静默降级为确定性裁剪,绝不阻塞主流程。
package opschat

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/huangchengsir/pipewright/internal/tokenest"
)

// compactMaxFailures 是模型压缩连续失败熔断阈值(仅本会话;成功即清零)。
const compactMaxFailures = 3

// estimateRequestTokens 估算一次 AgentRequest 的完整输入 tokens(system +
// question + history + tools schema + 全部轮次回灌)。
func estimateRequestTokens(req AgentRequest) int {
	total := tokenest.Estimate(req.System) + tokenest.Estimate(req.Question)
	for _, m := range req.History {
		total += tokenest.Estimate(m.Text) + 4
	}
	for _, t := range req.Tools {
		total += tokenest.Estimate(t.ID) + tokenest.Estimate(string(t.Schema)) + 8
	}
	for _, ex := range req.Rounds {
		for _, c := range ex.Calls {
			total += tokenest.Estimate(c.ToolID) + tokenest.Estimate(string(c.Args)) + 8
		}
		for _, r := range ex.Results {
			total += tokenest.Estimate(r.Output) + tokenest.Estimate(r.DenialReason) + 8
		}
	}
	return total
}

// maybeCompact 在发送前执行预算管理:估算超阈值时,先把 req.History 的旧部分
// 压缩为 checkpoint(模型;成功则落库并改写 req.History),随后(或模型压缩
// 不可用/失败时)对 req.Rounds 做确定性裁剪。返回是否写入了 checkpoint。
func (s *Service) maybeCompact(ctx context.Context, sessionID string, req *AgentRequest, reason string) bool {
	policy := s.policy()
	if estimateRequestTokens(*req) < policy.Ceiling() {
		return false
	}
	checkpointed := false
	if (policy.AutoCompact == nil || *policy.AutoCompact) && len(req.History) > 2 {
		if s.compactHistory(ctx, sessionID, req) {
			checkpointed = true
			// 压缩后仍超(主要来自本轮 rounds):继续走确定性裁剪。
		}
	}
	trimRounds(req, policy, reason)
	return checkpointed
}

// compactTarget 计算压缩后的目标 tokens:overflow 被动压缩按阈值的 70% 收缩
// (OpenCode SHRINK_STEPS 首档);主动压缩以触发阈值为目标即可。
func compactTarget(policy ContextPolicy, reason string) int {
	target := policy.Ceiling()
	if reason == "overflow" {
		target = target * 7 / 10
	}
	return target
}

// compactHistory 把 req.History 分割为「保留最近 KeepRecentTokens」与「待摘要
// older」,用模型把 older 压成结构化摘要并落库 checkpoint;成功后改写
// req.History = [checkpoint] + recent。req.History 以既有 checkpoint 开头时走合并模式。
func (s *Service) compactHistory(ctx context.Context, sessionID string, req *AgentRequest) bool {
	compacter, ok := s.model.(Compacter)
	if !ok || s.compactBroken(sessionID) {
		return false
	}
	policy := s.policy()
	recent, older := splitKeepRecent(req.History, policy.KeepRecentTokens)
	if len(older) == 0 {
		// 没有可压缩的旧历史(全是最近保留段):交由 rounds 裁剪。
		return false
	}
	prior := ""
	if strings.Contains(older[0].Text, "<conversation-checkpoint>") {
		prior = older[0].Text
		older = older[1:]
		if len(older) == 0 {
			return false
		}
	}
	// 摘要也是一次模型调用:同样受 ModelTimeout 约束,失败绝不拖垮主流程。
	sumCtx, stop := context.WithTimeout(ctx, s.runtime.ModelTimeout)
	defer stop()
	summary, err := compacter.Summarize(sumCtx, SummarizeRequest{
		Provider:         req.Provider,
		System:           "You are a precise operations-session summarizer.",
		Prompt:           compactSummaryPrompt(prior, flattenMessages(older)),
		MaxOutputTokens:  min(policy.MaxOutputTokens, 8_192),
		MaxResponseBytes: 16 << 10,
	})
	if err != nil {
		s.noteCompactFailure(sessionID, err)
		return false
	}
	summary = strings.TrimSpace(s.scrub(summary, 16<<10))
	if summary == "" {
		s.noteCompactFailure(sessionID, fmt.Errorf("empty summary"))
		return false
	}
	// checkpoint 落库:visible(前端分界条)+ context_allowed(下轮起读)。
	if err = s.persist(sessionID, func(c context.Context, tx *sql.Tx) error {
		return s.append(c, tx, Entry{SessionID: sessionID, Kind: KindCheckpoint, Text: summary, Status: "compacted"}, true, true)
	}); err != nil {
		s.noteCompactFailure(sessionID, err)
		return false
	}
	s.compactOK(sessionID)
	// 改写当前请求:checkpoint(user 角色 + 包裹说明) + 保留段。
	wrapped := "<conversation-checkpoint>\n" + summary + "\n</conversation-checkpoint>"
	req.History = append([]Message{{Role: "user", Text: wrapped}}, recent...)
	log.Printf("[opschat] context compacted session=%s older=%d recent=%d est=%d", sessionID, len(older), len(recent), estimateRequestTokens(*req))
	return true
}

// splitKeepRecent 从最新往回按 tokens 累计保留段,边界对齐 user 消息(user 的
// 提问与其后的回复不拆开);返回 (recent, older),均保持时间正序。
func splitKeepRecent(messages []Message, keepTokens int) (recent, older []Message) {
	acc := 0
	for i := len(messages) - 1; i >= 0; i-- {
		acc += tokenest.Estimate(messages[i].Text) + 4
		if acc >= keepTokens {
			// 超预算位置向前推进到最近的 user 边界:保留段以 user 提问开头,
			// older 不以孤立的 assistant 回答结尾。
			split := i
			for split > 0 && messages[split].Role != "user" {
				split--
			}
			if split == 0 {
				return []Message{}, messages
			}
			return messages[split:], messages[:split]
		}
	}
	return messages, nil
}

// flattenMessages 把 older 历史拍平为 transcript 文本(摘要 prompt 的输入):
// user/assistant 原文(截 4000 rune)。
func flattenMessages(older []Message) string {
	var b strings.Builder
	for _, m := range older {
		role := "[Assistant]"
		if m.Role == "user" {
			role = "[User]"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", role, clipRunes(m.Text, 4000))
	}
	return strings.TrimSpace(b.String())
}

// compactSummaryPrompt 构造结构化摘要指令(固定小节;有旧 checkpoint 时合并)。
func compactSummaryPrompt(prior, transcript string) string {
	var b strings.Builder
	if prior != "" {
		b.WriteString("Merge the previous checkpoint and the new transcript into ONE updated summary. New information takes precedence over the checkpoint.\n\nPrevious checkpoint:\n")
		b.WriteString(prior)
		b.WriteString("\n\n")
	} else {
		b.WriteString("Summarize the conversation below for another agent instance that will continue this operations session.\n\n")
	}
	b.WriteString(`Produce a markdown summary with exactly these sections:
## 目标 (Objective)
## 关键事实与决定 (Key facts and decisions)
## 已完成 (Completed)
## 进行中 (Active)
## 受阻塞 (Blocked)
## 下一步建议 (Suggested next steps)
## 重要上下文 (Important context)

Rules: one bullet per line; keep exact server/container names, paths, commands, error strings and identifiers; keep unresolved user questions; state scope honestly — unknown stays unknown; never invent results. Do not mention the summarization itself.

Transcript:
`)
	b.WriteString(transcript)
	return b.String()
}

// trimRounds 对本 run 的轮次回灌做确定性裁剪:从最旧轮开始整轮丢弃,直到
// 估算降到目标以下(或只剩最后一轮);丢弃规模以一条注入 System 的说明呈现,
// 模型仍知道「更早轮次存在但被省略」。
func trimRounds(req *AgentRequest, policy ContextPolicy, reason string) {
	if len(req.Rounds) <= 1 {
		return
	}
	target := compactTarget(policy, reason)
	dropped := 0
	for estimateRequestTokens(*req) > target && len(req.Rounds) > 1 {
		req.Rounds = req.Rounds[1:]
		dropped++
	}
	if dropped > 0 {
		note := fmt.Sprintf("\n\nContext budget note: %d earliest tool-call round(s) of this run were omitted to fit the context window; earlier conclusions still hold — do not redo settled work.", dropped)
		req.System = strings.Split(req.System, "\n\nContext budget note")[0] + note
	}
}

// compactBroken 报告该会话的模型压缩是否已熔断。
func (s *Service) compactBroken(sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactFails[sessionID] >= compactMaxFailures
}

func (s *Service) noteCompactFailure(sessionID string, err error) {
	s.mu.Lock()
	s.compactFails[sessionID]++
	count := s.compactFails[sessionID]
	s.mu.Unlock()
	log.Printf("[opschat] context compact failed session=%s count=%d err=%v", sessionID, count, err)
}

func (s *Service) compactOK(sessionID string) {
	s.mu.Lock()
	delete(s.compactFails, sessionID)
	s.mu.Unlock()
}

// clipRunes 按 rune 上限截断(保 UTF-8 完整)。
func clipRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + " [truncated]"
}
