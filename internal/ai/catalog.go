package ai

// 节点目录(AI 生成流水线的「可用工具清单」)。
//
// 痛点:旧 prompt 只举 git_source/build_image/deploy 三种 type,LLM 不知道产品其余节点存在,
// 只会产最粗的三段式。这里把**全部可用节点**作为工具清单喂给 LLM:内置节点(下方 Go 目录,
// 新增模板节点在此登记即自动生效)+ 复用库里的用户自定义节点(运行时从 DB 动态拼入,见
// httpapi 装配)。目录与前端 jobConfigSchema 的节点种类对齐,改动时请同步两侧。

// NodeKind 是一个可用节点的描述(喂给 LLM 的工具条目)。
type NodeKind struct {
	Type        string // job.type(LLM 只能从目录里选)
	Label       string // 人读名
	Category    string // source|build|deploy|quality|notify|custom
	Description string // 用途 + 关键配置/适用场景(给 LLM 判断何时用)
	Custom      bool   // true = 复用库里的用户自定义节点(按 Label 名称选用,type 通常为 templated)
}

// BuiltinNodeCatalog 返回内置节点的工具清单(新增内置/模板节点在此登记即生效)。
// 顺序按典型流水线推进(源 → 构建 → 部署 → 通知),便于 LLM 组合。
func BuiltinNodeCatalog() []NodeKind {
	return []NodeKind{
		{Type: "git_source", Label: "Gitee 源", Category: "source",
			Description: "拉取 Git 仓库源码到构建工作区。每条流水线必须恰有一个 source 阶段,含一个 git_source。"},
		{Type: "build_nodejs", Label: "Node.js 构建", Category: "build",
			Description: "node 容器内装依赖并构建,产出 dist。适合含 package.json 的前端/Node 服务。默认 image=node:20、npm ci && npm run build。"},
		{Type: "build_java", Label: "Java 构建", Category: "build",
			Description: "Maven 容器内打包,产出 jar。适合含 pom.xml 的后端(Gradle 项目改 image 为 gradle 镜像、命令为 gradle build)。"},
		{Type: "build_golang", Label: "Golang 构建", Category: "build",
			Description: "golang 容器内编译,产出二进制。默认 image=golang:1.22、CGO_ENABLED=0 go build -o bin/app ./...。"},
		{Type: "build_python", Label: "Python 构建", Category: "build",
			Description: "python 容器内装依赖并构建,产出 wheel/sdist。默认 image=python:3.12、pip install -r requirements.txt && python -m build。"},
		{Type: "build_image", Label: "构建镜像", Category: "build",
			Description: "用 Dockerfile 或工具链构建 Docker 镜像(产物=image)。有 Dockerfile 时优先用它。"},
		{Type: "push_image", Label: "推送镜像", Category: "build",
			Description: "把构建出的镜像推送到镜像仓库。通常紧随 build_image,部署 image 产物前需要。"},
		{Type: "script", Label: "自定义脚本", Category: "build",
			Description: "隔离容器内执行任意命令(跑测试、lint、代码扫描、自定义步骤等)。"},
		{Type: "deploy_ssh", Label: "主机部署", Category: "deploy",
			Description: "经 SSH 把文件产物(jar/dist)滚动部署到目标机:releases/<runId>/ + 原子 current 软链 + 重启命令 + 健康门控 + 失败自动回滚。config 必填 artifactJob(产物来源节点名);多机支持 firstBatchSize/batchSize 分批。"},
		{Type: "deploy_container", Label: "容器部署", Category: "deploy",
			Description: "经 SSH 在目标机部署镜像产物:docker login(可选)→ pull → 停旧容器 → run 新容器(ports + 结构化常用项 cpuLimit/memoryLimit/restartPolicy/envVars,或 runArgs 自由参数)→ 健康门控 → 失败回滚上一镜像。config 必填 artifactJob(指向 build_image 节点)、containerName。"},
		{Type: "health_check", Label: "健康检查", Category: "deploy",
			Description: "对目标机(selector/serverId 圈选)做健康探测(http=curl / command=自定义命令,带重试);失败令阶段失败。建议接在部署节点之后。"},
		{Type: "notify", Label: "通知", Category: "notify",
			Description: "运行到此节点时向已配渠道(飞书/Webhook/邮件)发通知,支持标题/正文模板。"},
		{Type: "templated", Label: "自定义节点", Category: "custom",
			Description: "用户自定义节点:参数 + 命令模板({{参数}}),用于产品未内置的步骤。"},
	}
}
