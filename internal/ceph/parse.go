// parse.go — ceph/rbd CLI 的 -f json 输出解析(纯函数,可单测)。
//
// 解析纪律与 mesh/easytier/status.go 相同:宽容 JSON(值可为任意形状),空/格式异常
// 一律零值,绝不 panic;版本差异导致的字段名变化经多候选键兜底。
package ceph

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

// objAt 按候选键取第一个存在的子对象。
func objAt(o obj, keys ...string) obj {
	for _, k := range keys {
		if sub := asObj(o[k]); sub != nil {
			return sub
		}
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

// words 把宽容值转小写词列表:数组逐元素、字符串按逗号/空白拆(如 osd status "up,in")。
func words(v any) []string {
	var out []string
	add := func(s string) {
		for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' }) {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, strings.ToLower(p))
			}
		}
	}
	switch t := v.(type) {
	case string:
		add(t)
	case []any:
		for _, e := range t {
			add(stringifyPlain(e))
		}
	}
	return out
}

// hasWord 报告词列表是否含 w。
func hasWord(ws []string, w string) bool {
	for _, x := range ws {
		if x == w {
			return true
		}
	}
	return false
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

// ── 集群状态 ─────────────────────────────────────────────────────────────────

// parseClusterStatus 解析 `ceph -s -f json`(health/osdmap/monmap/pgmap/mgr)。
func parseClusterStatus(stdout string) ClusterStatus {
	st := ClusterStatus{Health: "HEALTH_UNKNOWN"}
	root := asObj(parseJSON(stdout))
	if root == nil {
		return st
	}
	if health := objAt(root, "health"); health != nil {
		st.Health = pickStr(health, "status")
		if st.Health == "" {
			st.Health = "HEALTH_UNKNOWN"
		}
	}
	if mm := objAt(root, "monmap"); mm != nil {
		st.MonCount = int(pickNum(mm, "num_mons"))
	}
	// mgr:{"active":{...},"standbys":[...]}(宽容:active 存在即计 1)。
	if mgr := objAt(root, "mgr"); mgr != nil {
		n := len(asArr(mgr["standbys"]))
		if a := asObj(mgr["active"]); a != nil || pickStr(mgr, "active_name", "active") != "" {
			n++
		}
		st.MgrCount = n
	}
	if om := objAt(root, "osdmap"); om != nil {
		st.OsdUp = int(pickNum(om, "num_up_osds"))
		st.OsdIn = int(pickNum(om, "num_in_osds"))
	}
	if pg := objAt(root, "pgmap"); pg != nil {
		st.TotalBytes = uint64(pickNum(pg, "bytes_total"))
		st.UsedBytes = uint64(pickNum(pg, "bytes_used", "bytes_used_raw"))
		st.AvailBytes = uint64(pickNum(pg, "bytes_avail", "bytes_available"))
		for _, e := range asArr(pg["pgs_by_state"]) {
			o := asObj(e)
			if o == nil {
				continue
			}
			st.PGsByState = append(st.PGsByState, PGStateCount{
				State: pickStr(o, "state_name", "state"),
				Count: int(pickNum(o, "count")),
			})
		}
	}
	return st
}

// parseHealthDetail 解析 `ceph health detail -f json` 为人读告警行(≤50 条)。
// checks 兼容对象(map,按检查项 keyed,官方形态)与数组两种形态。
func parseHealthDetail(stdout string) []string {
	root := asObj(parseJSON(stdout))
	if root == nil {
		return nil
	}
	var entries []any
	switch checks := root["checks"].(type) {
	case map[string]any:
		for _, v := range checks {
			entries = append(entries, v)
		}
	case []any:
		entries = checks
	}
	var out []string
	for _, c := range entries {
		o := asObj(c)
		if o == nil {
			continue
		}
		sev := pickStr(o, "severity")
		if sum := asObj(o["summary"]); sum != nil {
			if msg := pickStr(sum, "message"); msg != "" {
				out = append(out, sev+": "+msg)
			}
			continue
		}
		if msg := pickStr(o, "summary", "message", "detail"); msg != "" {
			out = append(out, sev+": "+msg)
		}
	}
	if len(out) > 50 {
		out = out[:50]
	}
	return out
}

// ── 主机 / OSD ───────────────────────────────────────────────────────────────

// parseOrchHosts 解析 `ceph orch host ls -f json`。
func parseOrchHosts(stdout string) []OrchHost {
	arr := asArr(parseJSON(stdout))
	if arr == nil {
		// 部分版本把列表包在 {"hosts":[...]}? 官方为顶层数组;宽容兜底对象形态。
		if o := asObj(parseJSON(stdout)); o != nil {
			arr = asArr(o["hosts"])
		}
	}
	out := make([]OrchHost, 0, len(arr))
	for _, e := range arr {
		o := asObj(e)
		if o == nil {
			continue
		}
		h := OrchHost{
			Host:   pickStr(o, "host", "hostname", "name"),
			Addr:   pickStr(o, "addr", "address"),
			Status: pickStr(o, "status"),
		}
		for _, l := range words(o["labels"]) {
			h.Labels = append(h.Labels, l)
		}
		out = append(out, h)
	}
	return out
}

// parseOSDDf 解析 `ceph osd df -f json`(nodes[]:id/status/kb/util)。
func parseOSDDf(stdout string) map[int64]OSD {
	out := map[int64]OSD{}
	root := asObj(parseJSON(stdout))
	if root == nil {
		return out
	}
	for _, e := range asArr(root["nodes"]) {
		o := asObj(e)
		if o == nil {
			continue
		}
		id := int64(pickNum(o, "id"))
		if id < 0 {
			continue
		}
		ws := words(o["status"])
		out[id] = OSD{
			ID:          id,
			Up:          hasWord(ws, "up"),
			In:          hasWord(ws, "in"),
			SizeBytes:   uint64(pickNum(o, "kb")) * 1024,
			UsedBytes:   uint64(pickNum(o, "used_kb", "kb_used")) * 1024,
			Utilization: pickNum(o, "util", "utilization"),
		}
	}
	return out
}

// parseOSDMetadata 解析 `ceph osd metadata -f json`(数组:id/hostname/devices),
// 回填 df 条目的 host/device。
func parseOSDMetadata(stdout string, into map[int64]OSD) {
	for _, e := range asArr(parseJSON(stdout)) {
		o := asObj(e)
		if o == nil {
			continue
		}
		id := int64(pickNum(o, "id", "osd"))
		osd, ok := into[id]
		if !ok {
			continue
		}
		osd.Host = pickStr(o, "hostname", "host")
		if devs := words(o["devices"]); len(devs) > 0 {
			osd.Device = "/dev/" + strings.Join(devs, ",")
		}
		into[id] = osd
	}
}

// ── 存储池 ───────────────────────────────────────────────────────────────────

// parsePools 解析 `ceph osd pool ls detail -f json`。
func parsePools(stdout string) []Pool {
	out := []Pool{}
	for _, e := range asArr(parseJSON(stdout)) {
		o := asObj(e)
		if o == nil {
			continue
		}
		p := Pool{
			Name:          pickStr(o, "pool_name", "pool", "name"),
			Size:          int(pickNum(o, "size")),
			PGNum:         int(pickNum(o, "pg_num")),
			BytesUsed:     uint64(pickNum(o, "bytes_used", "stored", "size")),
			QuotaMaxBytes: uint64(pickNum(o, "quota_max_bytes")),
		}
		p.Type = "replicated"
		if t := pickStr(o, "type"); t == "erasure" {
			p.Type = "erasure"
			p.ECProfile = pickStr(o, "erasure_code_profile")
		} else if prof := pickStr(o, "erasure_code_profile"); prof != "" && prof != "default" {
			p.Type = "erasure"
			p.ECProfile = prof
		}
		if am := asObj(o["application_metadata"]); am != nil {
			for k := range am {
				p.Apps = append(p.Apps, k)
			}
		}
		out = append(out, p)
	}
	return out
}

// ── RBD ──────────────────────────────────────────────────────────────────────

// parseImages 解析 `rbd ls -l -p <pool> -f json`(快照条目 name 含 @,过滤掉)。
// 兼容顶层数组为纯名字数组(旧格式)与对象数组两种形态。
func parseImages(stdout string, pool string) []RBDImage {
	out := []RBDImage{}
	arr := asArr(parseJSON(stdout))
	for _, e := range arr {
		if name := stringifyPlain(e); name != "" && !asObjOK(e) {
			// 纯字符串条目(仅名字)。
			if !strings.Contains(name, "@") {
				out = append(out, RBDImage{Pool: pool, Name: name})
			}
			continue
		}
		o := asObj(e)
		if o == nil {
			continue
		}
		name := pickStr(o, "name", "image")
		if name == "" || strings.Contains(name, "@") {
			continue
		}
		out = append(out, RBDImage{
			Pool:         pool,
			Name:         name,
			SizeBytes:    uint64(pickNum(o, "size", "size_bytes")),
			Provisioning: pickStr(o, "provisioning"),
		})
	}
	return out
}

// asObjOK 报告值是否为对象(区分「字符串条目」与「对象条目」)。
func asObjOK(v any) bool { return asObj(v) != nil }

// parseSnapshots 解析 `rbd snap ls <pool>/<image> -f json`。
func parseSnapshots(stdout string) []RBDSnapshot {
	out := []RBDSnapshot{}
	for _, e := range asArr(parseJSON(stdout)) {
		o := asObj(e)
		if o == nil {
			continue
		}
		out = append(out, RBDSnapshot{
			Name:      pickStr(o, "name"),
			SizeBytes: uint64(pickNum(o, "size")),
			Protected: pickStr(o, "protected") == "true",
		})
	}
	return out
}

// ── CephFS ───────────────────────────────────────────────────────────────────

// parseFilesystems 解析 `ceph fs ls -f json`。
func parseFilesystems(stdout string) []Filesystem {
	out := []Filesystem{}
	for _, e := range asArr(parseJSON(stdout)) {
		o := asObj(e)
		if o == nil {
			continue
		}
		fs := Filesystem{
			Name:         pickStr(o, "name"),
			MetadataPool: pickStr(o, "metadata_pool"),
		}
		for _, p := range words(o["data_pools"]) {
			fs.DataPools = append(fs.DataPools, p)
		}
		out = append(out, fs)
	}
	return out
}

// parseMdsCount 解析 `ceph fs status -f json`,返回 up+standby 的 mds 总数(尽力而为)。
func parseMdsCount(stdout string) int {
	root := asObj(parseJSON(stdout))
	if root == nil {
		return 0
	}
	n := len(asArr(root["standbys"]))
	for _, v := range root {
		if o := asObj(v); o != nil {
			// 每个 fs 对象:{"mdsmap":{"Up":1,...}} 或 {"up":...}。
			if m := objAt(o, "mdsmap"); m != nil {
				n += int(pickNum(m, "Up", "up"))
			} else if u := pickNum(o, "up"); u > 0 {
				n += int(u)
			}
		}
	}
	return n
}

// parseSubvolumeNames 解析 `ceph fs subvolumels <vol> -f json`(名字数组)。
func parseSubvolumeNames(stdout string) []string {
	var out []string
	for _, e := range asArr(parseJSON(stdout)) {
		if s := stringifyPlain(e); s != "" {
			out = append(out, s)
		} else if o := asObj(e); o != nil {
			if s := pickStr(o, "name"); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// parseSubvolumeInfo 解析 `ceph fs subvolume info <vol> <name> -f json`(usage/quota)。
func parseSubvolumeInfo(stdout string) (bytesUsed, quota uint64) {
	root := asObj(parseJSON(stdout))
	if root == nil {
		return 0, 0
	}
	if usage := objAt(root, "usage"); usage != nil {
		bytesUsed = uint64(pickNum(usage, "bytes_used", "used"))
	}
	quota = uint64(pickNum(root, "bytes_quota", "quota", "bytes_pcent"))
	return bytesUsed, quota
}
