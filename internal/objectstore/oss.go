package objectstore

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
	"github.com/minio/minio-go/v7"
)

// newObjectClient 按提供商类型造真实 ObjectClient(阿里云 OSS / S3 兼容 RustFS)。
// useSSL 决定传输层是否走 TLS(Endpoint 本身不含 scheme)。
func newObjectClient(providerType, endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucketName string) (ObjectClient, error) {
	switch providerType {
	case TypeAliyunOSS:
		return newAliyunOSSClient(endpoint, useSSL, accessKeyID, accessKeySecret, bucketName)
	case TypeRustFS:
		return newRustFSClient(endpoint, useSSL, accessKeyID, accessKeySecret, bucketName)
	default:
		return nil, ErrInvalidType
	}
}

// aliyunObjectClient 是阿里云 OSS 的 ObjectClient 实现(基于官方 SDK)。
// 客户端无状态、每次操作即建即弃(配置变更无需失效缓存;上传频率不高,开销可忽略)。
type aliyunObjectClient struct {
	client     *oss.Client
	bucket     *oss.Bucket
	bucketName string
}

// newAliyunOSSClient 构造阿里云 OSS 客户端并绑定目标桶(endpoint 需为不带 scheme 的接入点)。
// SDK v3 无 SSL 开关选项,scheme 由 endpoint 前缀决定:useSSL 时临时拼 https:// 前缀
// (仅本方法的局部变量,存储值仍是裸 host)。
func newAliyunOSSClient(endpoint string, useSSL bool, accessKeyID, accessKeySecret, bucketName string) (ObjectClient, error) {
	if useSSL {
		endpoint = "https://" + endpoint
	}
	client, err := oss.New(endpoint, accessKeyID, accessKeySecret)
	if err != nil {
		return nil, err
	}
	bucket, err := client.Bucket(bucketName)
	if err != nil {
		return nil, err
	}
	return &aliyunObjectClient{client: client, bucket: bucket, bucketName: bucketName}, nil
}

// VerifyBucket 取桶信息(触网):校验凭据可访问 + 桶存在。
func (c *aliyunObjectClient) VerifyBucket(ctx context.Context) error {
	if _, err := c.client.GetBucketInfo(c.bucketName); err != nil {
		return err
	}
	return nil
}

// PutObject 上传一个对象。
func (c *aliyunObjectClient) PutObject(ctx context.Context, objectKey string, body io.Reader) error {
	return c.bucket.PutObject(objectKey, body)
}

// CreateBucket 显式新建一个桶(浏览页「新建桶」入口;默认私有 ACL,区域跟随 Endpoint)。
// 桶已存在时由服务端返回错误,归属判定与孤儿桶恢复在 service 层统一处理。
func (c *aliyunObjectClient) CreateBucket(ctx context.Context, bucketName string) error {
	return c.client.CreateBucket(bucketName)
}

// listBucketsPageSize 是 OSS 列桶的分页步长(账户桶数远小于对象数,200/页翻页拉全)。
const listBucketsPageSize = 200

// ListBuckets 列举账户下的桶(Marker 游标翻页拉全;截断上限由 service 层统一处理)。
func (c *aliyunObjectClient) ListBuckets(ctx context.Context) ([]BucketEntry, error) {
	var out []BucketEntry
	marker := ""
	for {
		res, err := c.client.ListBuckets(oss.Marker(marker), oss.MaxKeys(listBucketsPageSize))
		if err != nil {
			return nil, err
		}
		for _, b := range res.Buckets {
			out = append(out, BucketEntry{Name: b.Name, CreatedAt: b.CreationDate})
		}
		if !res.IsTruncated || res.NextMarker == "" {
			return out, nil
		}
		marker = res.NextMarker
	}
}

// ListPage 单页列举一个目录(V1 列举,delimiter 折叠子目录,marker 游标)。
// 桶内对象可能巨多,SDK 单次调用单页返回,绝不聚合全量。
func (c *aliyunObjectClient) ListPage(ctx context.Context, in ListPageInput) (ListPageResult, error) {
	res, err := c.bucket.ListObjects(
		oss.Prefix(in.Prefix),
		oss.Marker(in.Marker),
		oss.Delimiter("/"),
		oss.MaxKeys(in.MaxKeys),
	)
	if err != nil {
		return ListPageResult{}, err
	}
	out := ListPageResult{Prefixes: make([]string, 0, len(res.CommonPrefixes)), Objects: make([]ObjectEntry, 0, len(res.Objects))}
	var last string
	for _, p := range res.CommonPrefixes {
		out.Prefixes = append(out.Prefixes, p)
		if p > last {
			last = p
		}
	}
	for _, obj := range res.Objects {
		// 目录占位对象(key 恰等于当前前缀或以 / 结尾)不是文件,不展示。
		if obj.Key == in.Prefix || strings.HasSuffix(obj.Key, "/") {
			continue
		}
		out.Objects = append(out.Objects, ObjectEntry{Key: obj.Key, Size: obj.Size, LastModified: obj.LastModified})
		if obj.Key > last {
			last = obj.Key
		}
	}
	if res.IsTruncated {
		// OSS 带 delimiter 时返回 NextMarker;个别网关缺失时按 V1 语义取字典序最大 key 兜底。
		out.NextMarker = res.NextMarker
		if out.NextMarker == "" {
			out.NextMarker = last
		}
	}
	return out, nil
}

// isBucketAlreadyOwned 判断建桶错误是否为「桶已存在且归当前账户所有」——可视为建桶成功
// (覆盖此前半程失败在服务端留下的孤儿桶:桶已建成但配置没登记)。
func isBucketAlreadyOwned(err error) bool {
	if err == nil {
		return false
	}
	var se oss.ServiceError
	if errors.As(err, &se) {
		return se.Code == "BucketAlreadyOwnedByYou"
	}
	var me minio.ErrorResponse
	if errors.As(err, &me) {
		return me.Code == "BucketAlreadyOwnedByYou"
	}
	return false
}

// describeClientError 把 SDK / 网络错误渲染为人话(绝不含 AccessKey 明文)。
// 服务端错误(oss.ServiceError / minio.ErrorResponse)输出其 Code/Message;其余(网络/连接)
// 输出通用短语,不泄露原始串。
func describeClientError(err error) string {
	if err == nil {
		return ""
	}
	var se oss.ServiceError
	if errors.As(err, &se) {
		if se.Message != "" {
			if se.Code != "" {
				return se.Code + ": " + se.Message
			}
			return se.Message
		}
		if se.Code != "" {
			return se.Code
		}
	}
	var me minio.ErrorResponse
	if errors.As(err, &me) {
		if me.Message != "" {
			if me.Code != "" {
				return me.Code + ": " + me.Message
			}
			return me.Message
		}
		if me.Code != "" {
			return me.Code
		}
	}
	return "无法连接对象存储(网络异常、Endpoint 不可达或凭据无效)"
}
