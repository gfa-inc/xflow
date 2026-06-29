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

> js 与 wasm 是**根本不同的执行模型**：js 靠注入全局变量 + 取完成值返回；wasm 是预编译二进制，通过 WASI stdin/stdout 的 JSON ABI 与宿主交换数据。两者共用 `script.Engine` 抽象，但 I/O 机制不同（见 §5）。**凭证则两族完全一致**：密钥永不进脚本，宿主声明式预解密后把明文放进 `$credentials`（见 §6）。

## 3. 包结构与分层

```
nodes/node/script.go                 # ScriptNode（package node）：节点层，组装 DSL 契约 + 预解密
nodes/node/script/                    # package script —— 语言无关层
  ├── engine.go                       # Engine 接口 + (language, runtime) 注册表
  ├── decrypt.go                      # 声明式预解密：读密文字段 → 用凭证解密 → 产出 $credentials（语言无关，纯 Go）
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
- **预解密在 `decrypt.go` 完成（语言无关、纯 Go），密钥只在这一步存在于宿主侧**，产出物只有明文；引擎拿到的 `globals` 已含 `$credentials`，对 js/wasm 完全对称。

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

`globals` 已由节点层完成预解密——含 `$credentials`（明文映射），**不含任何密钥**。各引擎对 `Execute` 入参的解释：

| 入参 | js 引擎 | wasm 引擎 |
|------|---------|-----------|
| `code` | JS 源码文本 | base64 预编译 wasm 模块 |
| `globals`（含 `$credentials`） | 注入为 JS 全局变量 | 序列化为 JSON 写入 guest stdin |
| `h Helpers` | 绑定为 JS 全局 `$helpers`（仅 `base64` 等非安全工具，族内 goja/qjs 一致） | 不绑定 host 函数；guest 自带工具 |
| 返回值 | 脚本最后一个表达式的求值结果 | 从 guest stdout 读取的 JSON |

`Helpers` 是语言无关的**非安全**工具集合（base64 等）。它不含任何凭证/密钥能力——凭证一律走预解密（§6）。js 路径绑定为 `$helpers` 全局；wasm guest 自带等价工具。

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
| `$credentials` | 宿主预解密产出的明文映射（见 §6），无 `decrypt` 指令时为空对象 |

脚本最后一个表达式的求值结果即输出：

```js
var token = $credentials.token;        // 已是明文，密钥从未出现在脚本里
({ status: 'ok', token: token.length })   // 该对象即 Output.Data
```

### 5.2 wasm 语言族：WASI + stdin/stdout JSON

- `code` 为 base64 编码的预编译 WASI 模块（Rust / TinyGo / AssemblyScript 等标准工具链产物）。
- 宿主把输入对象（结构同 §5.1 的全局表，**含 `$credentials`**，封装为单个 JSON 对象）写入 guest **stdin**。
- guest 处理后把输出 JSON 写入 **stdout**，宿主读取并解析为输出对象。
- wazero **不挂载** preopen / FS / clock / random / 环境变量 → 仍是沙箱（详见 §7）。

### 5.3 输出结果映射（两族统一）

- 对象 → 直接作为 `Output.Data`
- 标量 → `{ "result": v }`
- null / undefined / 空 stdout → 空 `{}`

## 6. 凭证：声明式宿主侧预解密（两族完全一致）

**核心原则：密钥永不进入任何脚本/guest。** 这对齐 n8n（通用脚本节点不持有原始密钥，由框架声明式注入）以及 xflow 现有 HTTP 节点（`applyHTTPAuth` 在 Go 侧用凭证、脚本/DSL 从不见密钥）。script 节点是该模式的脚本版。

### 6.1 decrypt 指令

节点实例通过 **`decrypt` 参数**声明哪些输入字段是密文、用哪个凭证解密。每条指令：

```
{ name: "token", source: "$input.encrypted_token", credential: "aes_key" }
```

- `name`：解密结果在 `$credentials` 中的键
- `source`：密文来源路径（从 `$input` / `$params` 等取，base64 密文）
- `credential`：用于解密的凭证名（解析走现有 `Input.Credential(name)`）

执行顺序：脚本运行**之前**，节点层（`decrypt.go`）按每条指令——取密文 → `Input.Credential(credential)` 取 `{key, iv, mode}` → AES 解密 → 写入 `$credentials[name]`。脚本读到的只有明文。

```go
node.Script(code).Decrypt("token", "$input.encrypted_token", "aes_key")
```

### 6.2 凭证类型机制

**复用 xflow 现有 `Input.Credential(name)` + resolver 与凭证存储**，不为 script 新建凭证类型 schema——与 HTTP 节点完全一致。`decrypt` 指令中出现的 `credential` 名**即**授权声明：节点只会用到指令里列出的凭证，无需额外的白名单参数。

### 6.3 两族一致性

| 语言族 | 密钥 | 明文获取 | 模型 |
|--------|------|----------|------|
| js（goja + qjs） | 永不进脚本 | 读全局 `$credentials` | 预解密 → 注入全局 |
| wasm（wazero） | 永不进 guest | 读 stdin JSON 的 `$credentials` | 预解密 → 写 stdin |

两族**同一个模型、同一份数据语义、同一个安全姿态**：明文已在输入里，脚本直接读 `$credentials`。js 族内 goja/qjs 也因此天然一致（不依赖任何引擎特有的 crypto）。

### 6.4 安全约束

> [SEC-LOGIC] 凭证安全

- 密钥**永不**出现在脚本源码、DSL、`globals`、stdin 或脚本/guest 内存中——仅在 `decrypt.go` 解密的瞬间存在于宿主侧。
- `$credentials` 是明文，按业务数据对待：它会进入 `globals` / stdin，故**与普通业务数据同级**，可被脚本读取、可能被脚本写入输出（这是预期——明文本就是脚本要用的数据）。
- 解密失败返回通用错误（配置错误走 Go error，见 §7.4），**绝不**回显 key / iv / 密文/明文片段。
- 仅 `decrypt` 指令显式列出的凭证会被使用；无指令则 `$credentials` 为空对象。

> 本模型消除了"密钥进沙箱内存"的整条权衡：因为密钥从不进沙箱，wasm 的 stdin 注入也只含明文，与业务 input 同级——无需 input/credential 分离、无需输出 scrub。

### 6.5 已知限制：仅支持入站字段预解密

只能解密**配置期已知的输入字段**（`source` 指向的密文），**不能**解密脚本运行中才产生的密文（如先 HTTP 拉回密文再解密）。这与 n8n 声明式注入同款限制，覆盖绝大多数"解密入站数据"用例。运行时解密属罕见高级场景，v1 不做；将来按需加宿主 host 函数（js 回调）或扩展 ABI（wasm）。

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
| 配置错误（缺 code、language 非支持值、未知 runtime、qjs 未编译、wasm 模块无法编译、decrypt 指令解密失败/凭证缺失） | 返回 Go error，走引擎 ErrorPolicy |

## 8. 节点 API 与 Descriptor

```go
node.Script(`({result: $input.x * 2})`)        // 默认 language=js, runtime=goja
node.Script(code).Runtime("qjs")
node.Script(b64wasm).Language("wasm")           // runtime 默认 wazero
node.Script(code).Decrypt("token", "$input.enc_token", "aes_key")   // 预解密入站密文
```

Descriptor：

- `Type` = `xflow.script`
- Params：`language`（默认 `js`）/ `runtime`（默认 `goja`）/ `code`（必填，string）/ `decrypt`（指令数组，默认空：每项 `{name, source, credential}`）
- Inputs：`main`
- Outputs：`main` + `error`

`RawParams()` 仅输出非默认字段（与 http/function 节点惯例一致）。`init()` 自注册到全局节点 registry。凭证经 `decrypt` 指令在脚本运行前预解密为 `$credentials`（见 §6），密钥不进脚本。

## 9. 测试策略

引擎层（`script/js`、`script/wasm`）：

- **js**：完成值提取（对象/标量/null）、全局访问（含 `$credentials`）、`$helpers.base64` 往返、超时打断死循环、沙箱断言（`require`/`fetch`/`process`/`XMLHttpRequest` 为 undefined）、池复用隔离（`var` 泄漏不跨执行）、**goja 与 qjs 跑同一组用例并断言 `$helpers` 暴露完全一致**
- **wasm**：用一个最小测试模块（读 stdin JSON、写 stdout JSON）验证 I/O 往返、输出映射、`$credentials` 经 stdin 可被 guest 读取、超时由 ctx 中断、沙箱断言（无 FS/preopen 时文件操作失败）、模块编译缓存命中

预解密层（`script/decrypt.go`，语言无关）：

- 按指令取密文 → 解密 → 写 `$credentials[name]`，明文正确
- 凭证缺失 / 密文格式错误 / 解密失败 → Go error，且错误不回显 key/iv/密文/明文
- 无 decrypt 指令时 `$credentials` 为空对象
- **关键不变量：`globals` / stdin 中不含任何密钥**（断言序列化后的输入不出现凭证密钥值）

节点层（`nodes/node`）：

- Descriptor 正确性、`RawParams()` 往返
- 成功 → main 端口、运行时错误 → error 端口、缺 code → Go error
- runtime 选择：goja / qjs / wazero 分别可执行
- 两族一致性：同一 `decrypt` 指令在 js 与 wasm 下，脚本读到的 `$credentials` 明文一致

## 10. 已知限制

- **js 无内存硬限制**（goja 限制），仅脚本体积上限 + 超时打断。
- **qjs 若依赖 cgo**，则需 `qjs` build tag 显式启用，默认构建不含。
- **仅支持入站字段预解密**（§6.5）：不能解密脚本运行中才产生的密文；运行时解密 v1 不做。
- wasm 仅支持标准 WASI 模块；非 WASI 的自定义 ABI 模块不在本次范围。
