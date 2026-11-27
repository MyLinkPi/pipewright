# AI 测试使用指南

AI 测试会读取仓库中的 Markdown 测试需求，直接操作网页或调用接口，并给出测试结果。你只需要用自然语言写清楚“要验证什么、怎样算通过”，不需要编写自动化测试脚本。

## 1. 编写测试需求

### 1.1 创建需求文件

在仓库中创建 `.pipewright/tests/requirements/` 目录，每个 Markdown 文件描述一个测试场景：

```text
.pipewright/
└── tests/
    └── requirements/
        ├── web/
        │   └── checkout.md
        └── api/
            └── coupon.md
```

目录可以按业务或测试范围自由划分。建议一个文件只验证一个完整场景，并让它可以独立执行。

### 1.2 写清楚操作和预期结果

需求正文使用自然语言，至少写清楚：

- **前置条件**：使用什么账号、准备什么数据、从哪个页面或接口开始。
- **操作步骤**：需要执行哪些操作。
- **预期结果**：页面应出现什么内容，或接口应返回什么状态和字段。
- **清理方式**：如果会创建订单、资源或其他数据，说明测试结束后如何清理。

例如，`.pipewright/tests/requirements/web/checkout.md`：

```markdown
使用测试账号登录商城，将任一在售商品加入购物车并进入结算页。

1. 输入有效优惠券 COUPON-6，点击“使用”。
2. 确认订单金额减少 6 元。
3. 再输入无效优惠券 INVALID，点击“使用”。

预期结果：
- 使用 COUPON-6 后，页面显示优惠金额为 6 元，订单合计相应减少。
- 使用 INVALID 后，页面提示“券码无效”，订单合计不变。

测试完成后取消订单，不提交真实支付。
```

描述越明确，AI 越容易稳定执行。不要只写“检查优惠券功能是否正常”，而应写出可观察、可判断的结果。

### 1.3 按需添加文件头

文件顶部可以添加 YAML frontmatter；没有特殊需求时可以完全省略：

```markdown
---
id: checkout-coupon
severity: blocker
target: https://staging.example.com
---

在这里编写测试步骤和预期结果。
```

| 字段 | 用途 |
|---|---|
| `id` | 需求标识。需要被其他需求依赖时填写，同一批测试中不能重复。 |
| `severity` | `blocker` 表示重点验收项，`suggest` 表示建议项；默认为 `blocker`。 |
| `target` | 本条需求的测试地址。不填写时使用 AI 测试节点的目标地址。 |
| `needs` | 本条需求依赖的需求 `id` 列表，仅用于控制执行顺序。 |

`target` 支持变量，例如：

```yaml
target: ${BASE_URL}
```

如果需求既没有自己的 `target`，节点也没有配置默认目标地址，该需求会执行失败。

### 1.4 安排多条需求的顺序

优先把“创建 → 验证 → 删除 → 再次验证”等关联步骤写在同一个需求文件中，保证测试数据由该需求自己管理。

确实需要拆分时，使用 `needs` 指定前置需求：

```markdown
---
id: delete-resource
needs: [create-resource]
---
删除由 create-resource 创建的资源，并确认列表中不再显示该资源。
```

这里填写的是需求 `id`，不是流水线节点名称。前置需求失败时，依赖它的需求会标记为“跳过”。

### 1.5 按目录组织冒烟测试和全量测试

可以用目录区分不同测试范围：

```text
requirements/
├── smoke/
│   └── login.md
└── full/
    ├── checkout.md
    └── coupon.md
```

之后通过节点的“需求过滤”选择本次要执行的文件。例如：

```text
.pipewright/tests/requirements/smoke/**/*.md
```

### 1.6 编写测试需求时的注意事项

- 使用专用、低权限的测试账号和测试数据，不要在需求文件中写入生产密钥。
- 涉及下单、创建资源等操作时，明确允许操作的范围，并写清清理步骤。
- 预期结果应以页面实际内容或接口实际响应为准，避免使用“表现良好”等无法判断的描述。
- 需求文件应与对应功能代码一起提交和评审，确保测试内容与功能版本一致。

## 2. 配置 AI 测试节点

在部署节点所在阶段添加 **AI 测试**节点，并让它依赖部署节点，保证先部署、后测试。

```yaml
jobs:
  - id: job_deploy
    name: 部署测试机
    type: deploy_ssh
    config:
      artifactJob: 构建
      selector: env=test

  - id: job_ai
    name: AI 需求测试
    type: ai_test
    needs: [job_deploy]
    config:
      deployJob: 部署测试机
      target: https://staging.example.com
      filter: .pipewright/tests/requirements/**/*.md
      onAiUnavailable: fail
      timeoutSeconds: "600"
      maxSteps: "25"
```

AI 测试相关配置如下：

| 配置 | 说明 |
|---|---|
| `target` | 默认测试地址；需求文件中的 `target` 可以覆盖它。 |
| `filter` | 要执行的需求文件，默认是 `.pipewright/tests/requirements/**/*.md`。 |
| `deployJob` | 可选。填写同阶段部署节点的 `name`，需要使用部署机地址或经部署机访问被测系统时配置。 |
| `onAiUnavailable` | AI 不可用时的处理方式：`fail` 表示节点失败，`skip` 表示记为“未测试”并继续。默认为 `fail`。 |
| `timeoutSeconds` | 整体超时时间，默认 `300` 秒，最长 `3600` 秒。 |
| `maxSteps` | 单条需求允许执行的最大步数，默认 `25`，最大 `100`。 |

注意两种引用的区别：

- `needs: [job_deploy]` 填写部署节点的 **`id`**，用于控制执行顺序。
- `deployJob: 部署测试机` 填写部署节点的 **`name`**。

## 3. 查看测试结果

运行结束后，在测试报告中查看每条需求的裁决、原因和证据；在节点日志中查看具体操作过程。浏览器测试还会保留截图。

结果按以下方式理解：

- **通过**：AI 观察到的页面或接口结果符合需求。
- **失败**：结果不符合需求，或者因超时、超出步数等原因未能完成验证。
- **跳过**：前置需求失败，当前需求未执行。
- **未测试**：AI 不可用，且节点配置了 `onAiUnavailable: skip`。

> AI 测试裁决仅供参考，单条需求失败不会直接改变节点成败。用于发布验收时，请在后续发布阶段设置人工审批，根据测试报告决定是否放行。

如果没有发现任何需求、需求文件格式错误或需求依赖关系无效，AI 测试节点会失败。此时应先修正需求文件或过滤条件，再重新运行。
