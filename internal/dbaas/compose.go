// compose.go:三引擎的 compose 模板渲染。镜像 tag 来自白名单(见 engineVersions)。
// 口令字符集在 dbaas.go 收紧为 YAML 流序列/shell 双引号均安全的子集
// (字母数字 + _-.~@+=:),注入面在源头关闭;口令仅存在于目标机(与应用商店同安全等级)。
package dbaas

import (
	"fmt"
	"strings"
)

// renderCompose 产出单服务 compose(具名卷持久化 + 健康检查 + unless-stopped)。
// 资源限额由调用方经 composeops.InjectLimits 二次注入。
func renderCompose(engine, version, name string, port int, password string) (string, error) {
	inner, ok := engineInnerPort[engine]
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnsupported, engine)
	}
	vol := "db-" + name + "-data"
	var b strings.Builder
	switch engine {
	case "mysql":
		fmt.Fprintf(&b, mysqlTemplate, "mysql:"+version, password, port, inner, vol, vol)
	case "postgres":
		fmt.Fprintf(&b, postgresTemplate, "postgres:"+version+"-alpine", password, port, inner, vol, vol)
	case "redis":
		fmt.Fprintf(&b, redisTemplate, "redis:"+version+"-alpine", password, port, inner, vol, password, vol)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupported, engine)
	}
	return b.String(), nil
}

const mysqlTemplate = `services:
  db:
    image: %s
    restart: unless-stopped
    environment:
      MYSQL_ROOT_PASSWORD: %s
    ports:
      - "%d:%d"
    volumes:
      - %s:/var/lib/mysql
    healthcheck:
      test: ["CMD", "mysqladmin", "ping", "-h", "127.0.0.1"]
      interval: 10s
      timeout: 5s
      retries: 12
volumes:
  %s:
`

const postgresTemplate = `services:
  db:
    image: %s
    restart: unless-stopped
    environment:
      POSTGRES_PASSWORD: %s
    ports:
      - "%d:%d"
    volumes:
      - %s:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 10s
      timeout: 5s
      retries: 12
volumes:
  %s:
`

// redisTemplate 的健康检查经环境变量 PW 取口令(shell 双引号内 $ 展开;口令字符集
// 不含引号/反斜杠,双引号上下文安全)。
const redisTemplate = `services:
  db:
    image: %s
    restart: unless-stopped
    command: ["redis-server", "--requirepass", "%s", "--appendonly", "yes"]
    ports:
      - "%d:%d"
    volumes:
      - %s:/data
    environment:
      PW: %s
    healthcheck:
      test: ["CMD-SHELL", "redis-cli --no-auth-warning -a \"$PW\" ping"]
      interval: 10s
      timeout: 5s
      retries: 12
volumes:
  %s:
`
