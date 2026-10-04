package runner

import (
	"errors"
	"reflect"
	"testing"
)

func TestParseSelector(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
		err  error
	}{
		{"空=本地", "", nil, nil},
		{"空白=本地", "  ", nil, nil},
		{"单项", "linux", []string{"linux"}, nil},
		{"多项AND", "linux, arch=arm64 ,gpu", []string{"linux", "arch=arm64", "gpu"}, nil},
		{"去重保序", "linux,gpu,linux", []string{"linux", "gpu"}, nil},
		{"k=v 值含点划线", "arch=arm64,ver=1.2_3-x", []string{"arch=arm64", "ver=1.2_3-x"}, nil},
		{"空项视为笔误", "a,,b", nil, ErrInvalidSelector},
		{"非法字符", "os=linux!", nil, ErrInvalidSelector},
		{"k=v 缺值", "arch=", nil, ErrInvalidSelector},
		{"项以-开头", "-gpu", nil, ErrInvalidSelector},
		{"server: 钉死形式不归本函数(: 非法字符,由 PinnedServer 处理)", "server:abc", nil, ErrInvalidSelector},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseSelector(tc.in)
			if !errors.Is(err, tc.err) {
				t.Fatalf("err = %v, want %v", err, tc.err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("terms = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseSelectorLimits(t *testing.T) {
	if _, err := ParseSelector("a"); len("a") > selectorLenMax && err == nil {
		t.Fatal("unreachable")
	}
	// 17 项超上限。
	terms := make([]string, selectorTermMax+1)
	for i := range terms {
		terms[i] = "t" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	if _, err := ParseSelector(joinComma(terms)); err == nil {
		t.Fatal("超项数上限应报非法")
	}
	// 总长超限。
	long := "x"
	for len(long) <= selectorLenMax {
		long += ",xxxxxxxxxx"
	}
	if _, err := ParseSelector(long); err == nil {
		t.Fatal("超总长上限应报非法")
	}
}

func joinComma(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

func TestPinnedServer(t *testing.T) {
	if id, ok := PinnedServer("server:srv-1.2_x"); !ok || id != "srv-1.2_x" {
		t.Fatalf("PinnedServer = %q/%v, want srv-1.2_x/true", id, ok)
	}
	if _, ok := PinnedServer("server:"); ok {
		t.Fatal("空 id 不应识别为钉死形式")
	}
	if _, ok := PinnedServer("linux,gpu"); ok {
		t.Fatal("标签形式不应识别为钉死")
	}
	if _, ok := PinnedServer("server:bad!id"); ok {
		t.Fatal("非法 id 不应识别为钉死")
	}
}

func TestValidateSelector(t *testing.T) {
	for _, ok := range []string{"", "linux,arch=arm64", "server:srv-1"} {
		if err := ValidateSelector(ok); err != nil {
			t.Fatalf("ValidateSelector(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"a b", "arch=", ",,"} {
		if err := ValidateSelector(bad); err == nil {
			t.Fatalf("ValidateSelector(%q) 应报非法", bad)
		}
	}
}

func TestMatchSelector(t *testing.T) {
	cases := []struct {
		name   string
		labels string
		terms  []string
		want   bool
	}{
		{"全命中", "linux,arch=arm64,gpu", []string{"linux", "arch=arm64"}, true},
		{"缺一项即不中", "linux,arch=arm64", []string{"linux", "gpu"}, false},
		{"纯 tag 不匹配 k=v", "os=linux", []string{"linux"}, false},
		{"k=v 精确相等", "arch=arm64", []string{"arch=arm64"}, true},
		{"空标签永不中", "", []string{"linux"}, false},
		{"标签多余仍中(AND 只约束选择器侧)", "linux,gpu,extra", []string{"linux"}, true},
		{"空选择器不算匹配(本地路径不进调度)", "linux", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MatchSelector(tc.labels, tc.terms); got != tc.want {
				t.Fatalf("MatchSelector(%q,%v) = %v, want %v", tc.labels, tc.terms, got, tc.want)
			}
		})
	}
}

func TestValidateTerm(t *testing.T) {
	for _, good := range []string{"linux", "arch=arm64", "a.b_c-d=1.2", " linux "} {
		if err := ValidateTerm(good); err != nil {
			t.Fatalf("ValidateTerm(%q) 应合法,got %v", good, err)
		}
	}
	for _, bad := range []string{"", "  ", "a b", "=v", "k=", "-lead", "标签"} {
		if err := ValidateTerm(bad); err == nil {
			t.Fatalf("ValidateTerm(%q) 应报非法", bad)
		}
	}
}
