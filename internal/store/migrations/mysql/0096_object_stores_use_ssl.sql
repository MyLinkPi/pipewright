-- 0096 object_stores use_ssl(MySQL):对象存储配置加「是否走 HTTPS」开关。
--   Endpoint 仍按约定存不带 scheme 的 host[:port],是否 TLS 由本列决定:
--   use_ssl=1 → https,use_ssl=0 → http。默认值按类型的规则在领域层
--   (defaultUseSSL:aliyun_oss=true / rustfs=false),DB 列默认 0 仅为兜底。
--   存量数据一次性回填:阿里云 OSS 全量翻为 HTTPS(公网接入点 443 全支持,
--   明文上传 AK 签名的对象属安全隐患);RustFS 自建内网部署保持 HTTP 不变。
ALTER TABLE object_stores ADD COLUMN use_ssl TINYINT(1) NOT NULL DEFAULT 0;
UPDATE object_stores SET use_ssl = 1 WHERE type = 'aliyun_oss';
