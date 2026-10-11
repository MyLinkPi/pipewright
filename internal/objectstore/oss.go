package objectstore

import (
	"context"
	"errors"
	"io"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

// newObjectClient 按提供商类型造真实 ObjectClient(本期仅阿里云 OSS)。
func newObjectClient(providerType, endpoint, accessKeyID, accessKeySecret, bucketName string) (ObjectClient, error) {
	switch providerType {
	case TypeAliyunOSS:
		return newAliyunOSSClient(endpoint, accessKeyID, accessKeySecret, bucketName)
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
func newAliyunOSSClient(endpoint, accessKeyID, accessKeySecret, bucketName string) (ObjectClient, error) {
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

// describeClientError 把 SDK / 网络错误渲染为人话(绝不含 AccessKey 明文)。
// 服务端错误(oss.ServiceError)输出其 Code/Message;其余(网络/连接)输出通用短语,不泄露原始串。
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
	return "无法连接对象存储(网络异常、Endpoint 不可达或凭据无效)"
}
