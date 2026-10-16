-- 0076 drop_environment_feature: 移除「环境」功能(分支→环境映射 / 未匹配策略 / 环境晋级链 /
-- PR 预览环境 / 环境与凭据 / 环境聚合页),触发器改为纯「分支过滤 glob 白名单」。
--
-- 行为保真:存量 branch_mappings_json 里的 branchPattern 抽入新列 branch_filters_json
-- (仅映射分支才触发的旧语义得以保留,而非突变为"全分支触发");随后 DROP 旧列/旧表。
-- 环境功能计划日后以新设计重做,旧数据不做保留。

-- ① 环境功能旧表(0028 晋级链 + 0047 预览环境)。
DROP TABLE IF EXISTS project_environments;
DROP TABLE IF EXISTS environment_variables;
DROP TABLE IF EXISTS run_promotions;
DROP TABLE IF EXISTS preview_configs;
DROP TABLE IF EXISTS preview_envs;

-- ② 历史晋级审批占位行(promotion 包删除后无消费者)。
DELETE FROM run_approvals WHERE stage_id LIKE 'promote:%';

-- ③ 触发器:新增分支过滤白名单列(TEXT 无字面量默认;INSERT 恒显式供值,UPDATE 在此补齐存量)。
ALTER TABLE pipeline_triggers ADD COLUMN branch_filters_json TEXT NOT NULL;

-- ④ 行为保真转换:旧分支映射的 pattern 抽入白名单(脏 JSON / 空 → '[]')。
UPDATE pipeline_triggers pt
   SET pt.branch_filters_json = CASE
       WHEN pt.branch_mappings_json IS NULL
            OR TRIM(pt.branch_mappings_json) = ''
            OR JSON_VALID(pt.branch_mappings_json) = 0 THEN '[]'
       ELSE COALESCE((
           SELECT JSON_ARRAYAGG(JSON_UNQUOTE(JSON_EXTRACT(j.value, '$.branchPattern')))
             FROM JSON_TABLE(pt.branch_mappings_json, '$[*]' COLUMNS (value JSON PATH '$')) j
            WHERE TRIM(COALESCE(JSON_UNQUOTE(JSON_EXTRACT(j.value, '$.branchPattern')), '')) <> ''
       ), '[]')
   END;

-- ⑤ 旧列清理。
ALTER TABLE pipeline_triggers DROP COLUMN branch_mappings_json;
ALTER TABLE pipeline_triggers DROP COLUMN unmatched_policy;
ALTER TABLE pipeline_runs DROP COLUMN resolved_environment;
ALTER TABLE pipeline_runs DROP COLUMN resolved_target_server_ids;
ALTER TABLE pipeline_settings DROP COLUMN environments_json;
