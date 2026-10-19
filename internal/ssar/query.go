// query.go — 按需查询机器侧 ssar 细粒度历史(机器本地保留 7 天,sresar 60s/5s 采集)。
//
// 安全纪律(AC-SEC-02):view 走**静态白名单**映射到固定 argv,minutes/step 经数值
// 区间校验后仅以 strconv 结果进入 argv —— 绝无用户输入拼进命令行。
//
// 命令形态(sys 视图):`ssar --<view> --api -r <分钟> -i <步长>m -P`;
// load5s 子命令:`ssar load5s --api -r <分钟>`(其 CLI 无 -i/-P,原生 5s 行)。
// 时间参数语义:ssar `-i` 裸数字按分钟解析、秒级仅 live 模式允许(实现核实自
// ssar.cpp InitOptions),故步长统一以 `m` 后缀显式表达分钟;`-r` 为分钟跨度。
package ssar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 查询参数边界。
const (
	// queryMaxMinutes 是可查的最大跨度(7 天 = ssar 默认保留窗口)。
	queryMaxMinutes = 7 * 24 * 60
	// queryMaxRows 是单次查询的最大行数(步长过细防大结果)。
	queryMaxRows = 1440
	// queryTimeout 是单次按需查询超时(7 天跨度逐段读盘需要一些时间)。
	queryTimeout = 60 * time.Second
)

// ErrInvalidQuery 是查询参数不合法(view 越界 / 区间越界)。
var ErrInvalidQuery = errors.New("ssar: invalid query")

// viewSpec 描述一个白名单视图的命令形态。
type viewSpec struct {
	// sub 为 true 表示子命令形态(load5s:无 -i/-P)。
	sub bool
}

// queryViews 是视图白名单(键 = URL 参数值;与 sys.conf [view] 对齐,剔除 NUMA/
// 碎片化等小众视图)。
var queryViews = map[string]viewSpec{
	"cpu":     {},
	"mem":     {},
	"tcp":     {},
	"udp":     {},
	"vm":      {},
	"softirq": {},
	"retran":  {},
	"tcpofo":  {},
	"tcpdrop": {},
	"tcperr":  {},
	"load5s":  {sub: true},
}

// viewUnits 是已知列别名的单位口径静态表(未知列为空)。d = 每秒增量,c = 瞬时值。
var viewUnits = map[string]string{
	// cpu(jiffies/s)
	"user": "jiffies/s", "nice": "jiffies/s", "system": "jiffies/s", "idle": "jiffies/s",
	"iowait": "jiffies/s", "irq": "jiffies/s", "softirq": "jiffies/s", "steal": "jiffies/s",
	"guest": "jiffies/s", "guest_nice": "jiffies/s",
	// mem(kB)
	"memtotal": "kB", "memfree": "kB", "memavailable": "kB", "buffers": "kB", "cached": "kB",
	"anonpages": "kB", "shmem": "kB", "sreclaimable": "kB", "sunreclaim": "kB",
	"mlocked": "kB", "dirty": "kB", "writeback": "kB",
	// tcp / udp(/s 或瞬时)
	"activeopens": "/s", "passiveopens": "/s", "insegs": "/s", "outsegs": "/s",
	"estabresets": "/s", "attemptfails": "/s", "currestab": "conn", "retranssegs": "/s",
	"indatagrams": "/s", "noports": "/s", "inerrors": "/s", "outdatagrams": "/s",
	"rcvbuferrors": "/s", "sndbuferrors": "/s", "incsumerrors": "/s",
	// retran 家族(/s)
	"lostretransmit": "/s", "fastretrans": "/s", "forwardretrans": "/s",
	"slowstartretrans": "/s", "RTORetrans": "/s", "retransfail": "/s", "synretrans": "/s",
	// softirq(/s)
	"sum_softirq": "/s", "hi": "/s", "timer": "/s", "net_tx": "/s", "net_rx": "/s",
	"block": "/s", "irq_poll": "/s", "tasklet": "/s", "sched": "/s", "hrtimer": "/s", "rcu": "/s",
	// load5s(无量纲)
	"threads": "", "load1": "", "runq": "", "load5s": "", "act": "", "actr": "", "actd": "",
}

// queryArgs 构造一次按需查询的 argv(纯静态模板 + 数值化参数)。view 必须已在白名单。
func queryArgs(view string, minutes, stepMinutes int) []string {
	spec := queryViews[view]
	if spec.sub {
		// load5s:原生 5s 行,-r 为分钟跨度;无 -i/-P。
		return []string{"ssar", view, "--api", "-r", strconv.Itoa(minutes)}
	}
	return []string{"ssar", "--" + view, "--api", "-r", strconv.Itoa(minutes), "-i", strconv.Itoa(stepMinutes) + "m", "-P"}
}

// Query 按需查询机器侧 ssar 历史。view 白名单外 / 区间越界 → ErrInvalidQuery;
// 机器不可达 / ssar 未安装 → 人读错误;无数据(刚装/超出保留窗)→ 空 Rows,不报错。
func (s *service) Query(ctx context.Context, serverID, view string, minutes, stepMinutes int) (*QueryResult, error) {
	spec, ok := queryViews[view]
	if !ok {
		return nil, fmt.Errorf("%w: 未知视图 %q", ErrInvalidQuery, view)
	}
	if minutes < 1 || minutes > queryMaxMinutes {
		return nil, fmt.Errorf("%w: minutes 须在 1..%d", ErrInvalidQuery, queryMaxMinutes)
	}
	rows := minutes // load5s 无步长,按分钟跨度粗算行数上限
	if !spec.sub {
		if stepMinutes < 1 {
			return nil, fmt.Errorf("%w: step 须 ≥ 1 分钟", ErrInvalidQuery)
		}
		rows = minutes / stepMinutes
		if minutes%stepMinutes != 0 {
			rows++
		}
	}
	if rows > queryMaxRows {
		return nil, fmt.Errorf("%w: 行数超上限,请加大步长(step)或缩短时长", ErrInvalidQuery)
	}

	qctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	res, err := s.runner.RunPlain(qctx, serverID, queryArgs(view, minutes, stepMinutes))
	if err != nil {
		if res == nil {
			return nil, fmt.Errorf("查询失败:机器不可达或 ssar 不可用(%v)", err)
		}
		// ssar 查询失败走非零退出码(如无数据目录)—— 无数据视作空结果而非错误。
		if strings.Contains(res.Stdout, "Exception") || strings.TrimSpace(res.Stdout) == "" {
			return &QueryResult{View: view, Columns: []QueryColumn{}, Rows: []QueryRow{}}, nil
		}
	}
	return parseQueryResult(view, res.Stdout)
}

// parseQueryResult 解析 `ssar --api` 输出为 QueryResult(纯函数,可单测)。
// 列 = 首次出现的键序(与 ssar 输出列序一致,**不依赖 map 迭代序**);值可解析数值 →
// float64,否则保留字符串;占位符("-"/"*"/"<"/">")键不写入该行。
func parseQueryResult(view, out string) (*QueryResult, error) {
	rows, ok := parseOrderedRows(out)
	if !ok {
		return &QueryResult{View: view, Columns: []QueryColumn{}, Rows: []QueryRow{}}, nil
	}
	result := &QueryResult{View: view, Columns: []QueryColumn{}, Rows: make([]QueryRow, 0, len(rows))}
	colSeen := map[string]bool{}
	for _, row := range rows {
		qr := QueryRow{Values: map[string]any{}}
		qr.Time = strings.TrimSpace(row.vals["collect_datetime"])
		for _, k := range row.cols {
			if k == "collect_datetime" {
				continue
			}
			if !colSeen[k] {
				colSeen[k] = true
				result.Columns = append(result.Columns, QueryColumn{Key: k, Unit: viewUnits[k]})
			}
			t := strings.TrimSpace(row.vals[k])
			if t == "" || t == "-" || t == "*" || t == "<" || t == ">" {
				continue // 占位符:该格无数据
			}
			if num := ssarNum(t); num != nil {
				qr.Values[k] = *num
				continue
			}
			qr.Values[k] = t // 非数值列(如 stype/sstate)保留原字符串
		}
		if len(qr.Values) == 0 && qr.Time == "" {
			continue // 整行皆占位符:丢弃
		}
		result.Rows = append(result.Rows, qr)
		if len(result.Rows) >= queryMaxRows {
			break
		}
	}
	return result, nil
}

// orderedRow 是一行 `--api` 记录:cols 记录**原始键序**(ssar 列序),vals 为键值。
type orderedRow struct {
	cols []string
	vals map[string]string
}

// parseOrderedRows 解析 `ssar --api` 的 JSON 数组为**保序**行(先整串,失败取首 [ 到
// 末 ] 片段重试)。json.Unmarshal 进 map 会丢键序(map 迭代序随机,列序会抖动),
// 故用 Decoder 令牌流逐键读 —— 列序与 ssar 输出严格一致。空 / 非 JSON / 0 行 → false。
func parseOrderedRows(out string) ([]orderedRow, bool) {
	t := strings.TrimSpace(out)
	if t == "" {
		return nil, false
	}
	rows, err := decodeOrderedRows(t)
	if err != nil {
		start := strings.Index(t, "[")
		end := strings.LastIndex(t, "]")
		if start < 0 || end <= start {
			return nil, false
		}
		rows, err = decodeOrderedRows(t[start : end+1])
		if err != nil {
			return nil, false
		}
	}
	if len(rows) == 0 {
		return nil, false
	}
	return rows, true
}

// decodeOrderedRows 用 json.Decoder 令牌流解析 `[{...},{...}]` 形态(值必须为字符串,
// ssar --api 契约如此;出现其它形态 → 错误交由上层降级)。
func decodeOrderedRows(s string) ([]orderedRow, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return nil, errors.New("not an array")
	}
	var rows []orderedRow
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if d, ok := tok.(json.Delim); !ok || d != '{' {
			return nil, errors.New("not an object")
		}
		row := orderedRow{vals: map[string]string{}}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errors.New("bad key")
			}
			valTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			val, ok := valTok.(string)
			if !ok {
				return nil, errors.New("bad value")
			}
			if _, dup := row.vals[key]; dup {
				return nil, errors.New("duplicate key")
			}
			row.cols = append(row.cols, key)
			row.vals[key] = val
		}
		if _, err := dec.Token(); err != nil { // consume '}'
			return nil, err
		}
		rows = append(rows, row)
	}
	if _, err := dec.Token(); err != nil { // consume ']'
		return nil, err
	}
	return rows, nil
}
