package objectstore

import (
	"context"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// rustfsObjectClient 是 RustFS(及任意 S3 兼容自建存储)的 ObjectClient 实现,基于 minio-go
// (S3 API + AWS SigV4)。Endpoint 形如 host:port(不带 scheme);自建存储按 IP/主机名访问,
// 必须走 path-style(BucketLookupPath),不能用虚拟主机样式。
// 客户端无状态、每次操作即建即弃(与阿里云实现同一纪律)。
type rustfsObjectClient struct {
	client     *minio.Client
	bucketName string
}

// newRustFSClient 构造 RustFS 客户端并绑定目标桶(endpoint 为 host:port;HTTP,同现有
// Endpoint 归一化约定——去 scheme)。
func newRustFSClient(endpoint, accessKeyID, accessKeySecret, bucketName string) (ObjectClient, error) {
	cli, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(accessKeyID, accessKeySecret, ""),
		Secure:       false,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		return nil, err
	}
	return &rustfsObjectClient{client: cli, bucketName: bucketName}, nil
}

// VerifyBucket 校验桶存在且凭据可访问(触网)。
func (c *rustfsObjectClient) VerifyBucket(ctx context.Context) error {
	exists, err := c.client.BucketExists(ctx, c.bucketName)
	if err != nil {
		return err
	}
	if !exists {
		return minio.ErrorResponse{Code: "NoSuchBucket", BucketName: c.bucketName}
	}
	return nil
}

// PutObject 上传一个对象(流式未知长度,minio-go 自动分片)。
func (c *rustfsObjectClient) PutObject(ctx context.Context, objectKey string, body io.Reader) error {
	_, err := c.client.PutObject(ctx, c.bucketName, objectKey, body, -1, minio.PutObjectOptions{})
	return err
}

// EnsureBucket 确保目标桶存在(不存在则创建;已存在为幂等 no-op)。供应用商店部署 RustFS
// 后的自动登记链路使用(见 service.Register)。
func (c *rustfsObjectClient) EnsureBucket(ctx context.Context) error {
	exists, err := c.client.BucketExists(ctx, c.bucketName)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	return c.client.MakeBucket(ctx, c.bucketName, minio.MakeBucketOptions{})
}
