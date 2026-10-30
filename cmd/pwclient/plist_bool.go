// plist_bool.go 是 launchd plist 布尔值键的纯文本编辑小工具(供 svc_launchd.go 使用)。
// 不带 build tag:纯字符串函数,全平台编译使配套单测可在任意开发机/CI 上执行。
package main

import "strings"

// plistBoolAfter 返回 plist 中 key 后**最近**的布尔值标签("<true/>"/"<false/>");
// key 或值标签缺失返回空串。取最近而非首个同值标签:plist 里布尔值无键名包裹,
// 扫远了会把后续键(如 KeepAlive)的值当成当前键的。
func plistBoolAfter(text, key string) string {
	k := "<key>" + key + "</key>"
	i := strings.Index(text, k)
	if i < 0 {
		return ""
	}
	rest := text[i+len(k):]
	ti, fi := strings.Index(rest, "<true/>"), strings.Index(rest, "<false/>")
	switch {
	case ti >= 0 && (fi < 0 || ti < fi):
		return "<true/>"
	case fi >= 0:
		return "<false/>"
	default:
		return ""
	}
}

// setPlistBoolAfter 把 plist 中 key 后最近的布尔值标签替换为 want(已相等则原样),
// 返回替换后文本与是否找到了值标签。
func setPlistBoolAfter(text, key, want string) (string, bool) {
	k := "<key>" + key + "</key>"
	i := strings.Index(text, k)
	if i < 0 {
		return text, false
	}
	rest := text[i+len(k):]
	ti, fi := strings.Index(rest, "<true/>"), strings.Index(rest, "<false/>")
	j, tag := -1, ""
	switch {
	case ti >= 0 && (fi < 0 || ti < fi):
		j, tag = ti, "<true/>"
	case fi >= 0:
		j, tag = fi, "<false/>"
	}
	if j < 0 {
		return text, false
	}
	if tag == want {
		return text, true
	}
	return text[:i+len(k)+j] + want + rest[j+len(tag):], true
}
