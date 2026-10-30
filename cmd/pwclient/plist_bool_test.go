package main

import "testing"

// 样例与本服务 install() 写出的 plist 同构(RunAtLoad 与 KeepAlive 相邻,值标签
// 交错),覆盖"取最近值标签"的关键场景:改 RunAtLoad 不得命中 KeepAlive 的标签。
const samplePlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
    <key>Label</key><string>com.pipewright.client</string>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
</dict>
</plist>
`

func TestPlistBoolAfter(t *testing.T) {
	if got := plistBoolAfter(samplePlist, "RunAtLoad"); got != "<true/>" {
		t.Fatalf("RunAtLoad = %q, want <true/>", got)
	}
	if got := plistBoolAfter(samplePlist, "NoSuchKey"); got != "" {
		t.Fatalf("缺失键应空串, got %q", got)
	}
	noBool := "<dict><key>RunAtLoad</key><string>x</string></dict>"
	if got := plistBoolAfter(noBool, "RunAtLoad"); got != "" {
		t.Fatalf("无布尔值标签应空串, got %q", got)
	}
}

func TestSetPlistBoolAfterPicksNearestTag(t *testing.T) {
	// 禁用(改 false):RunAtLoad 的 <true/> 在前,必须改它而不是 KeepAlive 的。
	got, ok := setPlistBoolAfter(samplePlist, "RunAtLoad", "<false/>")
	if !ok {
		t.Fatal("应找到值标签")
	}
	if v := plistBoolAfter(got, "RunAtLoad"); v != "<false/>" {
		t.Fatalf("RunAtLoad 应已改 false, got %q", v)
	}
	if v := plistBoolAfter(got, "KeepAlive"); v != "<true/>" {
		t.Fatalf("KeepAlive 不应被误改, got %q", v)
	}

	// 再启用(改 true):此刻 RunAtLoad 的 <false/> 是最近的标签(KeepAlive 的 <true/>
	// 更远),旧实现取首个 <true/> 会改到 KeepAlive 而使 RunAtLoad 保持 false。
	reEnabled, _ := setPlistBoolAfter(got, "RunAtLoad", "<true/>")
	if v := plistBoolAfter(reEnabled, "RunAtLoad"); v != "<true/>" {
		t.Fatalf("RunAtLoad 应已改回 true, got %q", v)
	}
	if v := plistBoolAfter(reEnabled, "KeepAlive"); v != "<true/>" {
		t.Fatalf("KeepAlive 应保持 true, got %q", v)
	}

	// 幂等:已相等时原样返回。
	same, ok := setPlistBoolAfter(samplePlist, "RunAtLoad", "<true/>")
	if !ok || same != samplePlist {
		t.Fatalf("已相等应原样返回: ok=%v changed=%v", ok, same != samplePlist)
	}

	// 键缺失:不动文本。
	untouched, ok := setPlistBoolAfter(samplePlist, "NoSuchKey", "<false/>")
	if ok || untouched != samplePlist {
		t.Fatalf("缺失键应不动文本: ok=%v", ok)
	}
}
