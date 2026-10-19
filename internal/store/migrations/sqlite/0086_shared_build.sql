-- 0086 shared_build(SQLite):集群级共享构建机(经 mesh 把本端构建机共享给集群其他环境执行构建)。
--   servers.shared_build:共享标记 —— 开启后该机(kind=standard 且已打构建机标签)进入本端
--     共享构建机目录(/api/mesh/build/catalog),集群中经 build_allowed 授权的对端环境可用
--     `peer:<本端名>` 选择器把构建任务派到该机;本端构建/部署行为不变(部署目标选择不受影响,
--     对端只能构建、不能部署)。SSH 凭据仍只由本端解密使用,任务的工作区与产物仅经本端中转
--     暂存(OS 临时文件,拉完即删),绝不进本端制品库。
--   mesh_acl.build_allowed:第五张默认全拒绝授权单 —— 允许对端节点拉取我的共享构建机目录
--     并提交共享构建任务(进入本端构建机调度器的同一 FIFO,与本端任务统一排队)。

ALTER TABLE servers ADD COLUMN shared_build INTEGER NOT NULL DEFAULT 0;

ALTER TABLE mesh_acl ADD COLUMN build_allowed TEXT NOT NULL DEFAULT '[]';
