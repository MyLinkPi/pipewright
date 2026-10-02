-- 0061 system_config 新增 release_mirror:自升级(检查更新 + 二进制下载)的镜像源 base URL。
-- 空 = 未配置,回退 GitHub 官方源(api.github.com / github.com;env PIPEWRIGHT_RELEASE_MIRROR
-- 为部署级兜底);非空须为 GitHub 路径兼容的镜像 base(经设置界面 /api/system/config 配置),
-- 修改即时生效、无需重启。
ALTER TABLE system_config ADD COLUMN release_mirror TEXT NOT NULL DEFAULT '';
