package registryhub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client 是 Docker Registry HTTP API v2 的最小客户端(内置制品 registry 用:catalog/tags/
// manifest 元数据/DELETE manifest,供保留策略)。无鉴权(内网明文 HTTP);仅本包使用。
type Client struct {
	base string
	hc   *http.Client
}

// NewClient 构造客户端。base 形如 http://127.0.0.1:5000(无尾斜杠);hc 为 nil 用 10s 超时默认。
func NewClient(base string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{base: strings.TrimRight(base, "/"), hc: hc}
}

// Catalog 列全部仓库名(GET /v2/_catalog)。
func (c *Client) Catalog(ctx context.Context) ([]string, error) {
	var out struct {
		Repositories []string `json:"repositories"`
	}
	if _, err := c.getJSON(ctx, "/v2/_catalog", &out); err != nil {
		return nil, err
	}
	return out.Repositories, nil
}

// Tags 列某仓库全部 tag(GET /v2/<repo>/tags/list)。
func (c *Client) Tags(ctx context.Context, repo string) ([]string, error) {
	var out struct {
		Tags []string `json:"tags"`
	}
	if _, err := c.getJSON(ctx, "/v2/"+repo+"/tags/list", &out); err != nil {
		return nil, err
	}
	return out.Tags, nil
}

// TagInfo 是一个 tag 的元数据(Digest 供 DELETE;Created 取镜像 config blob 的 created 字段,
// 缺失时回退零值——保留策略按创建时间排序,零值排最后=最旧,倾向被清,安全方向)。
type TagInfo struct {
	Tag     string
	Digest  string
	Created time.Time
}

// Manifest 查某 tag 的 manifest,返回 digest(供 DELETE)与其引用的 config blob digest。
func (c *Client) Manifest(ctx context.Context, repo, tag string) (digest, configDigest string, err error) {
	path := "/v2/" + repo + "/manifests/" + tag
	req, rerr := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if rerr != nil {
		return "", "", rerr
	}
	// 同时声明 docker v2 与 OCI manifest;registry 以第一个匹配响应,Docker-Content-Digest 头给 digest。
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.oci.image.index.v1+json",
	}, ", "))
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("registry: manifest %s:%s → HTTP %d", repo, tag, resp.StatusCode)
	}
	digest = resp.Header.Get("Docker-Content-Digest")
	var m struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if jerr := json.Unmarshal(body, &m); jerr == nil {
		configDigest = m.Config.Digest
	}
	return digest, configDigest, nil
}

// ConfigCreated 取 config blob 的 created 时间(镜像构建时间,作 tag 的创建时间)。
func (c *Client) ConfigCreated(ctx context.Context, repo, configDigest string) (time.Time, error) {
	if configDigest == "" {
		return time.Time{}, nil
	}
	var cfg struct {
		Created time.Time `json:"created"`
	}
	if _, err := c.getJSON(ctx, "/v2/"+repo+"/blobs/"+configDigest, &cfg); err != nil {
		return time.Time{}, err
	}
	return cfg.Created.UTC(), nil
}

// TagInfos 汇总某仓库全部 tag 的 digest + 创建时间(manifest + config blob 两次往返/tag;
// 仓库规模为本平台单项目 tag 数,可接受)。单个 tag 查询失败跳过(不阻断整库清理)。
func (c *Client) TagInfos(ctx context.Context, repo string) ([]TagInfo, error) {
	tags, err := c.Tags(ctx, repo)
	if err != nil {
		return nil, err
	}
	infos := make([]TagInfo, 0, len(tags))
	for _, t := range tags {
		digest, cfgDigest, merr := c.Manifest(ctx, repo, t)
		if merr != nil || digest == "" {
			continue
		}
		created, _ := c.ConfigCreated(ctx, repo, cfgDigest)
		infos = append(infos, TagInfo{Tag: t, Digest: digest, Created: created})
	}
	return infos, nil
}

// DeleteManifest 按 digest 删除 manifest(需 registry 开 REGISTRY_STORAGE_DELETE_ENABLED;
// blob 空间由后续 garbage-collect 回收)。
func (c *Client) DeleteManifest(ctx context.Context, repo, digest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.base+"/v2/"+repo+"/manifests/"+digest, nil)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	// 202 Accepted = 已受理;404 = 已不在(幂等,视为成功);200/204 兼容老实现。
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("registry: delete %s@%s → HTTP %d", repo, digest, resp.StatusCode)
	}
}

// getJSON 发 GET 并把响应体解析进 out(out 为 nil 时仅探活)。
func (c *Client) getJSON(ctx context.Context, path string, out any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry: GET %s → HTTP %d", sanitizePath(path), resp.StatusCode)
	}
	if out != nil {
		if jerr := json.Unmarshal(body, out); jerr != nil {
			return nil, fmt.Errorf("registry: parse %s: %w", sanitizePath(path), jerr)
		}
	}
	return body, nil
}

// sanitizePath 把查询串从错误信息里剥掉(日志/错误体最小泄漏)。
func sanitizePath(p string) string {
	if u, err := url.Parse(p); err == nil {
		return u.Path
	}
	return p
}
