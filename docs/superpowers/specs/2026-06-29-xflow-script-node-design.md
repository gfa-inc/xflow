# xflow.script 节点设计

- 日期：2026-06-29
- 状态：已定稿（待实现计划）
- 优先级：高

## 1. 目标

实现 `xflow.script` 节点，支持在工作流中执行动态脚本。首期落地两个语言族：

- **js**：JavaScript 源码，两个 runtime（goja + qjs）
- **wasm**：预编译 WebAssembly 模块，runtime 为 wazero

脚本由工作流提交方注入（js 为源码字符串，wasm 为 base64 二进制），运行在沙箱中（禁止 IO / 网络 / 文件系统访问）。

## 2. 概念模型

两层选择：

- **language**：执行语言族（`js` | `wasm`）
- **runtime**：在某语言族内选择具体引擎（`js` → `goja` | `qjs`；`wasm` → `wazero`）

默认 `language=js`，`runtime=goja`。`language=wasm` 时 runtime 默认且当前仅 `wazero`。

> js 与 wasm 是**根本不同的执行模型**：js 靠注入全局变量 + 取完成值返回；wasm 是预编译二进制，通过 WASI stdin/stdout 的 JSON ABI 与宿主交换数据。两者共用 `script.Engine` 抽象，但 I/O 机制不同（见 §5）。**凭证则两族完全一致**：节点按名预声明，宿主把凭证值注入 `$credentials`（见 §6）。

## 3. 包结构与分层

```
nodes/node/script.go                 # ScriptNode（package node）：节点层，组装 DSL 契约 + 凭证解析
nodes/node/script/                    # package script —— 语言无关层
  ├── engine.go                       # Engine 接口 + (language, runtime) 注册表
  ├── credentials.go                  # 按名解析凭证值 → 产出 $credentials（语言无关，纯 Go）
  ├── helpers.go                      # base64 等非安全工具的纯 Go 实现（语言无关）
  ├── js/                             # package js —— language=js
  │   ├── js.go                       # JS 公共层：全局注入（含 $credentials）、完成值提取、$helpers 绑定
  │   ├── goja.go                     # runtime=goja：sync.Pool + 程序缓存
  │   ├── qjs.go                      # runtime=qjs：github.com/fastschema/qjs
  │   └── *_test.go
  └── wasm/                           # package wasm —— language=wasm
      ├── wasm.go                     # wasm 公共层：模块缓存、WASI 配置、stdin/stdout JSON 编解码
      ├── wazero.go                   # runtime=wazero：github.com/tetratelabs/wazero
      └── *_test.go
```

分层理由：

- `nodes/node/database.go`、`grpc.go` 已使 `nodes/node` 携带重依赖（mysql/grpc），故 goja/qjs/wazero 放此层合规。`engine/` 完全不受影响，依赖约束不破坏。
- 把引擎实现隔离进子包 `script/js/`、`script/wasm/`，节点文件 `script.go` 只依赖 `script.Engine` 接口，依赖边界清晰。
- **凭证解析在 `credentials.go` 完成（语言无关、纯 Go）**：按声明的 name 解析凭证值写入 `$credentials`；引擎拿到的 `globals` 已含 `$credentials`，对 js/wasm 完全对称。

## 4. Engine 接口与注册表

```go
// package script
type Engine interface {
    Name() string  // 形如 "js/goja"、"js/qjs"、"wasm/wazero"
    Execute(ctx context.Context, code string, globals map[string]any, h Helpers) (any, error)
}

// 注册表 key = (language, runtime) 二元组
func Register(language, runtime string, factory func() Engine)
func Lookup(language, runtime string) (Engine, bool)
```

各引擎 `init()` 自注册：`script.Register("js","goja",...)` / `("js","qjs",...)` / `("wasm","wazero",...)`。节点按 `(language, runtime)` 查找，缺省 `("js","goja")`。

`globals` 已由节点层完成凭证解析——含 `$credentials`（声明 name → 凭证值的映射）。各引擎对 `Execute` 入参的解释：

| 入参 | js 引擎 | wasm 引擎 |
|------|---------|-----------|
| `code` | JS 源码文本 | base64 预编译 wasm 模块 |
| `globals`（含 `$credentials`） | 注入为 JS 全局变量 | 序列化为 JSON 写入 guest stdin |
| `h Helpers` | 绑定为 JS 全局 `$helpers`（仅 `base64` 等非安全工具，族内 goja/qjs 一致） | 不绑定 host 函数；guest 自带工具 |
| 返回值 | 脚本最后一个表达式的求值结果 | 从 guest stdout 读取的 JSON |

`Helpers` 是语言无关的**非安全**工具集合（base64 等）。它不含任何凭证能力——凭证一律走按名预声明注入（§6）。js 路径绑定为 `$helpers` 全局；wasm guest 自带等价工具。

### qjs / cgo 隔离

`github.com/fastschema/qjs` 预计基于 wazero/WASM（纯 Go、零 cgo），与 wasm 语言族同源。实现计划阶段先核实其绑定模型：

- 若为纯 Go：直接编译进默认构建。
- 若依赖 cgo：`qjs.go` 加 build tag（如 `//go:build qjs`）隔离，避免污染默认 `make build`，节点层在 qjs 未编译时返回明确的“runtime 不可用”配置错误。

## 5. 脚本 I/O 契约

### 5.1 js 语言族：注入全局变量 + 完成值返回

注入的全局变量（与 expr 节点语义对齐，但**不**平铺 `Data` 的 key 到顶层，避免与 helper 命名 / JS 保留字冲突）：

| 全局 | 来源 |
|------|------|
| `$input` | `input.Data`（main 端口上游数据） |
| `$inputs` | `input.Inputs`（多端口） |
| `$vars` | `input.Vars`（工作流变量） |
| `$config` | `input.Config`（工作流配置） |
| `$params` | `input.Params`（节点参数） |
| `$runtime` | 每次执行的 runtime 上下文（含 `vars`） |
| `$credentials` | 声明 name → 凭证值的映射（见 §6），无 `credentials` 声明时为空对象 |
| `$credential` | 第一个声明的凭证值（`$credentials` 中首项的便捷别名），无声明时为 `null` |

脚本最后一个表达式的求值结果即输出：

```js
var key = $credential.key;                  // 单凭证场景：直接读首个凭证，免 name 索引
var token = $credentials.api_token.token;   // 多凭证场景：按 name 区分
({ status: 'ok', len: token.length })        // 该对象即 Output.Data
```

### 5.2 wasm 语言族：WASI + stdin/stdout JSON

- `code` 为 base64 编码的预编译 WASI 模块（Rust / TinyGo / AssemblyScript 等标准工具链产物）。
- 宿主把输入对象（结构同 §5.1 的全局表，**含 `$credentials` 与 `$credential`**，封装为单个 JSON 对象）写入 guest **stdin**。
- guest 处理后把输出 JSON 写入 **stdout**，宿主读取并解析为输出对象。
- wazero **不挂载** preopen / FS / clock / random / 环境变量 → 仍是沙箱（详见 §7）。

### 5.3 输出结果映射（两族统一）

- 对象 → 直接作为 `Output.Data`
- 标量 → `{ "result": v }`
- null / undefined / 空 stdout → 空 `{}`

## 6. 凭证：按名预声明注入（两族完全一致）

**核心：节点预声明需要哪些凭证名，宿主在脚本运行前把对应凭证值解析并注入 `$credentials`。** 复用 xflow 现有 `Input.Credential(name)` + resolver 与凭证存储（与 HTTP 节点的 `applyHTTPAuth` 同源），脚本直接读 `$credentials[name]`，不做任何解密。

### 6.1 凭证预声明

节点实例通过 **`credentials` 参数**（字符串数组）声明本脚本可用的凭证名：

```go
node.Script(code).Credentials("aes_key", "api_token")
```

执行顺序：脚本运行**之前**，节点层对每个声明的 name 调 `Input.Credential(name)` 取凭证值，写入 `$credentials[name]`。`$credentials.aes_key` 即该凭证的值（如 `{key, iv, mode}` 或 `{token}`，结构由凭证类型决定）。

**单数便捷别名 `$credential`**：指向 `credentials` 数组**声明顺序的首项**凭证值，等价于 `$credentials[第一个声明的 name]`。单凭证场景（最常见）可直接 `$credential.key`，免去 name 索引，理解成本对齐 n8n 的扁平 `$credentials.field`；多凭证场景仍用 `$credentials[name]` 区分。无声明时 `$credential` 为 `null`。两个全局始终同时提供，js / wasm 一致。

### 6.2 凭证类型机制

**复用 xflow 现有凭证 resolver 与存储**，不为 script 新建凭证类型 schema——与 HTTP 节点完全一致。`credentials` 参数列出的 name **即**授权白名单：仅这些 name 被解析注入，列表外的凭证不可见。

### 6.3 两族一致性

| 语言族 | 凭证值获取 | 模型 |
|--------|-----------|------|
| js（goja + qjs） | 读全局 `$credentials` | 按名解析 → 注入全局 |
| wasm（wazero） | 读 stdin JSON 的 `$credentials` | 按名解析 → 写 stdin |

两族**同一个模型、同一份数据语义、同一个安全姿态**：凭证值已在输入里，脚本直接读 `$credentials`。js 族内 goja/qjs 也因此天然一致（不依赖任何引擎特有的 crypto）。

### 6.4 安全约束

> [SEC-LOGIC] 凭证安全

- 仅 `credentials` 参数显式列出的 name 被解析注入；无声明则 `$credentials` 为空对象。这是最小暴露面的可执行闸门。
- `$credentials` 含解析后的凭证值（敏感数据），随 `globals` / stdin 进入沙箱——因为脚本本就要用它。沙箱禁 IO / 网络 + 每次执行隔离实例（§7.2，无跨执行残留），凭证值无法外泄。
- 凭证值不应被写入节点输出（会流向下游/落库）。文档明确警告；脚本可信度由提交方负责。
- 凭证解析失败（name 不存在、resolver 报错）返回通用错误（配置错误走 Go error，见 §7.4），**绝不**回显凭证值片段。

> [!WARNING] [SEC-LOGIC:WARN]
> **凭证值进入沙箱内存（已知权衡）**：name-only 模型把凭证值本身注入 `$credentials` 供脚本使用，弱于"宿主代用、凭证不出宿主"的 HTTP 节点模式。缓解：仅注入显式声明的 name + 沙箱禁 IO + 隔离实例。若某用例只需"用凭证认证外部调用"而非"脚本读凭证值"，应优先用 HTTP 等专用节点（凭证不进脚本），而非 script 节点。

### 6.5 已知限制

凭证值（含密钥）会进入脚本可读的 `$credentials`。需要"凭证绝不被脚本读取"的强隔离场景，应使用专用节点（HTTP 等）由宿主侧用凭证，不要用 script 节点暴露凭证值。


## 7. 沙箱、池化、超时

### 7.1 沙箱（默认隔离）

- **js**：goja 与 QuickJS 默认均为纯 ECMAScript，无 `fs`/`net`/`require`/`fetch`/`process`/`XMLHttpRequest`。只注入白名单 helper（`$helpers.base64` 等非安全工具），绝不挂载 IO 能力。测试断言这些标识符为 `undefined`。
- **wasm**：wazero 默认无任何能力；配置 WASI 仅开放 stdin/stdout，**不** preopen 任何目录、不挂载 FS / clock / random / env。guest 无法触达宿主文件系统或网络。

### 7.2 实例复用

- **js / goja**：goja runtime 非 goroutine-safe，用 `sync.Pool` 池化裸 runtime。每次执行：取出 → 注入 helper + 输入全局 → 编译并运行 → **清理用户引入的顶层全局**（以创建时 baseline key 集做 diff，防池复用泄漏）→ 归还。编译产物缓存 `code → *goja.Program`。遇 interrupt / panic 的 runtime 丢弃不归还。
- **js / qjs**、**wasm / wazero**：缓存编译后的模块（wazero `CompiledModule` / qjs 编译产物），每次执行实例化新的隔离 instance，天然无跨执行状态泄漏。

### 7.3 超时

- `input.Timeout > 0` 时派生 `context.WithTimeout`。
- **js**：watcher goroutine 在 `ctx.Done()` 调 `vm.Interrupt("timeout")` 打断死循环；QuickJS 用其 interrupt handler 对等实现。
- **wasm**：wazero 原生支持 `context` 取消，超时由 ctx 驱动中断模块执行。

> [!WARNING] [SEC-LOGIC:WARN]
> goja 无内置内存上限，超时仅能打断 CPU 死循环、无法拦截内存炸弹（如 `[].length = 1e9`）。本设计加入**脚本/模块体积上限**作为基础防护；wazero 可配置内存上限（max pages），wasm 路径据此设硬上限。js 路径内存硬限制作为**已知限制**记录。

### 7.4 错误分类

| 错误类型 | 处理 |
|----------|------|
| 脚本运行时错误（抛异常、超时打断、guest 非零退出） | 路到 `error` 端口：`Output{Data:{"error":...}, Port:"error"}` |
| 配置错误（缺 code、language 非支持值、未知 runtime、qjs 未编译、wasm 模块无法编译、声明的凭证 name 解析失败/不存在） | 返回 Go error，走引擎 ErrorPolicy |

## 8. 节点 API 与 Descriptor

```go
node.Script(`({result: $input.x * 2})`)        // 默认 language=js, runtime=goja
node.Script(code).Runtime("qjs")
node.Script(b64wasm).Language("wasm")           // runtime 默认 wazero
node.Script(code).Credentials("aes_key", "api_token")   // 按名预声明凭证
```

Descriptor：

- `Type` = `xflow.script`
- Params：`language`（默认 `js`）/ `runtime`（默认 `goja`）/ `code`（必填，string）/ `credentials`（string 数组，默认空，声明本实例可用的凭证 name）
- Inputs：`main`
- Outputs：`main` + `error`

`RawParams()` 仅输出非默认字段（与 http/function 节点惯例一致）。`init()` 自注册到全局节点 registry。凭证按 `credentials` 声明的 name 在脚本运行前解析为 `$credentials`（见 §6）。

## 9. 测试策略

引擎层（`script/js`、`script/wasm`）：

- **js**：完成值提取（对象/标量/null）、全局访问（含 `$credentials` 与 `$credential`）、`$helpers.base64` 往返、超时打断死循环、沙箱断言（`require`/`fetch`/`process`/`XMLHttpRequest` 为 undefined）、池复用隔离（`var` 泄漏不跨执行）、**goja 与 qjs 跑同一组用例并断言 `$helpers` 暴露完全一致**
- **wasm**：用一个最小测试模块（读 stdin JSON、写 stdout JSON）验证 I/O 往返、输出映射、`$credentials` / `$credential` 经 stdin 可被 guest 读取、超时由 ctx 中断、沙箱断言（无 FS/preopen 时文件操作失败）、模块编译缓存命中

凭证解析层（`script/credentials.go`，语言无关）：

- 按声明的 name 调 `Input.Credential(name)` → 写 `$credentials[name]`，凭证值正确
- `$credential` 指向声明顺序首项；无声明时为 `null`
- name 不存在 / resolver 报错 → Go error，且错误不回显凭证值
- 无 `credentials` 声明时 `$credentials` 为空对象
- **闸门不变量：仅声明的 name 出现在 `$credentials`，未声明的凭证不可见**

节点层（`nodes/node`）：

- Descriptor 正确性、`RawParams()` 往返
- 成功 → main 端口、运行时错误 → error 端口、缺 code → Go error
- runtime 选择：goja / qjs / wazero 分别可执行
- 两族一致性：同一 `credentials` 声明在 js 与 wasm 下，脚本读到的 `$credentials` / `$credential` 凭证值一致

## 10. 已知限制

- **js 无内存硬限制**（goja 限制），仅脚本体积上限 + 超时打断。
- **qjs 若依赖 cgo**，则需 `qjs` build tag 显式启用，默认构建不含。
- **凭证值进入沙箱**（§6.5）：name-only 模型把凭证值注入脚本可读的 `$credentials`；需"凭证绝不被脚本读取"的场景应改用 HTTP 等专用节点。
- wasm 仅支持标准 WASI 模块；非 WASI 的自定义 ABI 模块不在本次范围。
