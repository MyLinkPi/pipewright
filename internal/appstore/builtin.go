package appstore

// builtins 是内置应用模板(启动时经 EnsureBuiltins 幂等 seed;同名行已存在则跳过,不覆盖用户数据)。
// compose 里的 {{param}} 占位符在部署时替换;secret 参数空缺自动生成(部署响应一次性返回)。
// 端口默认值避开网关常用 80/443 与常见中间件容器端口冲突由用户按需改。
var builtins = []Template{
	{
		Name:        "mysql",
		DisplayName: "MySQL",
		Description: "关系型数据库;数据落具名卷 mysql-data,重建容器不丢。",
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
		Description: "关系型数据库;数据落具名卷 postgres-data。",
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
		Name:        "minio",
		DisplayName: "MinIO",
		Description: "S3 兼容对象存储;API 与控制台两个端口,数据落具名卷 minio-data。",
		Icon:        "🪣",
		ComposeYAML: `services:
  minio:
    image: minio/minio:{{version}}
    restart: unless-stopped
    command: ["server", "/data", "--console-address", ":9001"]
    environment:
      MINIO_ROOT_USER: {{root_user}}
      MINIO_ROOT_PASSWORD: {{root_password}}
    ports:
      - "{{api_port}}:9000"
      - "{{console_port}}:9001"
    volumes:
      - minio-data:/data
volumes:
  minio-data:
`,
		Params: []ParamSpec{
			{Name: "version", Label: "镜像版本", Type: ParamTypeString, Default: "latest"},
			{Name: "root_user", Label: "root 用户名", Type: ParamTypeString, Default: "minioadmin"},
			{Name: "root_password", Label: "root 密码", Type: ParamTypeSecret, Required: true, AutoGenerate: true},
			{Name: "api_port", Label: "API 端口", Type: ParamTypeInt, Default: "9000"},
			{Name: "console_port", Label: "控制台端口", Type: ParamTypeInt, Default: "9001"},
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
}
