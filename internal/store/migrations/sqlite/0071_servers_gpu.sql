-- 0071 servers.gpu:该机带 GPU(显卡机型)标记。
-- 勾选后多机状态总览(FR-15)对该机额外跑一次 `nvtop -s` 采集显卡指标(每卡利用率/显存/
-- 温度/功耗,支持多卡,兼容 NVIDIA 与 AMD)。纯**监控**开关:不参与构建机池调度
-- (调度仍看 labels/槽位/优先级),不影响部署目标选择。默认 0(关闭)→ 老行为不变。
ALTER TABLE servers ADD COLUMN gpu INTEGER NOT NULL DEFAULT 0;