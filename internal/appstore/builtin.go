package appstore

// builtins 是内置应用模板(启动时经 EnsureBuiltins 幂等 seed;同名行已存在则跳过,不覆盖用户数据)。
// compose 里的 {{param}} 占位符在部署时替换;secret 参数空缺自动生成(部署响应一次性返回)。
// 端口默认值避开网关常用 80/443 与常见中间件容器端口冲突由用户按需改。
var builtins = []Template{
	{
		Name:        "mysql",
		DisplayName: "MySQL",
		Description: "关系型数据库;数据落具名卷 mysql-data,重建容器不丢。部署成功后自动登记到「设置 → 数据库」,可在「数据库」页在线管理。",
		Icon:        "🐬",
		ComposeYAML: `services:
  mysql:
    image: mysql:{{version}}
    restart: unless-stopped
    environment:
      MYSQL_ROOT_PASSWORD: {{root_password}}
    ports:
      - "{{port}}:3306"
    volumes:
      - mysql-data:/var/lib/mysql
volumes:
  mysql-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "8.4"},
			{Name: "root_password", Label: "root 密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "3306"},
		},
		Builtin: true,
	},
	{
		Name:        "redis",
		DisplayName: "Redis",
		Description: "内存 KV 缓存;AOF 持久化落具名卷 redis-data。",
		Icon:        "🟥",
		ComposeYAML: `services:
  redis:
    image: redis:{{version}}
    restart: unless-stopped
    command: ["redis-server", "--requirepass", "{{password}}", "--appendonly", "yes"]
    ports:
      - "{{port}}:6379"
    volumes:
      - redis-data:/data
volumes:
  redis-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "7-alpine"},
			{Name: "password", Label: "访问密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "6379"},
		},
		Builtin: true,
	},
	{
		Name:        "postgres",
		DisplayName: "PostgreSQL",
		Description: "关系型数据库;数据落具名卷 postgres-data。部署成功后自动登记到「设置 → 数据库」,可在「数据库」页在线管理。",
		Icon:        "🐘",
		ComposeYAML: `services:
  postgres:
    image: postgres:{{version}}
    restart: unless-stopped
    environment:
      POSTGRES_PASSWORD: {{password}}
    ports:
      - "{{port}}:5432"
    volumes:
      - postgres-data:/var/lib/postgresql/data
volumes:
  postgres-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "16-alpine"},
			{Name: "password", Label: "postgres 密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "5432"},
		},
		Builtin: true,
	},
	{
		Name:        "nginx-web",
		DisplayName: "Nginx 静态站",
		Description: "静态网站/反代底座;把站点内容放到 /opt/<host>/nginx-html 后自行挂载或改 compose。",
		Icon:        "🌐",
		ComposeYAML: `services:
  nginx:
    image: nginx:{{version}}
    restart: unless-stopped
    ports:
      - "{{port}}:80"
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "stable-alpine"},
			{Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "8080", Required: true},
		},
		Builtin: true,
	},
	{
		Name:        "rustfs",
		DisplayName: "RustFS",
		Description: "Rust 高性能 S3 兼容对象存储(含控制台);部署成功后自动登记到「设置 → 对象存储」。",
		Icon:        "🦀",
		ComposeYAML: `services:
  rustfs:
    image: rustfs/rustfs:{{version}}
    restart: unless-stopped
    command: ["/data"]
    environment:
      RUSTFS_ACCESS_KEY: {{access_key}}
      RUSTFS_SECRET_KEY: {{secret_key}}
      RUSTFS_ADDRESS: ":9000"
      RUSTFS_CONSOLE_ADDRESS: ":9001"
      RUSTFS_CONSOLE_ENABLE: "true"
    ports:
      - "{{api_port}}:9000"
      - "{{console_port}}:9001"
    volumes:
      - rustfs-data:/data
volumes:
  rustfs-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "access_key", Label: "Access Key", Type: ParamTypeString, Default: "rustfsadmin"},
			{Name: "secret_key", Label: "Secret Key", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "api_port", Label: "API 端口", Type: ParamTypeInt, Default: "9000"},
			{Name: "console_port", Label: "控制台端口", Type: ParamTypeInt, Default: "9001"},
			// bucket 不进 compose,仅供部署后自动登记对象存储配置时读取(Render 会放入 Used)。
			{Name: "bucket", Label: "自动登记的桶名", Type: ParamTypeString, Default: "pipewright"},
		},
		Builtin: true,
	},
	{
		Name:        "rabbitmq",
		DisplayName: "RabbitMQ",
		Description: "消息队列(含管理插件);数据落具名卷 rabbitmq-data。",
		Icon:        "🐰",
		ComposeYAML: `services:
  rabbitmq:
    image: rabbitmq:{{version}}
    restart: unless-stopped
    environment:
      RABBITMQ_DEFAULT_USER: {{user}}
      RABBITMQ_DEFAULT_PASS: {{password}}
    ports:
      - "{{amqp_port}}:5672"
      - "{{mgmt_port}}:15672"
    volumes:
      - rabbitmq-data:/var/lib/rabbitmq
volumes:
  rabbitmq-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "3-management"},
			{Name: "user", Label: "用户名", Type: ParamTypeString, Default: "admin"},
			{Name: "password", Label: "密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "amqp_port", Label: "AMQP 端口", Type: ParamTypeInt, Default: "5672"},
			{Name: "mgmt_port", Label: "管理界面端口", Type: ParamTypeInt, Default: "15672"},
		},
		Builtin: true,
	},
	{
		Name:        "gitea",
		DisplayName: "Gitea",
		Description: "轻量 Git 代码托管(含 Actions CI);SQLite 起步,数据与配置全落具名卷 gitea-data,SSH 克隆端口默认 2222 避开宿主 22;首启经 Web 页完成初始化。",
		Icon:        "🍵",
		// rootful 镜像:容器内 sshd 恒听 22(SSH_LISTEN_PORT 默认跟随 SSH_PORT,须显式钉住),
		// SSH_PORT 只决定克隆地址展示的端口(= 宿主映射端口),SSH_DOMAIN 决定克隆地址主机
		//(缺省回落 localhost,首启安装页也带不出正确值,故必填;部署 handler 对留空的
		// 宿主地址约定参数自动注入目标机 Host);ROOT_URL 不预设,留给首启安装页按浏览器
		// 请求自动探测,避免预设错误值覆盖。
		ComposeYAML: `services:
  gitea:
    image: gitea/gitea:{{version}}
    restart: unless-stopped
    environment:
      USER_UID: "1000"
      USER_GID: "1000"
      GITEA__server__SSH_DOMAIN: "{{ssh_domain}}"
      GITEA__server__SSH_PORT: "{{ssh_port}}"
      GITEA__server__SSH_LISTEN_PORT: "22"
    ports:
      - "{{port}}:3000"
      - "{{ssh_port}}:22"
    volumes:
      - gitea-data:/data
volumes:
  gitea-data:
`,
		Params: []ParamSpec{
			// 默认 latest:Gitea 升级自动跑单向迁移,回退本就只能靠升级前备份,钉旧版
			// 攒下的安全漏洞比迁移风险更致命;要控制升级节奏仍可显式改版本号。
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			// port 命名沿用其他模板约定:一键服务注册的端口启发式优先取键 port(Web 才是反代目标)。
			{Name: "port", Label: "Web 端口(宿主)", Type: ParamTypeInt, Default: "3000"},
			{Name: "ssh_port", Label: "SSH 克隆端口(宿主)", Type: ParamTypeInt, Default: "2222"},
			{Name: "ssh_domain", Label: "SSH 克隆域名(服务器 IP 或域名)", Type: ParamTypeString, Required: true},
		},
		Builtin: true,
	},
	{
		Name:        "redpanda",
		DisplayName: "Redpanda",
		Description: "Kafka 协议兼容的高性能流数据平台;附带 Console 管理界面,数据落具名卷 redpanda-data。",
		Icon:        "🐼",
		// 单节点开发者模式:--overprovisioned + --mode dev-container 免去生产级的
		// 内核参数与资源检查,普通容器即可跑通;advertise 地址必须是客户端可达的
		// 宿主地址(部署 handler 对 advertise_host 参数自动注入机器配置的 Host),
		// Console 为独立镜像,经容器网络连 broker。
		ComposeYAML: `services:
  redpanda:
    image: redpandadata/redpanda:{{version}}
    restart: unless-stopped
    command:
      - redpanda
      - start
      - --overprovisioned
      - --mode dev-container
      - --smp 1
      - --memory 1G
      - --kafka-addr internal://0.0.0.0:9092
      - --advertise-kafka-addr internal://{{advertise_host}}:{{kafka_port}}
      - --schema-registry-addr internal://0.0.0.0:8081
    ports:
      - "{{kafka_port}}:9092"
      - "{{schema_port}}:8081"
    volumes:
      - redpanda-data:/var/lib/redpanda/data
  console:
    image: redpandadata/console:{{console_version}}
    restart: unless-stopped
    depends_on:
      - redpanda
    environment:
      KAFKA_BROKERS: redpanda:9092
    ports:
      - "{{console_port}}:8080"
volumes:
  redpanda-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			// advertise_host 留空则由部署 handler 自动注入目标机的 Host 地址;
			// 默认 127.0.0.1 兜底本机部署场景。
			{Name: "advertise_host", Label: "客户端通告地址(Kafka advertise)", Type: ParamTypeString, Default: "127.0.0.1"},
			{Name: "console_version", Label: "Console 镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "kafka_port", Label: "Kafka API 端口", Type: ParamTypeInt, Default: "9092"},
			{Name: "schema_port", Label: "Schema Registry 端口", Type: ParamTypeInt, Default: "8081"},
			{Name: "console_port", Label: "Console 管理界面端口", Type: ParamTypeInt, Default: "8080"},
		},
		Builtin: true,
	},
	{
		Name:        "nfs",
		DisplayName: "NFS Server",
		Description: "NFSv4 文件共享服务(仅需 2049 端口);客户端 mount -t nfs4 <宿主IP>:/ 挂载;数据落具名卷 nfs-data;容器需特权模式,宿主机内核需支持 nfsd。",
		Icon:        "📁",
		// itsthenetwork/nfs-server-alpine 仅支持 NFSv4(免 rpcbind,只暴露 2049/TCP),
		// 容器内要写 /etc/exports 并驱动宿主 nfsd 内核线程,必须 privileged;
		// PERMITTED 为客户端白名单(* 或 IP/CIDR,空格分隔),READ_ONLY 控制只读。
		ComposeYAML: `services:
  nfs:
    image: itsthenetwork/nfs-server-alpine:{{version}}
    restart: unless-stopped
    privileged: true
    environment:
      SHARED_DIRECTORY: /data/shared
      PERMITTED: "{{permitted}}"
      READ_ONLY: "{{read_only}}"
      SYNC: "true"
    ports:
      - "{{port}}:2049"
    volumes:
      - nfs-data:/data/shared
volumes:
  nfs-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "permitted", Label: "允许的客户端(* 或 IP/CIDR,空格分隔)", Type: ParamTypeString, Default: "*"},
			{Name: "read_only", Label: "只读共享(true/false)", Type: ParamTypeString, Default: "false"},
			{Name: "port", Label: "宿主端口", Type: ParamTypeInt, Default: "2049"},
		},
		Builtin: true,
	},
	{
		Name:        "openvas",
		DisplayName: "OpenVAS",
		Description: "开源漏洞扫描平台(GVM:扫描器 + Web 控制台单容器);Web 界面走 https://<宿主IP>:<端口>(自签证书,浏览器需手动信任)。宿主机需先执行 sysctl vm.overcommit_memory=1(vm.* 非命名空间化,容器内改不了)并预留 ≥4GB 空闲内存:首启同步 NVT 特征库内存峰值高、可达数十分钟,内存不足会被 OOM 杀成启动循环,期间勿反复重启容器;数据库与特征库落具名卷 openvas-data。",
		Icon:        "🛡️",
		// immauss/openvas 是社区维护的单容器 GVM 全栈(gsad/gvmd/ospd-openvas + 内置
		// postgres/redis),免多容器拼装;USERNAME/PASSWORD 即 Web 登录凭据(PASSWORD
		// 空缺会回落镜像默认 admin,故必填并自动生成);数据与已同步特征库全在
		// /var/lib/openvas,重建容器不重拉特征库。
		ComposeYAML: `services:
  openvas:
    image: immauss/openvas:{{version}}
    restart: unless-stopped
    environment:
      USERNAME: {{username}}
      PASSWORD: {{password}}
    ports:
      - "{{port}}:9392"
    volumes:
      - openvas-data:/var/lib/openvas
volumes:
  openvas-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "username", Label: "Web 登录用户名", Type: ParamTypeString, Default: "admin"},
			{Name: "password", Label: "Web 登录密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "port", Label: "Web 端口(宿主)", Type: ParamTypeInt, Default: "9392"},
		},
		Builtin: true,
	},
	{
		Name:        "trivy-server",
		DisplayName: "Trivy Server",
		Description: "安全中心的中心漏洞库服务:各目标机 Trivy 扫描的漏洞匹配都走这台,免每台机器各自下载漏洞库;部署本应用后,没有 docker 的目标机也能做主机漏洞扫描(包管理元数据经面板中继、由本机代跑)。部署成功后自动登记到「安全中心」;访问令牌默认自动生成,也可手动指定。注意:首启需先下载漏洞库(数百 MB)才开始监听端口,期间连接被拒绝属预期;漏洞库默认仓库 ghcr.io 在部分网络不可达时,可把「漏洞库仓库」换成镜像地址或填出网代理。漏洞库缓存落具名卷 trivy-cache,重建容器不重下。",
		Icon:        "🔍",
		// trivy server 是 Trivy 的 server 模式:启动先下载/更新漏洞库,完成后才开始监听
		// (首启期间端口连接被拒绝是预期,连接测试兜底);客户端经 --server 复用;--token
		// 开启访问鉴权(默认自动生成,自动登记时经 vault 密封)。TRIVY_DB_REPOSITORY 支持
		// 逗号分隔多仓库逐个回退,默认官方仓库,国内可换镜像;HTTPS_PROXY 留空 = 不走代理
		// (Go 语义:空串等价未设置)。
		ComposeYAML: `services:
  trivy-server:
    image: aquasec/trivy:{{version}}
    restart: unless-stopped
    command:
      - server
      - --listen
      - 0.0.0.0:{{port}}
      - --token
      - {{token}}
    environment:
      TRIVY_DB_REPOSITORY: "{{db_repository}}"
      HTTPS_PROXY: "{{https_proxy}}"
    ports:
      - "{{port}}:{{port}}"
    volumes:
      - trivy-cache:/root/.cache/trivy
volumes:
  trivy-cache:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "port", Label: "服务端口(宿主=容器)", Type: ParamTypeInt, Default: "4954"},
			{Name: "token", Label: "访问令牌", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "db_repository", Label: "漏洞库仓库(默认官方;可换镜像,逗号分隔逐个回退)", Type: ParamTypeString, Default: "ghcr.io/aquasecurity/trivy-db"},
			{Name: "https_proxy", Label: "出网代理(可选,如 http://proxy:7890;留空不走代理)", Type: ParamTypeString, Default: ""},
		},
		Builtin: true,
	},
	{
		Name:        "nuclei-runner",
		DisplayName: "Nuclei Runner",
		Description: "安全中心的常驻 Nuclei 执行容器:部署在任意有 docker 且与目标服务器网络互通的机器上,部署成功后自动登记到「安全中心」;此后所有 Nuclei 暴露面扫描都经它执行,目标机无需安装 docker。特征模板落具名卷 nuclei-templates(首次扫描自动下载,之后复用)。无对外端口,面板经 docker exec 驱动。",
		Icon:        "🎯",
		// nuclei 无 server 模式(纯 CLI),常驻形态 = 睡眠容器承载二进制与模板缓存:
		// entrypoint 覆写为 sleep infinity 保活,面板经 SSH 在该主机 docker exec 容器内
		// 跑 nuclei;特征模板在卷里跨扫描复用,目标机零依赖。
		ComposeYAML: `services:
  nuclei-runner:
    image: projectdiscovery/nuclei:{{version}}
    restart: unless-stopped
    entrypoint: ["sleep", "infinity"]
    volumes:
      - nuclei-templates:/root/nuclei-templates
volumes:
  nuclei-templates:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
		},
		Builtin: true,
	},
	{
		Name:        "safeline",
		DisplayName: "SafeLine WAF",
		Description: "雷池社区版 WAF(语义分析引擎,低误报):部署成功后自动登记到「安全中心」,在雷池控制台生成 API Token 并补填到实例后,即可在安全中心看到拦截态势;证书在「证书 → 部署目标」新建 SafeLine 类型目标即可自动同步(续期自动重推)。注意:tengine 独占宿主 80/443(network_mode host),须用独立边缘机;反代站点在雷池控制台手动接入(上游指向平台网关或后端),灰度切流用 DNS 控制。数据全落具名卷。",
		Icon:        "🛡",
		// 官方 compose 的参数化转写(chaitin/safeline 仓库 compose.yaml):SUBNET_PREFIX
		// 固定 172.22.34(内部 mgt/detector/tengine 按固定 IP 互联);SAFELINE_DIR 的
		// bind 目录改为具名卷;POSTGRES_PASSWORD 自动生成;管理台端口参数化(容器内
		// 恒 1443)。tengine host 网络直接监听宿主 80/443(WAF 必须是流量入口)。
		ComposeYAML: `services:
  safeline-pg:
    image: chaitin/safeline-postgres:{{pg_version}}
    restart: always
    environment:
      POSTGRES_USER: safeline-ce
      POSTGRES_PASSWORD: "{{db_password}}"
    volumes:
      - safeline-pg:/var/lib/postgresql/data
      - /etc/localtime:/etc/localtime:ro
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.2

  safeline-mgt:
    image: chaitin/safeline-mgt:latest
    restart: always
    depends_on:
      - safeline-pg
    volumes:
      - /etc/localtime:/etc/localtime:ro
      - safeline-mgt:/app/data
      - safeline-logs-nginx:/app/log/nginx
      - safeline-sock:/app/sock
      - /var/run:/app/run
    environment:
      MGT_PG: postgres://safeline-ce:{{db_password}}@safeline-pg/safeline-ce?sslmode=disable
    ports:
      - "{{port}}:1443"
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.4

  safeline-detector:
    image: chaitin/safeline-detector:latest
    restart: always
    environment:
      LOG_DIR: /logs/detector
    volumes:
      - safeline-detector:/resources/detector
      - safeline-logs-detector:/logs/detector
      - /etc/localtime:/etc/localtime:ro
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.5

  safeline-tengine:
    image: chaitin/safeline-tengine:latest
    restart: always
    ulimits:
      nofile:
        soft: 131072
        hard: 131072
    network_mode: host
    environment:
      TCD_MGT_API: https://172.22.34.4:1443/api/open/publish/server
      TCD_SNSERVER: 172.22.34.5:8000
      CHAOS_ADDR: 172.22.34.10
    volumes:
      - /etc/localtime:/etc/localtime:ro
      - /etc/resolv.conf:/etc/resolv.conf:ro
      - safeline-nginx:/etc/nginx
      - safeline-detector:/resources/detector
      - safeline-logs-nginx:/var/log/nginx
      - safeline-cache:/usr/local/nginx/cache
      - safeline-sock:/app/sock
    depends_on:
      - safeline-detector

  safeline-luigi:
    image: chaitin/safeline-luigi:latest
    restart: always
    depends_on:
      - safeline-detector
      - safeline-mgt
    environment:
      MGT_IP: 172.22.34.4
      LUIGI_PG: postgres://safeline-ce:{{db_password}}@safeline-pg/safeline-ce?sslmode=disable
    volumes:
      - /etc/localtime:/etc/localtime:ro
      - safeline-luigi:/app/data
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.7

  safeline-fvm:
    image: chaitin/safeline-fvm:latest
    restart: always
    volumes:
      - /etc/localtime:/etc/localtime:ro
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.8

  safeline-chaos:
    image: chaitin/safeline-chaos:latest
    restart: always
    environment:
      DB_ADDR: postgres://safeline-ce:{{db_password}}@safeline-pg/safeline-ce?sslmode=disable
    volumes:
      - safeline-sock:/app/sock
      - safeline-chaos:/app/chaos
    networks:
      safeline-ce:
        ipv4_address: 172.22.34.10

volumes:
  safeline-pg:
  safeline-mgt:
  safeline-detector:
  safeline-nginx:
  safeline-cache:
  safeline-sock:
  safeline-luigi:
  safeline-chaos:
  safeline-logs-nginx:
  safeline-logs-detector:

networks:
  safeline-ce:
    name: safeline-ce
    driver: bridge
    ipam:
      config:
        - subnet: 172.22.34.0/24
          gateway: 172.22.34.1
`,
		Params: []ParamSpec{
			{Name: "port", Label: "管理台端口(宿主)", Type: ParamTypeInt, Default: "9443"},
			{Name: "db_password", Label: "数据库密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "pg_version", Label: "Postgres 镜像版本", Type: ParamTypeString, Default: "15.2"},
		},
		Builtin: true,
	},
	{
		Name:        "playwright",
		DisplayName: "Playwright",
		Description: "常驻 headless Chromium 的 CDP 远程浏览器服务:外部工具(Playwright/Puppeteer 等)经 connectOverCDP(\"http://<宿主IP>:<端口>\")远程驱动浏览器,适合浏览器自动化、抓取与 AI Agent 接管。注意:CDP 协议本身无鉴权,端口务必只暴露在可信内网/本机。浏览器 Profile(/srv/pw/profile)落具名卷 playwright-data,重建容器不丢。",
		Icon:        "🎭",
		// 官方镜像自带全量浏览器,但没有现成的 CDP 服务入口:须以裸进程拉起浏览器并开
		// --remote-debugging。关键坑:v1.5x 起 chrome 与 headless shell 各二进制都无视
		// --remote-debugging-address、只绑容器内回环(v1.65.1 双二进制实测:宿主 curl 被
		// 重置,日志恒打印 ws://127.0.0.1)。故改为「回环 + 转发」架构:浏览器统一起在
		// 容器内回环 9333(回环绑定是各版本共同保证的行为),由镜像自带 node 把
		// 0.0.0.0:9222 转发到它,对 docker 端口映射暴露;while 循环保活浏览器(node 前台
		// 守护整体生命周期,挂了触发容器重启策略)。转发必须是 HTTP 感知代理而非裸 TCP
		// 管道:Chromium DevTools 有 DNS-rebinding 防护(RequestIsSafeToServe),Host 头
		// 非 IP/localhost 一律 500 拒绝,且 webSocketDebuggerUrl 直接按 Host 头拼
		// (ws://<Host><guid>)。故代理把上游 Host 改写为 127.0.0.1:9333 过校验,再把
		// /json 响应里的 ws://127.0.0.1:9333 回写成客户端原始外部地址,WebSocket upgrade
		// 同样改写 Host 后双向 pipe。注意:compose 会对 command 值做变量
		// 插值,shell 变量必须写成 $$BIN(单 $ 会被 compose 抹成空串,实战踩坑);
		// $(find) 命令替换不在插值语法内,保持单 $;--remote-allow-origins 的 * 须加引号
		// 防 shell 展开。找不到二进制时打印镜像内 /ms-playwright 清单再退出,便于日志
		// 直排根因。user: root 是因官方镜像默认 pwuser 对新建具名卷无写权限(root 跑
		// chromium 须配 --no-sandbox);--init 收尸 + --shm-size 1g 防默认 /dev/shm 64MB
		// 崩浏览器(同 internal/build/shell_driver.go 的结论)。代理必须给小写:
		// chromium 只读 http_proxy/https_proxy(见 chromium(1) ENVIRONMENT),大写仅供
		// 镜像内 npm/node 工具链;同值、留空 = 不走代理。
		ComposeYAML: `services:
  playwright:
    image: mcr.microsoft.com/playwright:{{version}}
    restart: unless-stopped
    user: root
    init: true
    shm_size: "1g"
    environment:
      HTTP_PROXY: "{{proxy}}"
      HTTPS_PROXY: "{{proxy}}"
      http_proxy: "{{proxy}}"
      https_proxy: "{{proxy}}"
    command:
      - /bin/sh
      - -c
      - |
        BIN="$(find /ms-playwright -type f \( -name chrome-headless-shell -o -name headless_shell \) 2>/dev/null | head -n 1)"
        [ -n "$$BIN" ] || BIN="$(find /ms-playwright -type f -name chrome 2>/dev/null | head -n 1)"
        if [ -z "$$BIN" ]; then
          echo "未在镜像内定位到浏览器;/ms-playwright 实际内容如下,请反馈此清单:" >&2
          ls -la /ms-playwright >&2
          exit 1
        fi
        (while true; do
          "$$BIN" \
            --no-sandbox \
            --remote-allow-origins="*" \
            --remote-debugging-port=9333 \
            --user-data-dir=/srv/pw/profile
          sleep 1
        done) &
        exec node -e 'const http=require("http"),net=require("net"),UP=9333,LP=9222;
        const srv=http.createServer((req,res)=>{
          const oh=req.headers.host||("127.0.0.1:"+LP);
          const up=http.request({hostname:"127.0.0.1",port:UP,path:req.url,method:req.method,headers:Object.assign({},req.headers,{host:"127.0.0.1:"+UP})},(ur)=>{
            if(req.url.startsWith("/json")){
              let cs=[];ur.on("data",d=>cs.push(d));ur.on("end",()=>{
                const s=Buffer.concat(cs).toString("utf8").split("ws://127.0.0.1:"+UP).join("ws://"+oh);
                const h=Object.assign({},ur.headers);delete h["content-length"];delete h["transfer-encoding"];
                res.writeHead(ur.statusCode,h);res.end(s);
              });
            }else{res.writeHead(ur.statusCode,ur.headers);ur.pipe(res);}
          });
          up.on("error",()=>{if(!res.headersSent){res.writeHead(502);res.end("upstream error")}});
          req.pipe(up);
        });
        srv.on("upgrade",(req,sock,head)=>{
          const up=net.connect(UP,"127.0.0.1",()=>{
            let l=req.method+" "+req.url+" HTTP/1.1\r\n";
            const h=Object.assign({},req.headers,{host:"127.0.0.1:"+UP});
            for(const k in h)l+=k+": "+h[k]+"\r\n";
            l+="\r\n";up.write(l);if(head&&head.length)up.write(head);
            sock.pipe(up).pipe(sock);
          });
          up.on("error",()=>sock.destroy());sock.on("error",()=>up.destroy());
        });
        srv.listen(LP,"0.0.0.0");'
    ports:
      - "{{port}}:9222"
    volumes:
      - playwright-data:/srv/pw
volumes:
  playwright-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "v1.65.1-noble"},
			{Name: "port", Label: "CDP 端口(宿主)", Type: ParamTypeInt, Default: "9222"},
			{Name: "proxy", Label: "出网代理(可选,如 http://proxy:7890;留空不走代理)", Type: ParamTypeString, Default: ""},
		},
		Builtin: true,
	},
}

// retiredBuiltins 是已从商店下架的内置模板名:EnsureBuiltins 时对库中的内置行删除,
// 使老环境升级后不再展示(如 MinIO 官方已停止维护社区版镜像);同名自定义模板不受影响。
var retiredBuiltins = []string{"minio"}
