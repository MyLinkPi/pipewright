-- 0086 shared_build(MySQL):集群级共享构建机,语义见 sqlite 同名文件。

ALTER TABLE servers ADD COLUMN shared_build TINYINT NOT NULL DEFAULT 0;

ALTER TABLE mesh_acl ADD COLUMN build_allowed TEXT NOT NULL DEFAULT '[]';
