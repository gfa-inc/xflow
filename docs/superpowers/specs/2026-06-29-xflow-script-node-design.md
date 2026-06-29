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

> js 与 wasm 是**根本不同的执行模型**：js 靠注入全局变量 + 取完成值返回；wasm 是预编译二进制，通过 WASI stdin/stdout 的 JSON ABI 与宿主交换数据。两者共用 `script.Engine` 抽象，但 I/O 机制不同（见 §5）。凭证获取则两族统一走 `getCredential`（见 §6）。

## 3. 包结构与分层

```
nodes/node/script.go                 # ScriptNode（package node）：节点层，组装 DSL 契约
nodes/node/script/                    # package script —— 语言无关层
  ├── engine.go                       # Engine 接口 + (language, runtime) 注册表
  ├── helpers.go                      # Helpers 接口实现：Credential / base64 / aes 纯 Go 核心逻辑（语言无关）
  ├── js/                             # package js —— language=js
  │   ├── js.go                       # JS 公共层：全局注入、完成值提取、helper 绑定约定
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
- helper 的**核心逻辑**（凭证解析、base64、aes 解密）是纯 Go，放语言无关的 `script/helpers.go`。js 路径把它绑定为 JS 全局 `$helpers`（`credential` + `base64` + `aes`，由 `js.go` 在族层面定义、goja/qjs 一致）；wasm 路径用它把声明的凭证序列化进 stdin JSON。

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

各引擎对 `Execute` 入参的解释：

| 入参 | js 引擎 | wasm 引擎 |
|------|---------|-----------|
| `code` | JS 源码文本 | base64 预编译 wasm 模块 |
| `globals` | 注入为 JS 全局变量 | 序列化为 JSON 写入 guest stdin |
| `h Helpers` | 绑定为 JS 全局 `$helpers`（`credential` + `base64` + `aes`，族内 goja/qjs 一致） | 不绑定 host 函数；声明的凭证经 stdin JSON 的 `$credentials` 字段注入 |
| 返回值 | 脚本最后一个表达式的求值结果 | 从 guest stdout 读取的 JSON |

`Helpers` 是语言无关的工具集合接口，核心是 `Credential(name)`（+ base64 / aes 纯 Go 实现）。js 路径绑定为 `$helpers` 全局命名空间；wasm 路径用它把声明的凭证序列化进 stdin JSON。具体见 §6。

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

脚本最后一个表达式的求值结果即输出：

```js
var order = $input.order;
({ status: 'ok', total: order.price * order.qty })   // 该对象即 Output.Data
```

### 5.2 wasm 语言族：WASI + stdin/stdout JSON

- `code` 为 base64 编码的预编译 WASI 模块（Rust / TinyGo / AssemblyScript 等标准工具链产物）。
- 宿主把输入对象（结构同 §5.1 的全局表，封装为单个 JSON 对象；声明的凭证置于专用字段如 `$credentials`）写入 guest **stdin**。
- guest 处理后把输出 JSON 写入 **stdout**，宿主读取并解析为输出对象。
- wazero **不挂载** preopen / FS / clock / random / 环境变量 → 仍是沙箱（详见 §7）。

### 5.3 输出结果映射（两族统一）

- 对象 → 直接作为 `Output.Data`
- 标量 → `{ "result": v }`
- null / undefined / 空 stdout → 空 `{}`

## 6. 凭证获取与工具函数

js 路径把工具集合绑定为单一全局命名空间 **`$helpers`**（与 `$input`/`$vars` 的 `$` 前缀惯例一致，自解释、避免污染顶层）：

```js
$helpers.credential(name)              // 取声明的凭证 → {key, iv, mode, ...}
$helpers.base64.encode(s) / .decode(s)
$helpers.aes.decrypt(ciphertext, key, iv, mode?)   // AES 解密（js 族始终提供）
```

> **族内一致性原则**：`$helpers` 契约定义在**语言族层面**（`js.go`），同族所有 runtime 绑定**完全相同**的 helper 集合。脚本在 goja 上能用的 `$helpers.*`，换到 qjs 必须同样可用——否则同一段 js 脚本换引擎就跑不了。

因此整个 **js 族始终暴露 `$helpers.aes`**：它由纯 Go 的 `helpers.go` 支撑，goja 和 qjs 绑定同一个 Go host 函数。该兜底的存在**不**取决于单个引擎是否带原生 WebCrypto——即便 qjs 自带 WebCrypto，那只是脚本可选的额外原生能力，`$helpers.aes` 依然存在以保证族内一致。`$helpers.credential` / `$helpers.base64` 同理，全族始终提供。

凭证仅限**节点 Descriptor 显式声明**的项（最小暴露面）。`$helpers.credential(name)` 返回 `{key, iv, mode, ...}`（key/iv 为 base64）。

> 跨语言族暴露不同是预期的（执行模型本就不同，见下）；一致性约束只在**族内**。`$helpers` 是 **JS 专属**——host 函数只能绑进 JS runtime。wasm guest 调不了 host 函数，凭证以**数据**形式经 stdin JSON 的 `$credentials` 字段传入，guest 用自带 crypto 解密。两族凭证来源语义统一（都源于声明的凭证），只是 js 是函数调用、wasm 是数据读取。

### 6.1 各语言族绑定

| 语言族 | 凭证获取 | AES | base64 | 族内一致性 |
|--------|----------|-----|--------|------------|
| js（goja + qjs） | `$helpers.credential(name)` host 函数 | `$helpers.aes`（Go 支撑，goja/qjs 相同） | `$helpers.base64`（Go 支撑，goja/qjs 相同） | 两引擎绑定完全相同的 `$helpers` |
| wasm（wazero） | stdin JSON 的 `$credentials` 字段 | guest 自带 crypto，不提供 | guest 自带 | 单引擎 |

- **js**：脚本 `var c = $helpers.credential("db_key"); var plain = $helpers.aes.decrypt(ct, c.key, c.iv)`。goja、qjs 行为一致；qjs 若另有原生 WebCrypto，脚本可选用，但不影响 `$helpers` 契约。
- **wasm**：guest 自包含，宿主不绑定 host 函数；节点层把声明的凭证序列化进 stdin JSON 的 `$credentials`，guest 读取后用自带 crypto 解密。

### 6.2 安全约束

> [SEC-LOGIC] 凭证安全

**基础约束（两族）：**

- 仅注入节点 Descriptor 显式声明的凭证，且按需取用。
- `$helpers.aes` 失败时返回通用错误，**绝不**回显 key / iv / 明文片段。
- js 路径仅注入 `$helpers`（credential / base64 / aes），不挂载任何 IO 能力；wasm 路径仅经 stdin 注入凭证数据，不绑定 host 函数。

**凭证与可观测 input 分离（wasm 平台侧根治）：**

wasm 的 `$credentials` 是**数据**，与业务 input 同处一份 JSON，存在经引擎持久化/日志/trace 泄漏的风险（StateStore 落 Redis/MySQL、`TraceID` 链路记录等）——密钥会明文外泄，违反 security-detail 日志黑名单。因此节点层必须构造两份视图：

- `logicalInput`：业务数据，**唯一**用于 persist / log / trace 的视图，**不含** `$credentials`。
- `wireInput = logicalInput + $credentials`：**仅**用于写 guest stdin（内存 `bytes.Reader`，绝不落临时文件、绝不被宿主日志打印）。`$credentials` 只在写 stdin 的瞬间存在，guest 退出即释放。

> js 路径无此 vector：`$helpers.credential(name)` 是函数调用，密钥只在脚本主动调用时进 JS 堆，JS 堆不进 StateStore / 日志。

**输出 scrub（两族对称）：**

脚本/guest 若把凭证值写进输出（js 把 `$helpers.credential()` 返回值塞进返回对象、wasm 把 `$credentials` echo 进 stdout）即泄漏到 `Output.Data` → 下游 → 落库。这是脚本可信度问题、非平台漏洞，两族同等。缓解：宿主在把结果映射成 `Output.Data` 前，扫描输出中是否出现等于已注入凭证值的内容，命中则剔除/拒绝并记错（不回显密钥本身）。**js 与 wasm 套用同一层 scrub**。文档明确：凭证值不得写入节点输出。

> [!WARNING] [SEC-LOGIC:WARN]
> **密钥进入沙箱内存（两族一致的已知权衡）**：`$helpers.credential` / `$credentials` 把密钥本身交给脚本/guest 自行解密，因此密钥会进入沙箱内存——js 与 wasm 姿态统一。沙箱禁 IO / 网络 + 每次执行隔离实例（§7.2，无跨执行残留），密钥无法外泄，故非漏洞；但弱于“宿主代解密、密钥不出宿主”的模式。缓解：仅注入显式声明的凭证 + 输出 scrub。若需“密钥绝不进脚本”的强隔离，应在宿主侧预解密后以普通数据传入，不要用 credential 工具。

## 7. 沙箱、池化、超时

### 7.1 沙箱（默认隔离）

- **js**：goja 与 QuickJS 默认均为纯 ECMAScript，无 `fs`/`net`/`require`/`fetch`/`process`/`XMLHttpRequest`。只注入白名单 helper，绝不挂载 IO 能力。测试断言这些标识符为 `undefined`。
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
| 脚本运行时错误（抛异常、超时打断、guest 非零退出） | 路由到 `error` 端口：`Output{Data:{"error":...}, Port:"error"}` |
| 配置错误（缺 code、language 非支持值、未知 runtime、qjs 未编译、wasm 模块无法编译） | 返回 Go error，走引擎 ErrorPolicy |

## 8. 节点 API 与 Descriptor

```go
node.Script(`({result: $input.x * 2})`)        // 默认 language=js, runtime=goja
node.Script(code).Runtime("qjs")
node.Script(b64wasm).Language("wasm")           // runtime 默认 wazero
node.Script(code).Language("js").Runtime("goja")
```

Descriptor：

- `Type` = `xflow.script`
- Params：`language`（默认 `js`）/ `runtime`（默认 `goja`）/ `code`（必填，string）
- Inputs：`main`
- Outputs：`main` + `error`

`RawParams()` 仅输出非默认字段（与 http/function 节点惯例一致）。`init()` 自注册到全局节点 registry。凭证声明走 Descriptor.Credentials；两族据此决定 `$helpers.credential` / `$credentials` 可取哪些凭证。

## 9. 测试策略

引擎层（`script/js`、`script/wasm`）：

- **js**：完成值提取（对象/标量/null）、全局访问、`$helpers.base64` 往返、`$helpers.credential` 取回声明凭证、`$helpers.aes` 解密（解密正确、错误不回显密钥）、超时打断死循环、沙箱断言（`require`/`fetch`/`process`/`XMLHttpRequest` 为 undefined）、池复用隔离（`var` 泄漏不跨执行）、**goja 与 qjs 跑同一组用例并断言 `$helpers` 暴露完全一致**
- **wasm**：用一个最小测试模块（读 stdin JSON、写 stdout JSON）验证 I/O 往返、输出映射、超时由 ctx 中断、沙箱断言（无 FS/preopen 时文件操作失败）、模块编译缓存命中、声明的凭证经 stdin JSON 注入可被 guest 读取并解密

节点层（`nodes/node`）：

- Descriptor 正确性、`RawParams()` 往返
- 成功 → main 端口、运行时错误 → error 端口、缺 code → Go error
- runtime 选择：goja / qjs / wazero 分别可执行
- 凭证流向：两族凭证均源于声明项；js 经 `$helpers.credential` host 函数、wasm 经 stdin JSON 的 `$credentials` 字段；未声明的凭证不可获取
- **凭证/logical-input 分离（wasm）**：持久化/日志/trace 用的 logicalInput 不含 `$credentials`；仅 wireInput 含，且只用于写 stdin
- **输出 scrub（两族）**：脚本把凭证值写入输出时，宿主在映射成 `Output.Data` 前剔除/拒绝并记错，密钥不进下游

## 10. 已知限制

- **js 无内存硬限制**（goja 限制），仅脚本体积上限 + 超时打断。
- **qjs 若依赖 cgo**，则需 `qjs` build tag 显式启用，默认构建不含。
- **密钥进入沙箱内存**（§6.2 权衡）；需“密钥绝不进脚本”的强隔离场景应在宿主侧预解密后以普通数据传入。
- wasm 仅支持标准 WASI 模块；非 WASI 的自定义 ABI 模块不在本次范围。
