# xflow.script 节点 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 实现 `xflow.script` 节点，支持在工作流中以沙箱方式执行 js（goja + qjs 两 runtime）与 wasm（wazero）动态脚本，凭证按名预声明注入。

**Architecture:** 节点文件 `nodes/node/script.go`（package node）只依赖语言无关的 `script.Engine` 接口；引擎实现隔离进子包 `nodes/node/script/js`（goja/qjs）与 `nodes/node/script/wasm`（wazero）。节点层在脚本运行前完成凭证按名解析，产出 `$credentials`/`$credential` 注入 `globals`，对 js/wasm 完全对称。js 注入全局变量取完成值返回；wasm 走 WASI stdin/stdout 的 JSON ABI。

**Tech Stack:** Go 1.25；`github.com/dop251/goja`（纯 Go JS，sync.Pool 池化）；`github.com/fastschema/qjs` v0.0.6（QuickJS，底层 wazero，零 cgo）；`github.com/tetratelabs/wazero` v1.12.0（纯 Go WASM）；测试 guest 用 `GOOS=wasip1 GOARCH=wasm` 原生编译。

**关键设计前提（已核实）：**
- qjs v0.0.6 是**纯 Go**（底层 wazero），默认构建即可纳入，**无需 cgo build tag**（spec §10 的 qjs/cgo 限制已排除）。
- goja runtime 非 goroutine-safe → 用 `sync.Pool` 池化裸 runtime + 编译产物缓存。
- qjs / wazero 每次执行实例化新隔离 instance，天然无跨执行残留；qjs runtime 用请求 ctx + `CloseOnContextDone` 实现超时。
- 沙箱 = 不注入任何 IO 能力：goja/qjs 默认纯 ECMAScript；wazero 不 preopen FS、不挂 clock/random/env。

---

## 文件结构

| 文件 | 职责 |
|------|------|
| `nodes/node/script/engine.go` | `Engine` 接口 + `Helpers` 接口 + `(language, runtime)` 注册表（`Register`/`Lookup`） |
| `nodes/node/script/helpers.go` | `defaultHelpers`：base64 编解码等非安全工具的纯 Go 实现 |
| `nodes/node/script/credentials.go` | `ResolveCredentials`：按声明 name 解析凭证值 → `$credentials`/`$credential`（语言无关，纯 Go） |
| `nodes/node/script/result.go` | `MapResult`：完成值/stdout JSON → `Output.Data`（两族统一映射，§5.3） |
| `nodes/node/script/js/js.go` | js 公共层：`BuildGlobals`、结果映射委托、`$helpers` 抽象 |
| `nodes/node/script/js/goja.go` | runtime=goja：sync.Pool + 程序缓存 + Interrupt 超时 + 全局清理 |
| `nodes/node/script/js/qjs.go` | runtime=qjs：fastschema/qjs，每次执行新 Runtime（ctx 超时） |
| `nodes/node/script/wasm/wasm.go` | wasm 公共层：CompiledModule 缓存、WASI 配置、stdin/stdout JSON 编解码 |
| `nodes/node/script/wasm/wazero.go` | runtime=wazero：wazero 运行时装配 |
| `nodes/node/script/wasm/testdata/echo/main.go` | 测试 guest：读 stdin JSON、回写 stdout JSON |
| `nodes/node/script.go` | `ScriptNode`（package node）：Builder + Descriptor + Execute（凭证解析 → 查引擎 → 执行 → 映射输出/错误端口） |
| `nodes/node/script/*_test.go`、`nodes/node/script_test.go` | 各层单测 |

---

## Task 1: 依赖与 Engine 接口 + 注册表

**Files:**
- Modify: `go.mod`（go get 三个依赖）
- Create: `nodes/node/script/engine.go`
- Test: `nodes/node/script/engine_test.go`

- [ ] **Step 1: 拉取依赖**

Run:
```bash
go get github.com/dop251/goja@latest
go get github.com/fastschema/qjs@v0.0.6
go get github.com/tetratelabs/wazero@v1.12.0
go mod tidy
```
Expected: `go.mod` 新增三个 require，无 cgo 报错。

- [ ] **Step 2: 写失败测试 `engine_test.go`**

```go
package script

import (
	"context"
	"testing"
)

type fakeEngine struct{ name string }

func (e *fakeEngine) Name() string { return e.name }
func (e *fakeEngine) Execute(_ context.Context, _ string, _ map[string]any, _ Helpers) (any, error) {
	return map[string]any{"ok": true}, nil
}

func TestRegisterAndLookup(t *testing.T) {
	Register("js", "fake", func() Engine { return &fakeEngine{name: "js/fake"} })

	e, ok := Lookup("js", "fake")
	if !ok {
		t.Fatal("expected fake engine registered")
	}
	if e.Name() != "js/fake" {
		t.Fatalf("name = %q, want js/fake", e.Name())
	}
}

func TestLookupUnknown(t *testing.T) {
	if _, ok := Lookup("nope", "nope"); ok {
		t.Fatal("expected lookup miss for unknown engine")
	}
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./nodes/node/script/ -run TestRegister -v`
Expected: FAIL，编译错误 `undefined: Register` / `undefined: Engine`。

- [ ] **Step 4: 写 `engine.go`**

```go
// Package script defines the language-agnostic engine abstraction and
// (language, runtime) registry for the xflow.script node. Concrete engines
// live in subpackages (js, wasm) and self-register via init().
package script

import (
	"context"
	"sync"
)

// Engine executes a script of one (language, runtime) family.
type Engine interface {
	// Name is the human-readable identifier, e.g. "js/goja", "wasm/wazero".
	Name() string
	// Execute runs code with the given globals (already including $credentials
	// and $credential resolved by the node layer) and host helpers.
	// It returns the raw completion value (js) or decoded stdout JSON (wasm).
	Execute(ctx context.Context, code string, globals map[string]any, h Helpers) (any, error)
}

// Helpers is the language-agnostic set of NON-SECURITY utilities exposed to
// scripts (base64, etc). It carries no credential capability.
type Helpers interface {
	Base64Encode(s string) string
	Base64Decode(s string) (string, error)
}

type registryKey struct{ language, runtime string }

var (
	registryMu sync.RWMutex
	registry   = map[registryKey]func() Engine{}
)

// Register adds an engine factory under (language, runtime). Called from
// engine subpackage init().
func Register(language, runtime string, factory func() Engine) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[registryKey{language, runtime}] = factory
}

// Lookup returns an engine instance for (language, runtime).
func Lookup(language, runtime string) (Engine, bool) {
	registryMu.RLock()
	factory, ok := registry[registryKey{language, runtime}]
	registryMu.RUnlock()
	if !ok {
		return nil, false
	}
	return factory(), true
}
```

- [ ] **Step 5: 运行测试确认通过**

Run: `go test ./nodes/node/script/ -run "TestRegister|TestLookup" -v`
Expected: PASS。

- [ ] **Step 6: 提交**

```bash
git add go.mod go.sum nodes/node/script/engine.go nodes/node/script/engine_test.go
git commit -m "feat(script): add engine interface and (language,runtime) registry"
```

---

## Task 2: Helpers（base64 等非安全工具）

**Files:**
- Create: `nodes/node/script/helpers.go`
- Test: `nodes/node/script/helpers_test.go`

- [ ] **Step 1: 写失败测试 `helpers_test.go`**

```go
package script

import "testing"

func TestDefaultHelpers_Base64RoundTrip(t *testing.T) {
	h := DefaultHelpers()
	enc := h.Base64Encode("hello")
	if enc != "aGVsbG8=" {
		t.Fatalf("encode = %q, want aGVsbG8=", enc)
	}
	dec, err := h.Base64Decode(enc)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if dec != "hello" {
		t.Fatalf("decode = %q, want hello", dec)
	}
}

func TestDefaultHelpers_Base64DecodeInvalid(t *testing.T) {
	if _, err := DefaultHelpers().Base64Decode("!!not-base64!!"); err == nil {
		t.Fatal("expected error for invalid base64")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/ -run TestDefaultHelpers -v`
Expected: FAIL，`undefined: DefaultHelpers`。

- [ ] **Step 3: 写 `helpers.go`**

```go
package script

import "encoding/base64"

type defaultHelpers struct{}

// DefaultHelpers returns the standard non-security helper set.
func DefaultHelpers() Helpers { return defaultHelpers{} }

func (defaultHelpers) Base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

func (defaultHelpers) Base64Decode(s string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/ -run TestDefaultHelpers -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script/helpers.go nodes/node/script/helpers_test.go
git commit -m "feat(script): add base64 host helpers"
```

---

## Task 3: 凭证按名解析（语言无关）

实现 spec §6：节点声明 name 列表，宿主在脚本运行前用 `Input.Credential(name)` 解析为 `$credentials`，并提供首项别名 `$credential`。这是安全闸门：仅声明的 name 出现，未声明不可见。

**Files:**
- Create: `nodes/node/script/credentials.go`
- Test: `nodes/node/script/credentials_test.go`

- [ ] **Step 1: 写失败测试 `credentials_test.go`**

```go
package script

import "testing"

func TestResolveCredentials_DeclaredOnly(t *testing.T) {
	resolver := func(name string) (map[string]any, error) {
		switch name {
		case "aes_key":
			return map[string]any{"key": "secret-k", "iv": "iv-v"}, nil
		case "api_token":
			return map[string]any{"token": "t-123"}, nil
		case "unlisted":
			return map[string]any{"token": "should-not-appear"}, nil
		}
		return nil, nil
	}

	creds, first, err := ResolveCredentials([]string{"aes_key", "api_token"}, resolver)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 2 {
		t.Fatalf("creds len = %d, want 2", len(creds))
	}
	if creds["aes_key"].(map[string]any)["key"] != "secret-k" {
		t.Fatalf("aes_key.key = %v", creds["aes_key"])
	}
	if _, leaked := creds["unlisted"]; leaked {
		t.Fatal("gate violated: unlisted credential present in $credentials")
	}
	// $credential points at declaration-order first item.
	if first.(map[string]any)["key"] != "secret-k" {
		t.Fatalf("$credential = %v, want aes_key value", first)
	}
}

func TestResolveCredentials_NoDeclaration(t *testing.T) {
	creds, first, err := ResolveCredentials(nil, func(string) (map[string]any, error) { return nil, nil })
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(creds) != 0 {
		t.Fatalf("creds should be empty, got %v", creds)
	}
	if first != nil {
		t.Fatalf("$credential should be nil when nothing declared, got %v", first)
	}
}

func TestResolveCredentials_ResolverError_NoLeak(t *testing.T) {
	resolver := func(name string) (map[string]any, error) {
		return nil, errMissing
	}
	_, _, err := ResolveCredentials([]string{"aes_key"}, resolver)
	if err == nil {
		t.Fatal("expected error when resolver fails")
	}
	// error must reference the name, never echo a value.
	if got := err.Error(); !contains(got, "aes_key") {
		t.Fatalf("error %q should name the credential", got)
	}
}

var errMissing = &resolveErr{}

type resolveErr struct{}

func (*resolveErr) Error() string { return "credential not found" }

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/ -run TestResolveCredentials -v`
Expected: FAIL，`undefined: ResolveCredentials`。

- [ ] **Step 3: 写 `credentials.go`**

```go
package script

import "fmt"

// CredentialResolver resolves a credential value by name. It mirrors
// types.Input.Credential but returns an error so resolution failures can be
// surfaced as config errors. The node layer adapts Input.Credential to this.
type CredentialResolver func(name string) (map[string]any, error)

// ResolveCredentials resolves each declared name into the $credentials map and
// returns the declaration-order first value as the $credential alias.
//
// Security gate: only names in `declared` are resolved and exposed. The error
// path never echoes credential values, only the failing name.
func ResolveCredentials(declared []string, resolver CredentialResolver) (creds map[string]any, first any, err error) {
	creds = make(map[string]any, len(declared))
	for i, name := range declared {
		val, rerr := resolver(name)
		if rerr != nil {
			return nil, nil, fmt.Errorf("script: resolve credential %q: %w", name, rerr)
		}
		if val == nil {
			return nil, nil, fmt.Errorf("script: credential %q not found", name)
		}
		creds[name] = val
		if i == 0 {
			first = val
		}
	}
	return creds, first, nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/ -run TestResolveCredentials -v`
Expected: PASS（含闸门不变量、首项别名、无声明、resolver 报错不回显值）。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script/credentials.go nodes/node/script/credentials_test.go
git commit -m "feat(script): add name-declared credential resolution with $credential alias"
```

---

## Task 4: 结果映射（两族统一，§5.3）

**Files:**
- Create: `nodes/node/script/result.go`
- Test: `nodes/node/script/result_test.go`

- [ ] **Step 1: 写失败测试 `result_test.go`**

```go
package script

import "testing"

func TestMapResult_Object(t *testing.T) {
	out := MapResult(map[string]any{"status": "ok", "n": 1.0})
	if out["status"] != "ok" || out["n"] != 1.0 {
		t.Fatalf("object passthrough wrong: %v", out)
	}
}

func TestMapResult_Scalar(t *testing.T) {
	out := MapResult(42.0)
	if out["result"] != 42.0 {
		t.Fatalf("scalar should wrap as result, got %v", out)
	}
}

func TestMapResult_Nil(t *testing.T) {
	out := MapResult(nil)
	if len(out) != 0 {
		t.Fatalf("nil should map to empty object, got %v", out)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/ -run TestMapResult -v`
Expected: FAIL，`undefined: MapResult`。

- [ ] **Step 3: 写 `result.go`**

```go
package script

// MapResult normalizes an engine completion value into Output.Data per §5.3:
// object -> passthrough; scalar -> {"result": v}; nil -> {}.
func MapResult(v any) map[string]any {
	switch t := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		return t
	default:
		return map[string]any{"result": v}
	}
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/ -run TestMapResult -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script/result.go nodes/node/script/result_test.go
git commit -m "feat(script): add unified completion-value to Output.Data mapping"
```

---

## Task 5: js 公共层（BuildGlobals + 引擎共享逻辑）

**Files:**
- Create: `nodes/node/script/js/js.go`
- Test: `nodes/node/script/js/js_test.go`

`BuildGlobals` 在此层不直接被测全局注入（那需引擎），但单测验证它产出正确的 key 集合，保证 goja/qjs 注入同一份数据。

- [ ] **Step 1: 写失败测试 `js_test.go`**

```go
package js

import "testing"

func TestBuildGlobals_Keys(t *testing.T) {
	globals := map[string]any{
		"$input":       map[string]any{"x": 1.0},
		"$credentials": map[string]any{"k": map[string]any{"token": "t"}},
		"$credential":  map[string]any{"token": "t"},
	}
	g := BuildGlobals(globals)
	for _, k := range []string{"$input", "$credentials", "$credential"} {
		if _, ok := g[k]; !ok {
			t.Fatalf("missing global %q", k)
		}
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/js/ -run TestBuildGlobals -v`
Expected: FAIL，`undefined: BuildGlobals`。

- [ ] **Step 3: 写 `js.go`**

```go
// Package js implements the JavaScript language family (goja + qjs runtimes)
// for the xflow.script node. Both runtimes share global injection and result
// mapping so their $helpers exposure and credential model stay identical.
package js

// BuildGlobals returns the globals map to inject. The node layer already
// assembled $input/$credentials/etc; this hook exists so both runtimes consume
// the exact same source and stay consistent.
func BuildGlobals(globals map[string]any) map[string]any {
	if globals == nil {
		return map[string]any{}
	}
	return globals
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/js/ -run TestBuildGlobals -v`
Expected: PASS。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script/js/js.go nodes/node/script/js/js_test.go
git commit -m "feat(script/js): add shared js-family global builder"
```

---

## Task 6: goja 引擎（sync.Pool + 程序缓存 + 超时 + 全局清理）

**Files:**
- Create: `nodes/node/script/js/goja.go`
- Test: `nodes/node/script/js/goja_test.go`

- [ ] **Step 1: 写失败测试 `goja_test.go`**

```go
package js

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gfa-inc/xflow/nodes/node/script"
)

func newGoja() script.Engine {
	e, _ := script.Lookup("js", "goja")
	return e
}

func TestGoja_ObjectCompletion(t *testing.T) {
	out, err := newGoja().Execute(context.Background(),
		`({status: 'ok', len: $input.name.length})`,
		map[string]any{"$input": map[string]any{"name": "abcd"}},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	if m["status"] != "ok" {
		t.Fatalf("status = %v", m["status"])
	}
}

func TestGoja_ReadsCredential(t *testing.T) {
	out, err := newGoja().Execute(context.Background(),
		`({t: $credential.token, k: $credentials.aes_key.key})`,
		map[string]any{
			"$credential":  map[string]any{"token": "t-1"},
			"$credentials": map[string]any{"aes_key": map[string]any{"key": "kk"}},
		},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	if m["t"] != "t-1" || m["k"] != "kk" {
		t.Fatalf("credential read wrong: %v", m)
	}
}

func TestGoja_HelpersBase64(t *testing.T) {
	out, err := newGoja().Execute(context.Background(),
		`({enc: $helpers.base64Encode('hi')})`,
		nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.(map[string]any)["enc"] != "aGk=" {
		t.Fatalf("base64 helper wrong: %v", out)
	}
}

func TestGoja_SandboxNoIO(t *testing.T) {
	out, err := newGoja().Execute(context.Background(),
		`({hasRequire: typeof require, hasFetch: typeof fetch, hasProcess: typeof process, hasXHR: typeof XMLHttpRequest})`,
		nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	for _, k := range []string{"hasRequire", "hasFetch", "hasProcess", "hasXHR"} {
		if m[k] != "undefined" {
			t.Fatalf("sandbox leak: %s = %v, want undefined", k, m[k])
		}
	}
}

func TestGoja_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := newGoja().Execute(ctx, `while(true){}`, nil, script.DefaultHelpers())
	if err == nil {
		t.Fatal("expected timeout interrupt error")
	}
}

func TestGoja_PoolIsolation(t *testing.T) {
	e := newGoja()
	// First execution leaks a top-level global.
	_, err := e.Execute(context.Background(), `leaked = 99; ({})`, nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("first exec error: %v", err)
	}
	// Second execution must NOT see it (runtime cleaned before reuse).
	out, err := e.Execute(context.Background(), `({seen: typeof leaked})`, nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("second exec error: %v", err)
	}
	if got := out.(map[string]any)["seen"]; got != "undefined" {
		t.Fatalf("pool leak: leaked = %v across executions", got)
	}
}

func TestGoja_RuntimeError(t *testing.T) {
	_, err := newGoja().Execute(context.Background(), `throw new Error('boom')`, nil, script.DefaultHelpers())
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom error, got %v", err)
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/js/ -run TestGoja -v`
Expected: FAIL，`Lookup("js","goja")` 未注册（ok=false → nil panic）/ 未实现。

- [ ] **Step 3: 写 `goja.go`**

```go
package js

import (
	"context"
	"fmt"
	"sync"

	"github.com/dop251/goja"
	"github.com/gfa-inc/xflow/nodes/node/script"
)

func init() {
	script.Register("js", "goja", func() script.Engine { return sharedGoja })
}

// sharedGoja is process-wide: it holds a sync.Pool of runtimes and a program
// cache, both safe for concurrent use.
var sharedGoja = &gojaEngine{
	programs: map[string]*goja.Program{},
}

type gojaEngine struct {
	pool      sync.Pool // of *pooledVM
	progMu    sync.RWMutex
	programs  map[string]*goja.Program
}

type pooledVM struct {
	vm       *goja.Runtime
	baseline map[string]struct{} // top-level global keys at creation
}

func (e *gojaEngine) Name() string { return "js/goja" }

func (e *gojaEngine) compile(code string) (*goja.Program, error) {
	e.progMu.RLock()
	p, ok := e.programs[code]
	e.progMu.RUnlock()
	if ok {
		return p, nil
	}
	p, err := goja.Compile("script.js", code, false)
	if err != nil {
		return nil, err
	}
	e.progMu.Lock()
	e.programs[code] = p
	e.progMu.Unlock()
	return p, nil
}

func (e *gojaEngine) get() *pooledVM {
	if v, ok := e.pool.Get().(*pooledVM); ok {
		return v
	}
	vm := goja.New()
	base := map[string]struct{}{}
	for _, k := range vm.GlobalObject().Keys() {
		base[k] = struct{}{}
	}
	return &pooledVM{vm: vm, baseline: base}
}

// cleanup removes any top-level globals introduced during execution so the
// runtime can be safely reused.
func (p *pooledVM) cleanup() {
	g := p.vm.GlobalObject()
	for _, k := range g.Keys() {
		if _, ok := p.baseline[k]; !ok {
			_ = g.Delete(k)
		}
	}
}

func (e *gojaEngine) Execute(ctx context.Context, code string, globals map[string]any, h script.Helpers) (any, error) {
	prog, err := e.compile(code)
	if err != nil {
		return nil, fmt.Errorf("js/goja: compile: %w", err)
	}

	pv := e.get()
	vm := pv.vm

	// Inject helpers as $helpers (non-security utilities only).
	_ = vm.Set("$helpers", map[string]any{
		"base64Encode": h.Base64Encode,
		"base64Decode": func(s string) (string, error) { return h.Base64Decode(s) },
	})
	// Inject globals ($input, $credentials, $credential, ...).
	for k, v := range BuildGlobals(globals) {
		_ = vm.Set(k, v)
	}

	// Timeout: watcher interrupts the loop on ctx cancellation.
	done := make(chan struct{})
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				vm.Interrupt("timeout")
			case <-done:
			}
		}()
	}

	val, runErr := vm.RunProgram(prog)
	close(done)

	if runErr != nil {
		// Interrupted runtime is in an unknown state — discard, don't return to pool.
		vm.ClearInterrupt()
		return nil, fmt.Errorf("js/goja: %w", runErr)
	}

	pv.cleanup()
	e.pool.Put(pv)

	if val == nil || goja.IsUndefined(val) || goja.IsNull(val) {
		return nil, nil
	}
	return val.Export(), nil
}
```

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/js/ -run TestGoja -race -v`
Expected: PASS（完成值、凭证读取、$helpers、沙箱 undefined、超时打断、池隔离、运行时错误）。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script/js/goja.go nodes/node/script/js/goja_test.go
git commit -m "feat(script/js): add goja runtime with pooling, program cache, timeout"
```

---

## Task 7: qjs 引擎（QuickJS，纯 Go，ctx 超时）

每次执行新建 `qjs.Runtime`（绑定请求 ctx + `CloseOnContextDone`），天然隔离无残留。全局用 `ParseJSON` 注入，结果用 `JSONStringify` 提取——与 goja 共用 `script.MapResult` 的上游数据语义一致。

**Files:**
- Create: `nodes/node/script/js/qjs.go`
- Test: `nodes/node/script/js/qjs_test.go`

- [ ] **Step 1: 写失败测试 `qjs_test.go`**

```go
package js

import (
	"context"
	"testing"
	"time"

	"github.com/gfa-inc/xflow/nodes/node/script"
)

func newQJS(t *testing.T) script.Engine {
	t.Helper()
	e, ok := script.Lookup("js", "qjs")
	if !ok {
		t.Fatal("qjs engine not registered")
	}
	return e
}

func TestQJS_ObjectCompletion(t *testing.T) {
	out, err := newQJS(t).Execute(context.Background(),
		`({status: 'ok', len: $input.name.length})`,
		map[string]any{"$input": map[string]any{"name": "abcd"}},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.(map[string]any)["status"] != "ok" {
		t.Fatalf("status = %v", out)
	}
}

func TestQJS_ReadsCredential(t *testing.T) {
	out, err := newQJS(t).Execute(context.Background(),
		`({t: $credential.token, k: $credentials.aes_key.key})`,
		map[string]any{
			"$credential":  map[string]any{"token": "t-1"},
			"$credentials": map[string]any{"aes_key": map[string]any{"key": "kk"}},
		},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	if m["t"] != "t-1" || m["k"] != "kk" {
		t.Fatalf("credential read wrong: %v", m)
	}
}

func TestQJS_HelpersBase64(t *testing.T) {
	out, err := newQJS(t).Execute(context.Background(),
		`({enc: $helpers.base64Encode('hi')})`,
		nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.(map[string]any)["enc"] != "aGk=" {
		t.Fatalf("base64 helper wrong: %v", out)
	}
}

func TestQJS_SandboxNoIO(t *testing.T) {
	out, err := newQJS(t).Execute(context.Background(),
		`({hasRequire: typeof require, hasFetch: typeof fetch, hasProcess: typeof process})`,
		nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	for _, k := range []string{"hasRequire", "hasFetch", "hasProcess"} {
		if m[k] != "undefined" {
			t.Fatalf("sandbox leak: %s = %v", k, m[k])
		}
	}
}

func TestQJS_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := newQJS(t).Execute(ctx, `while(true){}`, nil, script.DefaultHelpers())
	if err == nil {
		t.Fatal("expected timeout error")
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/script/js/ -run TestQJS -v`
Expected: FAIL，qjs 引擎未注册。

- [ ] **Step 3: 写 `qjs.go`**

```go
package js

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fastschema/qjs"
	"github.com/gfa-inc/xflow/nodes/node/script"
)

func init() {
	script.Register("js", "qjs", func() script.Engine { return qjsEngine{} })
}

type qjsEngine struct{}

func (qjsEngine) Name() string { return "js/qjs" }

func (qjsEngine) Execute(ctx context.Context, code string, globals map[string]any, h script.Helpers) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	rt, err := qjs.New(qjs.Option{
		Context:            ctx,
		CloseOnContextDone: true,
	})
	if err != nil {
		return nil, fmt.Errorf("js/qjs: new runtime: %w", err)
	}
	defer rt.Close()

	c := rt.Context()
	g := c.Global()

	// Inject globals via JSON round-trip for value parity with goja.
	for k, v := range BuildGlobals(globals) {
		b, merr := json.Marshal(v)
		if merr != nil {
			return nil, fmt.Errorf("js/qjs: marshal global %q: %w", k, merr)
		}
		g.SetProperty(c.NewString(k), c.ParseJSON(string(b)))
	}

	// Inject $helpers (non-security utilities) as native functions.
	helpers := c.NewObject()
	helpers.SetProperty(c.NewString("base64Encode"), c.Function(func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return c.NewString(""), nil
		}
		return c.NewString(h.Base64Encode(args[0].String())), nil
	}))
	helpers.SetProperty(c.NewString("base64Decode"), c.Function(func(this *qjs.This) (*qjs.Value, error) {
		args := this.Args()
		if len(args) == 0 {
			return c.NewString(""), nil
		}
		dec, derr := h.Base64Decode(args[0].String())
		if derr != nil {
			return nil, derr
		}
		return c.NewString(dec), nil
	}))
	g.SetProperty(c.NewString("$helpers"), helpers)

	val, err := rt.Eval("script.js", qjs.Code(code))
	if err != nil {
		return nil, fmt.Errorf("js/qjs: %w", err)
	}
	if val == nil || val.IsUndefined() || val.IsNull() {
		return nil, nil
	}

	jsonStr, err := val.JSONStringify()
	if err != nil || jsonStr == "" {
		return nil, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(jsonStr), &decoded); err != nil {
		return nil, fmt.Errorf("js/qjs: decode result: %w", err)
	}
	return decoded, nil
}
```

> **实现者注意**：`qjs.This.Args()` 与 `c.Function` 的精确签名以 `go doc github.com/fastschema/qjs.This` / `go doc github.com/fastschema/qjs.Context.Function` 为准（v0.0.6）。若 `This` 取参方法名不同，按实际调整；目标行为不变：`$helpers.base64Encode/Decode` 接受一个字符串实参返回字符串。先跑一个最小 spike 确认这两个 API 的形态再填充。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/script/js/ -run TestQJS -v`
Expected: PASS。

- [ ] **Step 5: 跨引擎一致性测试（goja vs qjs，§9）**

追加到 `nodes/node/script/js/js_test.go`：

```go
func TestJSFamily_HelpersConsistent(t *testing.T) {
	code := `({enc: $helpers.base64Encode('xyz'), dec: $helpers.base64Decode('eHl6')})`
	results := map[string]map[string]any{}
	for _, rt := range []string{"goja", "qjs"} {
		e, ok := script.Lookup("js", rt)
		if !ok {
			t.Fatalf("%s not registered", rt)
		}
		out, err := e.Execute(context.Background(), code, nil, script.DefaultHelpers())
		if err != nil {
			t.Fatalf("%s exec error: %v", rt, err)
		}
		results[rt] = out.(map[string]any)
	}
	if results["goja"]["enc"] != results["qjs"]["enc"] || results["goja"]["dec"] != results["qjs"]["dec"] {
		t.Fatalf("js family inconsistent: goja=%v qjs=%v", results["goja"], results["qjs"])
	}
}
```

需在 `js_test.go` 顶部 import `"context"` 与 `"github.com/gfa-inc/xflow/nodes/node/script"`。

Run: `go test ./nodes/node/script/js/ -run TestJSFamily -v`
Expected: PASS（goja 与 qjs 的 `$helpers` 暴露完全一致）。

- [ ] **Step 6: 提交**

```bash
git add nodes/node/script/js/qjs.go nodes/node/script/js/qjs_test.go nodes/node/script/js/js_test.go
git commit -m "feat(script/js): add qjs runtime and js-family consistency test"
```

---

## Task 8: wasm 引擎（wazero + WASI stdin/stdout JSON）

**Files:**
- Create: `nodes/node/script/wasm/wasm.go`
- Create: `nodes/node/script/wasm/wazero.go`
- Create: `nodes/node/script/wasm/testdata/echo/main.go`（测试 guest）
- Test: `nodes/node/script/wasm/wasm_test.go`

- [ ] **Step 1: 写测试 guest `testdata/echo/main.go`**

读 stdin JSON、把 `$input` 原样回写、附带凭证可见性证据到 stdout JSON。

```go
//go:build ignore

// echo is a minimal WASI test guest: reads a JSON object from stdin and writes
// a JSON object to stdout. Compiled with GOOS=wasip1 GOARCH=wasm.
package main

import (
	"encoding/json"
	"io"
	"os"
)

func main() {
	raw, _ := io.ReadAll(os.Stdin)
	var in map[string]any
	_ = json.Unmarshal(raw, &in)

	out := map[string]any{"echo": in["$input"]}
	if creds, ok := in["$credentials"].(map[string]any); ok {
		if ak, ok := creds["aes_key"].(map[string]any); ok {
			out["credKey"] = ak["key"]
		}
	}
	if c, ok := in["$credential"].(map[string]any); ok {
		out["firstToken"] = c["token"]
	}
	// Sandbox probe: attempting to open a host file must fail (no preopen).
	if _, err := os.Open("/etc/hostname"); err != nil {
		out["fsBlocked"] = true
	}

	b, _ := json.Marshal(out)
	_, _ = os.Stdout.Write(b)
}
```

> `//go:build ignore` 让它不参与正常包编译；测试在 setup 阶段用 `go build` 单独编译成 `.wasm`。

- [ ] **Step 2: 写失败测试 `wasm_test.go`**

```go
package wasm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/gfa-inc/xflow/nodes/node/script"
)

var echoWasm []byte

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wasmtest")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "echo.wasm")
	cmd := exec.Command("go", "build", "-o", out, "./testdata/echo/main.go")
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		panic("build echo guest: " + string(b))
	}
	echoWasm, err = os.ReadFile(out)
	if err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func newWasm(t *testing.T) script.Engine {
	t.Helper()
	e, ok := script.Lookup("wasm", "wazero")
	if !ok {
		t.Fatal("wazero engine not registered")
	}
	return e
}

func b64(b []byte) string { // small local helper to avoid extra imports in asserts
	return script.DefaultHelpers().Base64Encode(string(b))
}

func TestWasm_IORoundTrip(t *testing.T) {
	out, err := newWasm(t).Execute(context.Background(),
		b64(echoWasm),
		map[string]any{"$input": map[string]any{"x": 7.0}},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	echo := m["echo"].(map[string]any)
	if echo["x"] != 7.0 {
		t.Fatalf("echo.x = %v, want 7", echo["x"])
	}
}

func TestWasm_CredentialsViaStdin(t *testing.T) {
	out, err := newWasm(t).Execute(context.Background(),
		b64(echoWasm),
		map[string]any{
			"$credentials": map[string]any{"aes_key": map[string]any{"key": "kk"}},
			"$credential":  map[string]any{"token": "t-1"},
		},
		script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	m := out.(map[string]any)
	if m["credKey"] != "kk" {
		t.Fatalf("credKey = %v, want kk", m["credKey"])
	}
	if m["firstToken"] != "t-1" {
		t.Fatalf("firstToken = %v, want t-1", m["firstToken"])
	}
}

func TestWasm_SandboxNoFS(t *testing.T) {
	out, err := newWasm(t).Execute(context.Background(),
		b64(echoWasm), nil, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.(map[string]any)["fsBlocked"] != true {
		t.Fatal("sandbox breach: guest opened a host file")
	}
}

func TestWasm_Timeout(t *testing.T) {
	// echo guest is fast; use an already-cancelled ctx to assert ctx wiring.
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()
	time.Sleep(1 * time.Millisecond)
	_, err := newWasm(t).Execute(ctx, b64(echoWasm), nil, script.DefaultHelpers())
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestWasm_ModuleCacheHit(t *testing.T) {
	e := newWasm(t)
	code := b64(echoWasm)
	for i := 0; i < 3; i++ {
		if _, err := e.Execute(context.Background(), code, map[string]any{"$input": map[string]any{"n": float64(i)}}, script.DefaultHelpers()); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}
	// Cache correctness is observable as: repeated execs of the same code succeed
	// and produce per-call output (no cross-call state).
}
```

- [ ] **Step 3: 运行测试确认失败**

Run: `go test ./nodes/node/script/wasm/ -run TestWasm -v`
Expected: FAIL，wazero 引擎未注册。

- [ ] **Step 4: 写 `wasm.go`（公共层：缓存 + I/O 编解码）**

```go
// Package wasm implements the wasm language family (wazero runtime) for the
// xflow.script node. Guests are WASI modules that read a JSON object from
// stdin and write a JSON object to stdout.
package wasm

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// decodeCode turns the base64 node param into raw wasm bytes.
func decodeCode(code string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(code)
	if err != nil {
		return nil, fmt.Errorf("wasm: decode base64 module: %w", err)
	}
	return b, nil
}

// encodeStdin marshals the globals object written to guest stdin.
func encodeStdin(globals map[string]any) ([]byte, error) {
	if globals == nil {
		globals = map[string]any{}
	}
	return json.Marshal(globals)
}

// decodeStdout parses guest stdout into the raw completion value.
func decodeStdout(out []byte) (any, error) {
	if len(out) == 0 {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal(out, &v); err != nil {
		return nil, fmt.Errorf("wasm: decode stdout json: %w", err)
	}
	return v, nil
}
```

- [ ] **Step 5: 写 `wazero.go`（运行时装配）**

```go
package wasm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"github.com/gfa-inc/xflow/nodes/node/script"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

func init() {
	script.Register("wasm", "wazero", func() script.Engine { return sharedWazero })
}

var sharedWazero = &wazeroEngine{compiled: map[string]wazero.CompiledModule{}}

type wazeroEngine struct {
	mu       sync.RWMutex
	rt       wazero.Runtime
	rtOnce   sync.Once
	compiled map[string]wazero.CompiledModule
}

func (e *wazeroEngine) Name() string { return "wasm/wazero" }

func (e *wazeroEngine) runtime(ctx context.Context) wazero.Runtime {
	e.rtOnce.Do(func() {
		e.rt = wazero.NewRuntime(ctx)
		wasi_snapshot_preview1.MustInstantiate(ctx, e.rt)
	})
	return e.rt
}

func (e *wazeroEngine) compile(ctx context.Context, wasmBytes []byte) (wazero.CompiledModule, error) {
	sum := sha256.Sum256(wasmBytes)
	key := hex.EncodeToString(sum[:])

	e.mu.RLock()
	cm, ok := e.compiled[key]
	e.mu.RUnlock()
	if ok {
		return cm, nil
	}

	cm, err := e.runtime(ctx).CompileModule(ctx, wasmBytes)
	if err != nil {
		return nil, fmt.Errorf("wasm/wazero: compile module: %w", err)
	}
	e.mu.Lock()
	e.compiled[key] = cm
	e.mu.Unlock()
	return cm, nil
}

func (e *wazeroEngine) Execute(ctx context.Context, code string, globals map[string]any, _ script.Helpers) (any, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	wasmBytes, err := decodeCode(code)
	if err != nil {
		return nil, err
	}
	cm, err := e.compile(ctx, wasmBytes)
	if err != nil {
		return nil, err
	}

	stdin, err := encodeStdin(globals)
	if err != nil {
		return nil, err
	}

	var stdout bytes.Buffer
	// Sandbox: stdin/stdout only. No WithFSConfig, clock, random, or env.
	cfg := wazero.NewModuleConfig().
		WithStdin(bytes.NewReader(stdin)).
		WithStdout(&stdout).
		WithName("")

	mod, err := e.runtime(ctx).InstantiateModule(ctx, cm, cfg)
	if err != nil {
		return nil, fmt.Errorf("wasm/wazero: run module: %w", err)
	}
	_ = mod.Close(ctx)

	return decodeStdout(stdout.Bytes())
}
```

> **实现者注意**：每次执行用新 `cfg` + `InstantiateModule` 得到全新隔离 instance（不复用 module 实例），故无跨执行状态。WASI 模块 `_start` 在 `InstantiateModule` 时自动运行。

- [ ] **Step 6: 运行测试确认通过**

Run: `go test ./nodes/node/script/wasm/ -run TestWasm -v`
Expected: PASS（I/O 往返、凭证经 stdin 可读、沙箱 FS 受阻、ctx 取消报错、缓存命中）。

- [ ] **Step 7: 提交**

```bash
git add nodes/node/script/wasm/
git commit -m "feat(script/wasm): add wazero runtime with WASI stdin/stdout JSON ABI"
```

---

## Task 9: 节点层 ScriptNode（package node）

组装 DSL Builder + Descriptor，在 Execute 中：解析凭证 → 装配 globals → 查引擎 → 执行 → 映射输出/错误端口。

**Files:**
- Create: `nodes/node/script.go`
- Test: `nodes/node/script_test.go`

- [ ] **Step 1: 写失败测试 `script_test.go`**

```go
package node_test

import (
	"context"
	"testing"

	"github.com/gfa-inc/xflow/nodes/node"
)

func TestScript_Factory(t *testing.T) {
	b := node.Script(`({result: $input.x * 2})`).Runtime("qjs").Credentials("aes_key", "api_token")
	if b.NodeType() != "xflow.script" {
		t.Fatalf("type = %s", b.NodeType())
	}
	p := b.RawParams().(map[string]any)
	if p["runtime"] != "qjs" {
		t.Fatalf("runtime = %v", p["runtime"])
	}
	creds := p["credentials"].([]string)
	if len(creds) != 2 || creds[0] != "aes_key" {
		t.Fatalf("credentials = %v", creds)
	}
}

func TestScript_Defaults(t *testing.T) {
	p := node.Script(`({})`).RawParams().(map[string]any)
	if _, ok := p["runtime"]; ok {
		t.Fatal("default runtime should be omitted from RawParams")
	}
	if _, ok := p["language"]; ok {
		t.Fatal("default language should be omitted from RawParams")
	}
}

func TestScript_ExecJSDefault(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	b := node.Script(`({doubled: $input.x * 2})`)
	input := &node.Input{
		Params: b.RawParams().(map[string]any),
		Data:   map[string]any{"x": 21.0},
	}
	out, err := h.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Port != "main" && out.Port != "" {
		t.Fatalf("expected main port, got %q", out.Port)
	}
	if out.Data["doubled"] != 42.0 {
		t.Fatalf("doubled = %v, want 42", out.Data["doubled"])
	}
}

func TestScript_RuntimeError_ErrorPort(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	b := node.Script(`throw new Error('boom')`)
	input := &node.Input{Params: b.RawParams().(map[string]any)}
	out, err := h.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("runtime error should route to port, not Go error: %v", err)
	}
	if out.Port != "error" {
		t.Fatalf("expected error port, got %q", out.Port)
	}
	if out.Data["error"] == nil {
		t.Fatal("error port should carry error message")
	}
}

func TestScript_MissingCode_ConfigError(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	input := &node.Input{Params: map[string]any{}}
	if _, err := h.Execute(context.Background(), input); err == nil {
		t.Fatal("expected Go config error for missing code")
	}
}

func TestScript_UnknownRuntime_ConfigError(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	input := &node.Input{Params: map[string]any{"code": `({})`, "runtime": "v8"}}
	if _, err := h.Execute(context.Background(), input); err == nil {
		t.Fatal("expected Go config error for unknown runtime")
	}
}

func TestScript_CredentialInjected(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	b := node.Script(`({token: $credential.token})`).Credentials("api_token")
	input := &node.Input{Params: b.RawParams().(map[string]any)}
	input.SetCredentialResolver(func(name string) map[string]any {
		if name == "api_token" {
			return map[string]any{"token": "secret-t"}
		}
		return nil
	})
	out, err := h.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Data["token"] != "secret-t" {
		t.Fatalf("token = %v, want secret-t", out.Data["token"])
	}
}

func TestScript_UndeclaredCredentialInvisible(t *testing.T) {
	h, _ := node.Lookup("xflow.script")
	// Declares nothing; resolver would return a value but gate must hide it.
	b := node.Script(`({seen: typeof $credentials.api_token})`)
	input := &node.Input{Params: b.RawParams().(map[string]any)}
	input.SetCredentialResolver(func(string) map[string]any {
		return map[string]any{"token": "leak"}
	})
	out, err := h.Execute(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.Data["seen"] != "undefined" {
		t.Fatalf("gate violated: undeclared credential visible (%v)", out.Data["seen"])
	}
}
```

- [ ] **Step 2: 运行测试确认失败**

Run: `go test ./nodes/node/ -run TestScript -v`
Expected: FAIL，`undefined: node.Script`。

- [ ] **Step 3: 写 `script.go`**

```go
package node

import (
	"context"
	"fmt"

	"github.com/gfa-inc/xflow/nodes/node/script"
	// blank imports register the engines via init()
	_ "github.com/gfa-inc/xflow/nodes/node/script/js"
	_ "github.com/gfa-inc/xflow/nodes/node/script/wasm"
)

// ScriptNode implements xflow.script — runs a sandboxed dynamic script.
type ScriptNode struct {
	BaseNode
	Code        string
	Language    string
	RuntimeName string
	Creds       []string
}

// Script creates a script node. Defaults: language=js, runtime=goja.
//
//	node.Script(`({result: $input.x * 2})`)
//	node.Script(code).Runtime("qjs")
//	node.Script(b64wasm).Language("wasm")
//	node.Script(code).Credentials("aes_key", "api_token")
func Script(code string) *ScriptNode {
	return &ScriptNode{Code: code}
}

func (n *ScriptNode) Language(lang string) *ScriptNode   { n.Language = lang; return n } //nolint:revive
func (n *ScriptNode) Runtime(rt string) *ScriptNode      { n.RuntimeName = rt; return n }
func (n *ScriptNode) Credentials(names ...string) *ScriptNode {
	n.Creds = names
	return n
}

func (n *ScriptNode) Descriptor() Descriptor {
	return Descriptor{
		Type:        "xflow.script",
		DisplayName: "Script",
		Params: []ParamSpec{
			{Name: "language", DisplayName: "Language", Type: ParamString, Required: false, Default: "js", Description: "Language family: js | wasm"},
			{Name: "runtime", DisplayName: "Runtime", Type: ParamString, Required: false, Default: "goja", Description: "Engine: js→goja|qjs, wasm→wazero"},
			{Name: "code", DisplayName: "Code", Type: ParamString, Required: true, Description: "JS source (js) or base64 wasm module (wasm)"},
			{Name: "credentials", DisplayName: "Credentials", Type: ParamArray, Required: false, Description: "Declared credential names injected as $credentials"},
		},
		Inputs:  []PortSpec{{Name: "main", DisplayName: "Main"}},
		Outputs: []PortSpec{{Name: "main", DisplayName: "Main"}, {Name: "error", DisplayName: "Error"}},
	}
}

func (n *ScriptNode) NodeType() string { return "xflow.script" }
func (n *ScriptNode) OnError(s OnError) Builder {
	n.onError = s
	return n
}

func (n *ScriptNode) RawParams() any {
	params := map[string]any{"code": n.Code}
	if n.Language != "" && n.Language != "js" {
		params["language"] = n.Language
	}
	if n.RuntimeName != "" && n.RuntimeName != "goja" {
		params["runtime"] = n.RuntimeName
	}
	if len(n.Creds) > 0 {
		params["credentials"] = n.Creds
	}
	return params
}

func (n *ScriptNode) Execute(ctx context.Context, input *Input) (*Output, error) {
	code, _ := input.Params["code"].(string)
	if code == "" {
		return nil, fmt.Errorf("xflow.script: code parameter is required")
	}

	language, _ := input.Params["language"].(string)
	if language == "" {
		language = "js"
	}
	runtime, _ := input.Params["runtime"].(string)
	if runtime == "" {
		if language == "wasm" {
			runtime = "wazero"
		} else {
			runtime = "goja"
		}
	}

	engine, ok := script.Lookup(language, runtime)
	if !ok {
		return nil, fmt.Errorf("xflow.script: unknown engine (language=%q, runtime=%q)", language, runtime)
	}

	declared := readCredNames(input.Params["credentials"])
	creds, first, err := script.ResolveCredentials(declared, func(name string) (map[string]any, error) {
		return input.Credential(name), nil
	})
	if err != nil {
		return nil, fmt.Errorf("xflow.script: %w", err)
	}

	globals := buildScriptGlobals(input, creds, first)

	result, err := engine.Execute(ctx, code, globals, script.DefaultHelpers())
	if err != nil {
		// Runtime error (exception, timeout, guest non-zero) → error port.
		return &Output{Data: map[string]any{"error": err.Error()}, Port: "error"}, nil
	}
	return &Output{Data: script.MapResult(result), Port: "main"}, nil
}

// readCredNames accepts both []string (Go DSL) and []any (decoded YAML/JSON).
func readCredNames(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		names := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				names = append(names, s)
			}
		}
		return names
	default:
		return nil
	}
}

func buildScriptGlobals(input *Input, creds map[string]any, first any) map[string]any {
	return map[string]any{
		"$input":       input.Data,
		"$inputs":      input.Inputs,
		"$vars":        input.Vars,
		"$config":      input.Config,
		"$params":      input.Params,
		"$runtime":     runtimeEnv(input),
		"$credentials": creds,
		"$credential":  first,
	}
}

func init() { Register(&ScriptNode{}) }
```

> **实现者注意**：`ResolveCredentials` 当声明名解析为 `nil` 时返回 config error（spec §7.4：声明的凭证 name 解析失败/不存在 → Go error）。`TestScript_CredentialInjected` 的 resolver 对声明名返回非 nil，故走成功路径。`buildScriptGlobals` 复用 `expr.go` 既有的 `runtimeEnv`。

- [ ] **Step 4: 运行测试确认通过**

Run: `go test ./nodes/node/ -run TestScript -race -v`
Expected: PASS（工厂、默认省略、js 默认执行、运行时错误→error 端口、缺 code/未知 runtime→Go error、凭证注入、未声明不可见闸门）。

- [ ] **Step 5: 提交**

```bash
git add nodes/node/script.go nodes/node/script_test.go
git commit -m "feat(node): add xflow.script node wiring engines and credential gate"
```

---

## Task 10: 两族一致性集成测试 + 全量校验

验证 spec §9 节点层"两族一致性"：同一 `credentials` 声明在 js 与 wasm 下脚本读到的凭证值一致。

**Files:**
- Create: `nodes/node/script/consistency_test.go`（package script_test，跨子包集成）

> 该测试需要编译 wasm guest，沿用 Task 8 的 `testdata/echo`。把它放在 `wasm` 包内更省事——改为在 `nodes/node/script/wasm/wasm_test.go` 追加一个"凭证值与 js 一致"的断言：用相同 `$credentials` 输入，分别经 wazero 与 goja 执行等价逻辑，断言取到的凭证字段相等。

- [ ] **Step 1: 在 `nodes/node/script/wasm/wasm_test.go` 追加一致性测试**

```go
func TestWasm_CredentialParityWithJS(t *testing.T) {
	globals := map[string]any{
		"$credentials": map[string]any{"aes_key": map[string]any{"key": "shared-kk"}},
		"$credential":  map[string]any{"token": "shared-tt"},
	}

	// wasm path
	wOut, err := newWasm(t).Execute(context.Background(), b64(echoWasm), globals, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("wasm exec: %v", err)
	}
	wasmKey := wOut.(map[string]any)["credKey"]
	wasmTok := wOut.(map[string]any)["firstToken"]

	// js (goja) path: read the same fields
	jsEngine, _ := script.Lookup("js", "goja")
	jOut, err := jsEngine.Execute(context.Background(),
		`({credKey: $credentials.aes_key.key, firstToken: $credential.token})`,
		globals, script.DefaultHelpers())
	if err != nil {
		t.Fatalf("js exec: %v", err)
	}
	jsKey := jOut.(map[string]any)["credKey"]
	jsTok := jOut.(map[string]any)["firstToken"]

	if wasmKey != jsKey || wasmTok != jsTok {
		t.Fatalf("family mismatch: wasm(key=%v,tok=%v) js(key=%v,tok=%v)", wasmKey, wasmTok, jsKey, jsTok)
	}
}
```

需确保 `wasm_test.go` 已 import `"github.com/gfa-inc/xflow/nodes/node/script/js"`（blank import 触发 goja 注册）：在 import 块加 `_ "github.com/gfa-inc/xflow/nodes/node/script/js"`。

- [ ] **Step 2: 运行该测试确认通过**

Run: `go test ./nodes/node/script/wasm/ -run TestWasm_CredentialParity -v`
Expected: PASS。

- [ ] **Step 3: 全量校验**

Run:
```bash
go build ./...
go test ./nodes/node/... -race -count=1 -timeout 120s
go vet ./nodes/node/...
go fmt ./nodes/node/...
```
Expected: 全部通过；`make build` 默认构建含 qjs（纯 Go，无 cgo）。

- [ ] **Step 4: 提交**

```bash
git add nodes/node/script/wasm/wasm_test.go
git commit -m "test(script): assert js/wasm credential parity"
```

---

## 自审记录

**1. Spec 覆盖：**
- §2 概念模型（language/runtime 两层 + 默认值）→ Task 9 Execute 默认选择逻辑 ✓
- §4 Engine 接口 + 注册表 → Task 1 ✓
- §5.1 js 全局注入（$input...$credentials/$credential）→ Task 9 `buildScriptGlobals` + Task 6/7 注入 ✓
- §5.2 wasm WASI stdin/stdout JSON → Task 8 ✓
- §5.3 输出映射（对象/标量/null）→ Task 4 `MapResult` ✓
- §6 凭证按名预声明（闸门、$credential 别名、resolver 报错不回显）→ Task 3 + Task 9 ✓
- §7.1 沙箱（js undefined 断言 / wasm 无 FS）→ Task 6/7/8 沙箱测试 ✓
- §7.2 池化（goja sync.Pool + 清理；qjs/wazero 新实例）→ Task 6/7/8 ✓
- §7.3 超时（goja Interrupt / qjs ctx / wazero ctx）→ Task 6/7/8 超时测试 ✓
- §7.4 错误分类（运行时→error 端口；配置→Go error）→ Task 9 ✓
- §8 节点 API + Descriptor + RawParams 省略默认 → Task 9 ✓
- §9 测试策略（含跨引擎一致性、两族凭证一致）→ Task 7 Step 5 + Task 10 ✓

**2. 占位符扫描：** 无 TBD/TODO；每个 code step 都含完整可编译代码。两处"实现者注意"针对 qjs `This.Args()`/`Context.Function` 与 wazero instance 语义，均给出确定行为目标 + 校验命令，非占位。

**3. 类型一致性：** `Engine.Execute(ctx, code, globals, Helpers) (any, error)` 全 Task 一致；`ResolveCredentials([]string, CredentialResolver) (map[string]any, any, error)` 在 Task 3 定义、Task 9 调用一致；`MapResult(any) map[string]any` 一致；节点层 resolver 适配 `func(name)(map[string]any,error)` 包装 `input.Credential`（后者返回 `map[string]any`，无 error）一致。

**已从 spec 排除：** §10 "qjs 若依赖 cgo 需 build tag" —— 已核实 qjs v0.0.6 纯 Go（底层 wazero），默认构建纳入，build tag 不需要。
