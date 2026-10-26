-- 0105 projects.pac_file:每项目「流水线即代码」文件路径(流水线即代码 · 可指定文件)。
-- 空串 = 默认 .pipewright.yml(历史行为不变);非空 = 运行时/预览/AI 运维从该相对路径读
-- 流水线文件(如 ci/pipeline.yml、.pipewright.staging.yml)。入口校验强制相对路径
-- (拒绝绝对路径/盘符/.. 段),拉取侧与既有 SSRF 防线叠加。运行落库的
-- pipeline_runs.spec_source_file 自动记录实际文件名,无需迁移。
ALTER TABLE projects ADD COLUMN pac_file TEXT NOT NULL DEFAULT '';
