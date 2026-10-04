-- 0063 labels:机器标签登记处(标签字典)。
-- 解决「悬置标签无处可建」:标签可先建后挂——先在登记处建好(`gpu` / `arch=arm64`),
-- 机器后面打上同名标签即入池;各选择器下拉的候选 = 登记处 ∪ 机器实际标签。
-- servers.labels 仍是机器上实际标签的唯一事实来源;本表只是字典:
-- 建标签不作用于任何机器;删标签时若仍被机器引用,由服务层拒绝(labels.ErrInUse)。
CREATE TABLE IF NOT EXISTS labels (
    name       TEXT PRIMARY KEY,
    created_at TEXT NOT NULL
);
