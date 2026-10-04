-- 0063 labels(MySQL):机器标签登记处(标签字典)。语义见 sqlite 同名文件。
CREATE TABLE IF NOT EXISTS labels (
    name       VARCHAR(128) PRIMARY KEY,
    created_at VARCHAR(32) NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
