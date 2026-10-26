-- 0105 projects.pac_file(MySQL):每项目「流水线即代码」文件路径(语义同 sqlite 版)。
ALTER TABLE projects ADD COLUMN pac_file VARCHAR(200) NOT NULL DEFAULT '';
