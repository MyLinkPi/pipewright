// overflow.go:provider「上下文窗口溢出」错误识别(参考 OpenCode provider-error 的
// 正则表 + 排除表)。opschat 据此触发被动压缩 + 重试一次(opschat.OverflowClassifier)。
package ai

import (
	"errors"
	"regexp"
)

var overflowPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)context_length_exceeded`),
	regexp.MustCompile(`(?i)context length exceeded`),
	regexp.MustCompile(`(?i)context_window_exceeded`),
	regexp.MustCompile(`(?i)context window exceeded`),
	regexp.MustCompile(`(?i)model_context_exceeded`),
	regexp.MustCompile(`(?i)model context window exceeded`),
	regexp.MustCompile(`(?i)prompt is too long`),
	regexp.MustCompile(`(?i)prompt too long`),
	regexp.MustCompile(`(?i)maximum context length`),
	regexp.MustCompile(`(?i)maximum context window`),
	regexp.MustCompile(`(?i)exceeds? the context (?:window|length|limit)`),
	regexp.MustCompile(`(?i)exceeds? .{0,40}context window`),
	regexp.MustCompile(`(?i)requested .{0,40}tokens .{0,20}exceed`),
	regexp.MustCompile(`(?i)input.{0,40}exceeds.{0,40}context`),
	regexp.MustCompile(`(?i)reduce.{0,30}prompt.{0,20}length`),
	regexp.MustCompile(`(?i)too many tokens`),
	regexp.MustCompile(`(?i)输入.{0,12}超.{0,12}(上下文|长度|限制)`),
	regexp.MustCompile(`(?i)超过.{0,12}上下文.{0,8}(长度|限制|窗口)`),
	regexp.MustCompile(`(?i)上下文.{0,8}(长度|窗口).{0,8}超`),
	regexp.MustCompile(`(?i)长度过长`),
}

// overflowExcludes 先于 patterns 判定:命中即不是窗口溢出(rate limit / 计费 /
// 输出参数问题等常与溢出文案相似)。
var overflowExcludes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)rate.?limit`),
	regexp.MustCompile(`(?i)max_tokens must be less than`),
	regexp.MustCompile(`(?i)minimum request length`),
	regexp.MustCompile(`(?i)quota`),
	regexp.MustCompile(`(?i)billing`),
	regexp.MustCompile(`(?i)insufficient`),
	regexp.MustCompile(`(?i)permission`),
}

// IsContextOverflow 判定 AgentRound 错误是否为 provider 上下文窗口溢出。
// 只认 OpsFailure 系错误:HTTP 413(context_limit)或错误细节文案命中模式。
// token_limit(输出被 max_tokens 截断)不是窗口溢出。
func IsContextOverflow(err error) bool {
	var failure *OpsFailure
	if !errors.As(err, &failure) {
		return false
	}
	if failure.SafeCode() == "context_limit" {
		return true
	}
	msg := err.Error()
	for _, ex := range overflowExcludes {
		if ex.MatchString(msg) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(msg) {
			return true
		}
	}
	return false
}

// opsModel 实现 opschat.OverflowClassifier。
func (m *opsModel) IsContextOverflow(err error) bool { return IsContextOverflow(err) }
