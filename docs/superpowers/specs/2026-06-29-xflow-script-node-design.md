# xflow.script 节点设计

- 日期：2026-06-29
- 状态：已定稿（待实现计划）
- 优先级：高

## 1. 目标

实现 `xflow.script` 节点，支持在工作流中执行动态脚本。首期落地 JavaScript（goja + qjs 两个 runtime），并预留 wasm 语言族的扩展位。脚本由工作流提交方以内联字符串注入，运行在沙箱中（禁止 IO / 网络 / 文件系统访问）。

## 2. 概念模型

两层选择：

- **language**：执行语言族（首期 `js`，未来 `wasm`）
- **runtime**：在某语言族内选择具体引擎（`js` → `goja` | `qjs`；`wasm` → `wazero`）

默认 `language=js`，`runtime=goja`。

## 3. 包结构与分层

```
nodes/node/script.go                 # ScriptNode（package node）：节点层，组装 DSL 契约
nodes/node/script/                    # package script —— 语言无关层
  ├── engine.go                       # Engine 接口 + (language, runtime) 注册表
  ├── helpers.go                      # base64 / aesDecrypt 纯 Go 核心逻辑（语言无关）
  ├── js/                             # package js —— language=js
  │   ├── js.go                       # JS 公共层：全局注入、完成值提取、helper 绑定约定
  │   ├── goja.go                     # runtime=goja：sync.Pool + 程序缓存
  │   ├── qjs.go                      # runtime=qjs：github.com/fastschema/qjs
  │   └── *_test.go
  └── wasm/                           # language=wasm（未来，github.com/wazero/wazero）
```

分层理由：

- `nodes/node/database.go`、`grpc.go` 已使 `nodes/node` 携带重依赖（mysql/grpc），故 goja/qjs 放此层合规。`engine/` 完全不受影响，依赖约束不破坏。
- 把引擎实现隔离进子包 `script/js/`，节点文件 `script.go` 只依赖 `script.Engine` 接口，依赖边界清晰。
- helper 的**核心逻辑**（aesDecrypt 凭证解密、base64 往返）是纯 Go，放语言无关的 `script/helpers.go`；**如何把它绑进具体 runtime** 各引擎不同，由 `js.go` 定约定、`goja.go`/`qjs.go` 各自实现。未来 wasm 复用同一份解密核心。

## 4. Engine 接口与注册表

```go
// package script
type Engine interface {
    Name() string  // 形如 "js/goja"、"js/qjs"
    Execute(ctx context.Context, code string, globals map[string]any, h Helpers) (any, error)
}

// 注册表 key = (language, runtime) 二元组
func Register(language, runtime string, factory func() Engine)
func Lookup(language, runtime string) (Engine, bool)
```

`js/goja.go`、`js/qjs.go` 各自 `init()` 调 `script.Register("js", "goja", ...)` / `script.Register("js", "qjs", ...)`。节点按 `(language, runtime)` 查找，缺省 `("js", "goja")`。

`Helpers` 是语言无关的工具集合接口（base64 / aesDecrypt 的纯 Go 实现 + 凭证解析器），由节点层注入，引擎负责把它绑定到对应 runtime 的全局对象。

### qjs / cgo 隔离

`github.com/fastschema/qjs` 预计基于 wazero/WASM（纯 Go、零 cgo），与 wasm 语言族同源。实现计划阶段先核实其绑定模型：

- 若为纯 Go：直接编译进默认构建。
- 若依赖 cgo：`qjs.go` 加 build tag（如 `//go:build qjs`）隔离，避免污染默认 `make build`，节点层在 qjs 未编译时返回明确的“runtime 不可用”配置错误。

## 5. 脚本 I/O 契约

### 输入：注入全局变量（不平铺到顶层）

与 expr 节点语义对齐，但**不**将 `Data` 的 key 平铺到顶层作用域，避免与 helper 命名 / JS 保留字冲突：

| 全局 | 来源 |
|------|------|
| `$input` | `input.Data`（main 端口上游数据） |
| `$inputs` | `input.Inputs`（多端口） |
| `$vars` | `input.Vars`（工作流变量） |
| `$config` | `input.Config`（工作流配置） |
| `$params` | `input.Params`（节点参数） |
| `$runtime` | 每次执行的 runtime 上下文（含 `vars`） |

### 输出：完成值返回

脚本最后一个表达式的求值结果即节点输出：

```js
var order = $input.order;
({ status: 'ok', total: order.price * order.qty })   // 该对象即 Output.Data
```

结果映射沿用 function 节点惯例：

- 对象 → 直接作为 `Output.Data`
- 标量 → `{ "result": v }`
- null / undefined → 空 `{}`

## 6. 内置工具函数

| 函数 | 说明 |
|------|------|
| `base64.encode(str)` / `base64.decode(str)` | 无状态编解码 |
| `crypto.aesDecrypt(ciphertext, credName)` | **密钥来自工作流凭证**：`input.Credential(credName)` 取 `{key, iv, mode}`（key/iv 为 base64，mode 默认 CBC）；`ciphertext` 为 base64，返回明文字符串 |
| `JSON` | JS runtime 原生已有，不额外注入 |

### 安全约束

> [SEC-LOGIC] AI/凭证安全

- 密钥**永不**出现在脚本源码或 DSL 中，仅以凭证名引用。
- aesDecrypt 失败时返回通用错误，**绝不**回显 key / iv / 明文片段。
- helper 白名单注入：除上述函数外，不挂载任何能力。

## 7. 沙箱、池化、超时

### 沙箱（默认隔离）

goja 与 QuickJS 默认均为纯 ECMAScript，无 `fs` / `net` / `require` / `fetch` / `process` / `XMLHttpRequest`。我们只注入白名单 helper，绝不挂载 IO 能力 → 默认即隔离。测试需断言这些标识符为 `undefined`。

### sync.Pool（goja runtime 非 goroutine-safe）

- 池化裸 runtime。每次执行：取出 → 注入 helper + 输入全局 → 编译并运行 → **清理用户引入的顶层全局**（以 runtime 创建时的 baseline key 集做 diff，防止池复用时状态泄漏）→ 归还。
- 编译产物缓存：`code → *goja.Program`，避免重复编译同一脚本。
- 遇 interrupt / panic 的 runtime 丢弃不归还。

### 超时

- `input.Timeout > 0` 时派生 `context.WithTimeout`。
- watcher goroutine 在 `ctx.Done()` 时调 `vm.Interrupt("timeout")` 打断死循环；QuickJS 用其 interrupt handler 对等实现。

> [!WARNING] [SEC-LOGIC:WARN]
> 沙箱已禁 IO，但 goja 无内置内存上限，超时仅能打断 CPU 死循环、无法拦截内存炸弹（如 `[].length = 1e9`）。本设计加入**脚本体积上限**作为基础防护；**内存硬限制**作为已知限制记录，后续迭代评估。

### 错误分类

| 错误类型 | 处理 |
|----------|------|
| 脚本运行时错误（抛异常、超时打断） | 路由到 `error` 端口：`Output{Data:{"error":...}, Port:"error"}` |
| 配置错误（缺 code、language≠支持值、未知 runtime、qjs 未编译） | 返回 Go error，走引擎 ErrorPolicy |

## 8. 节点 API 与 Descriptor

```go
node.Script(`({result: $input.x * 2})`)        // 默认 language=js, runtime=goja
node.Script(code).Runtime("qjs")
node.Script(code).Language("js").Runtime("goja")
```

Descriptor：

- `Type` = `xflow.script`
- Params：`language`（默认 `js`）/ `runtime`（默认 `goja`）/ `code`（必填，string）
- Inputs：`main`
- Outputs：`main` + `error`

`RawParams()` 仅输出非默认字段（与 http/function 节点惯例一致）。`init()` 自注册到全局节点 registry。

## 9. 测试策略

引擎层（`script/js`）：

- 完成值提取：对象 / 标量 / null
- 全局访问：`$input` / `$params` / `$vars` 等可读
- base64 往返
- aesDecrypt：含测试凭证、解密正确、错误不回显密钥
- 超时：死循环被 interrupt 打断
- 沙箱断言：`require` / `fetch` / `process` / `XMLHttpRequest` 为 undefined
- 池复用隔离：脚本 A 的 `var` 泄漏不影响后续脚本 B
- 两个 runtime（goja / qjs）跑同一组用例

节点层（`nodes/node`）：

- Descriptor 正确性
- `RawParams()` 往返
- 成功 → main 端口
- 运行时错误 → error 端口
- 缺 code → Go error
- runtime 选择：goja / qjs 分别可执行

## 10. 已知限制

- 无内存硬限制（goja 限制），仅脚本体积上限 + 超时打断。
- qjs 若依赖 cgo，则需 `qjs` build tag 显式启用，默认构建不含。
- 首期仅 `js` 语言族；`wasm`（wazero）为预留扩展位，本次不实现。
