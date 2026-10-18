// parse.go — lsblk / pvs / vgs / lvs / df 输出解析(纯函数,可单测)。
//
// 解析纪律与 ceph/parse.go、mesh/easytier/status.go 相同:宽容 JSON(值可为任意形状),
// 空/格式异常一律零值,绝不 panic;版本差异导致的字段名变化经多候选键兜底。
// lsblk 老版本无 MOUNTPOINTS 列时由采集脚本回退 MOUNTPOINT 单列,此处同时兼容两者。
package diskadmin

import (
	"encoding/json"
	"strconv"
	"strings"
)

// obj 是宽容 JSON 对象。
type obj map[string]any

// asObj 把任意 JSON 值宽容转为对象;非对象返回 nil。
func asObj(v any) obj {
	if m, ok := v.(map[string]any); ok {
		return obj(m)
	}
	return nil
}

// asArr 把任意 JSON 值宽容转为数组;非数组返回 nil。
func asArr(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	return nil
}

// pickStr 从对象取第一个存在且非空的键,宽容 string/number/bool。
func pickStr(o obj, keys ...string) string {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			if s := stringifyPlain(v); s != "" {
				return s
			}
		}
	}
	return ""
}

// pickNum 从对象取第一个可解析为数的键(string 自动去单位解析)。
func pickNum(o obj, keys ...string) float64 {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			switch t := v.(type) {
			case float64:
				return t
			case string:
				if f, err := strconv.ParseFloat(strings.TrimSpace(t), 64); err == nil {
					return f
				}
			}
		}
	}
	return 0
}

// pickBytes 取字节数(LVM --units b 的字符串形如 "1073741824B",去 B 后缀解析)。
func pickBytes(o obj, keys ...string) uint64 {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			switch t := v.(type) {
			case float64:
				if t > 0 {
					return uint64(t)
				}
			case string:
				s := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(t), "B"))
				if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
					return uint64(f)
				}
			}
		}
	}
	return 0
}

// pickBool 取布尔(lsblk "ro":"0"/"1"/true 均宽容)。
func pickBool(o obj, keys ...string) bool {
	for _, k := range keys {
		if v, ok := o[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return t == "1" || strings.EqualFold(t, "true")
			case float64:
				return t != 0
			}
		}
	}
	return false
}

// pickMountpoints 取挂载点列表(新版 MOUNTPOINTS 数组;老版 MOUNTPOINT 单串)。
func pickMountpoints(o obj) []string {
	var out []string
	for _, e := range asArr(o["mountpoints"]) {
		if s := stringifyPlain(e); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		if s := pickStr(o, "mountpoint"); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func stringifyPlain(v any) string {
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	return ""
}

// parseJSON 顶层解析;空/非法返回 nil。
func parseJSON(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil
	}
	return v
}

// splitSections 按行首 "##PW:<名>" 标记切分合并脚本 stdout(段名 → 段文本;与
// httpapi/server_metrics.go splitMetricSections 同形)。
func splitSections(out string) map[string]string {
	const marker = "##PW:"
	sections := map[string]string{}
	cur := ""
	var buf strings.Builder
	flush := func() {
		if cur != "" {
			sections[cur] = buf.String()
		}
	}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(line, marker) {
			flush()
			cur = strings.TrimSpace(strings.TrimPrefix(line, marker))
			buf.Reset()
			continue
		}
		if cur != "" {
			buf.WriteString(line)
			buf.WriteByte('\n')
		}
	}
	flush()
	return sections
}

// parseDfUsage 解析 `df -B1 -P`:挂载点 → {used,total}(字节;按第二/三列与末列,
// 宽容超长设备名换行 —— -P 格式恒单行,字段数 ≥6)。
func parseDfUsage(stdout string) map[string]dfUsage {
	out := map[string]dfUsage{}
	for _, line := range strings.Split(stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		total, err1 := strconv.ParseUint(fields[1], 10, 64)
		used, err2 := strconv.ParseUint(fields[2], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out[fields[len(fields)-1]] = dfUsage{Used: used, Total: total}
	}
	return out
}

// dfUsage 是 df 一行的用量(字节)。
type dfUsage struct {
	Used  uint64
	Total uint64
}

// dmName 计算 LVM 设备的 /dev/mapper 名(vg-lv,名中 "-" 转义为 "--";lsblk 树上的
// lvm 节点以此路径出现,用于 LV 与树节点匹配)。
func dmName(vg, lv string) string {
	return "/dev/mapper/" + vg + "-" + strings.ReplaceAll(lv, "-", "--")
}
