# EasyTier 第三方二进制说明

本目录的 `windows-<arch>/bin/easytier-core.exe`、`easytier-cli.exe` 是
[EasyTier](https://github.com/EasyTier/EasyTier) 官方 Release 的**未修改**二进制
(版本见 `VERSION`),随 pipewright 发行包再分发。

- 上游许可证:**LGPL-3.0**(全文见 `licenses/LICENSE-LGPL-3.0`,及其基础的
  GPL-3.0 全文 `licenses/LICENSE-GPL-3.0`)
- 对应源码:官方仓库同一 tag(`https://github.com/EasyTier/EasyTier/tree/<VERSION>`)
  按 LGPL-3.0 §4 / GPL-3.0 §6(d) 向接收者公开提供,本包未做任何修改
- pipewright 本体(MIT)以独立进程方式调用上述二进制,不构成链接或衍生

升级方法:`bash scripts/fetch-easytier.sh <新tag>`(可用 `PIPEWRIGHT_RELEASE_MIRROR`
指定镜像下载源)。
