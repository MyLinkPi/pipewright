-- 0074 object_stores path prefix(MySQL):对象存储配置加可选路径前缀。
--   所有经该配置上传的对象键统一附加此前缀(存储级默认,流水线节点无需各自填写);
--   归一化规则(去空白/首尾斜杠)由领域层负责,DB 存原文。
ALTER TABLE object_stores ADD COLUMN path_prefix VARCHAR(512) NOT NULL DEFAULT '';
