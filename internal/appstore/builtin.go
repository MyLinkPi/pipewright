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
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "28.1.0"},
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
}

// retiredBuiltins 是已从商店下架的内置模板名:EnsureBuiltins 时对库中的内置行删除,
// 使老环境升级后不再展示(如 MinIO 官方已停止维护社区版镜像);同名自定义模板不受影响。
var retiredBuiltins = []string{"minio"}
