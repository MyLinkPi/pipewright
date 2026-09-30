-- 0052 runner pool:多节点构建机池 + 标签调度(FR-8-14 续 / FR-8-19)。
-- servers 增三列,把「构建机」从"项目绑定单台服务器"升级为"打了标签的服务器池":
--   labels    : 逗号分隔标签项(纯 tag 如 `linux`,或 `k=v` 如 `arch=arm64`)。空标签不匹配任何
--               选择器 → 未打标签的服务器永远不会被当作构建机选中(无需单独的启用开关)。
--   max_builds: 该机并发构建槽位。0 = 用全局默认(PIPEWRIGHT_RUNNER_SLOTS,默认 1)。
--   priority  : 调度优先级,0-100,数值越大越优先;同优先级再看流水线亲和与负载。
-- project_runners 增 selector 列(标签选择器表达式,如 `linux,arch=arm64`;特殊形式 `server:<id>`
-- 钉死单机 = 旧 runner_server_id 的规范形式)并回填存量行;旧列保留不删,读取以 selector 为准。
-- 空 selector = 本地构建(行为不变)。
ALTER TABLE servers ADD COLUMN labels TEXT NOT NULL DEFAULT '';
ALTER TABLE servers ADD COLUMN max_builds INTEGER NOT NULL DEFAULT 1;
ALTER TABLE servers ADD COLUMN priority INTEGER NOT NULL DEFAULT 0;
ALTER TABLE project_runners ADD COLUMN selector TEXT NOT NULL DEFAULT '';
UPDATE project_runners SET selector = 'server:' || runner_server_id WHERE runner_server_id <> '';
