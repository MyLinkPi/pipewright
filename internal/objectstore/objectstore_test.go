package objectstore

import (
	"strings"
	"testing"
)

func TestValidType(t *testing.T) {
	if !ValidType(TypeAliyunOSS) {
		t.Fatalf("aliyun_oss 应为合法类型")
	}
	if !ValidType(TypeRustFS) {
		t.Fatalf("rustfs 应为合法类型")
	}
	for _, bad := range []string{"", "s3", "minio", "Aliyun_OSS", "RustFS"} {
		if ValidType(bad) {
			t.Fatalf("类型 %q 不应合法", bad)
		}
	}
}

func TestNormalizeEndpoint(t *testing.T) {
	cases := map[string]string{
		"  oss-cn-hangzhou.aliyuncs.com  ":        "oss-cn-hangzhou.aliyuncs.com",
		"https://oss-cn-hangzhou.aliyuncs.com":    "oss-cn-hangzhou.aliyuncs.com",
		"http://oss-cn-beijing.aliyuncs.com/":     "oss-cn-beijing.aliyuncs.com",
		"https://oss-cn-shanghai.aliyuncs.com///": "oss-cn-shanghai.aliyuncs.com",
		"": "",
	}
	for in, want := range cases {
		if got := normalizeEndpoint(in); got != want {
			t.Fatalf("normalizeEndpoint(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestValidateCreate(t *testing.T) {
	valid := CreateInput{
		Type: TypeAliyunOSS, Name: "prod", Endpoint: "oss-cn-hangzhou.aliyuncs.com",
		Bucket: "my-bucket", AccessKeyID: "LTAIxxxx", CredentialID: "cred-1",
	}
	if err := validateCreate(valid); err != nil {
		t.Fatalf("合法入参被拒: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*CreateInput)
		want error
	}{
		{"bad type", func(in *CreateInput) { in.Type = "s3" }, ErrInvalidType},
		{"empty name", func(in *CreateInput) { in.Name = "" }, ErrEmptyName},
		{"empty endpoint", func(in *CreateInput) { in.Endpoint = "" }, ErrEmptyEndpoint},
		{"empty bucket", func(in *CreateInput) { in.Bucket = "" }, ErrEmptyBucket},
		{"empty ak", func(in *CreateInput) { in.AccessKeyID = "" }, ErrEmptyAccessKeyID},
		{"empty credential", func(in *CreateInput) { in.CredentialID = "" }, ErrEmptyCredentialID},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := valid
			c.mut(&in)
			if err := validateCreate(in); err != c.want {
				t.Fatalf("validateCreate=%v, want %v", err, c.want)
			}
		})
	}
}

func TestNormalizeCreate(t *testing.T) {
	in := CreateInput{
		Type: "  Aliyun_OSS ", Name: "  prod  ", Endpoint: "https://oss-cn-hangzhou.aliyuncs.com/",
		Bucket: "  my-bucket ", AccessKeyID: "  LTAIxxxx ", CredentialID: "  cred-1 ",
	}
	normalizeCreate(&in)
	if in.Type != "aliyun_oss" || in.Name != "prod" || in.Endpoint != "oss-cn-hangzhou.aliyuncs.com" ||
		in.Bucket != "my-bucket" || in.AccessKeyID != "LTAIxxxx" || in.CredentialID != "cred-1" {
		t.Fatalf("normalizeCreate 归一化不符预期: %+v", in)
	}
}

func TestDescribeClientError(t *testing.T) {
	if got := describeClientError(nil); got != "" {
		t.Fatalf("nil 错误应返回空串, got %q", got)
	}
	// 非 ServiceError(如网络错误)→ 通用短语,绝不回显原始串。
	got := describeClientError(errString("dial tcp: connection refused accessKey=SECRET"))
	if strings.Contains(got, "SECRET") || strings.Contains(got, "accessKey") {
		t.Fatalf("通用短语不应泄漏原始错误串: %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
