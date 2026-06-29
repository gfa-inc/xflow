# XFlow Runner Cobra CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace `cmd/runner`'s single `flag` entrypoint with a `spf13/cobra` CLI that supports `run`, `verify`, `config validate`, and `config sample` while preserving current runner behavior.

**Architecture:** Keep cobra, YAML/env/flag config resolution, and process wiring inside `cmd/runner`. `service/runner`, `service/protocol`, `execution`, `engine`, SDK, and backend packages remain independent of cobra and runner CLI concerns. The runner still talks only to xflow-server through Runner Protocol and never connects to Redis, Asynq, MySQL, or StateStore.

**Tech Stack:** Go, `github.com/spf13/cobra`, `gopkg.in/yaml.v3`, existing `service/protocol`, existing `service/runner`, existing `execution.Registry`.

## Global Constraints

- `engine/` must NOT import redis/asynq/mysql/sql.
- `execution/` and `backend/memory/` must remain free of Redis/Asynq/MySQL/network dependencies.
- SDK should assemble reusable backend providers; server/runner code must not depend on SDK internals.
- Core packages (engine/nodes/types/store/sdk) must NEVER import `service/` or `cmd/`.
- Runner must not connect to Redis, Asynq, MySQL, or StateStore directly.
- Preserve current defaults: server `http://localhost:8080`, runner id `runner-<pid>`, concurrency `1`, capability `xflow.function`.
- Configuration resolution order is `CLI flags > XFLOW_RUNNER_* environment variables > config file > defaults`.
- First implementation commands are `run`, `verify`, `config validate`, and `config sample`.
- Do not implement durable runner registration, unregister, list, OS service management, dynamic reload, or multi-runner config in this plan.
- Existing direct flags must still work on `xflow-runner run`.

---

## File Structure

- Modify `go.mod` / `go.sum`: add direct dependencies on `github.com/spf13/cobra` and `gopkg.in/yaml.v3`.
- Modify `cmd/runner/main.go`: reduce it to root command execution.
- Create `cmd/runner/command.go`: cobra root command, shared options, output/error stream injection for tests.
- Create `cmd/runner/config.go`: config structs, defaults, YAML loading, env overrides, flag merge, validation, and capability parsing.
- Create `cmd/runner/run.go`: `run` command and existing `runRunner` process wiring.
- Create `cmd/runner/verify.go`: `verify` command and protocol reachability check.
- Create `cmd/runner/config_command.go`: `config validate` and `config sample` commands.
- Modify `cmd/runner/main_test.go`: replace old `flag` parser test with cobra/config tests.
- Create `cmd/runner/config_test.go`: focused config precedence and validation tests.
- Create `cmd/runner/verify_test.go`: httptest-based verify command test.

---

### Task 1: Cobra Root and Run Command Compatibility

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `cmd/runner/main.go`
- Create: `cmd/runner/command.go`
- Create: `cmd/runner/run.go`
- Modify: `cmd/runner/main_test.go`

**Interfaces:**
- Consumes: existing `runnerConfig`, `parseCapabilities(raw string) []protocol.Capability`, and `runRunner(ctx context.Context, cfg runnerConfig) error` behavior from `cmd/runner/main.go`.
- Produces:
  - `type commandOptions struct`
  - `func newRootCommand(opts commandOptions) *cobra.Command`
  - `func newRunCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command`
  - `func executeRoot(args ...string) error`
  - `func parseCapabilities(raw string) []protocol.Capability`
  - `func runRunner(ctx context.Context, cfg runnerConfig) error`

- [ ] **Step 1: Add failing cobra compatibility tests**

Replace `cmd/runner/main_test.go` with tests that execute the cobra root in-memory:

```go
package main

import (
	"bytes"
	"testing"
)

func TestRunCommandParsesExistingFlags(t *testing.T) {
	var ran runnerConfig
	cmd := newRootCommand(commandOptions{
		runFunc: func(cfg runnerConfig) error {
			ran = cfg
			return nil
		},
		out: &bytes.Buffer{},
		err: &bytes.Buffer{},
	})
	cmd.SetArgs([]string{
		"run",
		"--server", "http://localhost:8080",
		"--id", "runner-1",
		"--concurrency", "2",
		"--cap", "xflow.function,xflow.http",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ran.serverURL != "http://localhost:8080" || ran.runnerID != "runner-1" || ran.concurrency != 2 {
		t.Fatalf("config = %+v", ran)
	}
	if len(ran.capabilities) != 2 || ran.capabilities[0].NodeType != "xflow.function" || ran.capabilities[1].NodeType != "xflow.http" {
		t.Fatalf("capabilities = %+v", ran.capabilities)
	}
}

func TestRootDefaultsToRunCommand(t *testing.T) {
	var ran runnerConfig
	cmd := newRootCommand(commandOptions{
		runFunc: func(cfg runnerConfig) error {
			ran = cfg
			return nil
		},
		out: &bytes.Buffer{},
		err: &bytes.Buffer{},
	})
	cmd.SetArgs([]string{"--id", "runner-root"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ran.runnerID != "runner-root" {
		t.Fatalf("runner id = %q, want runner-root", ran.runnerID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestRunCommandParsesExistingFlags|TestRootDefaultsToRunCommand'
```

Expected: FAIL because `newRootCommand` and `commandOptions` are not defined.

- [ ] **Step 3: Add cobra dependency**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go get github.com/spf13/cobra@latest
```

Expected: `go.mod` contains a direct `github.com/spf13/cobra` requirement and `go.sum` is updated.

- [ ] **Step 4: Move runner process wiring into `run.go`**

Create `cmd/runner/run.go` with the existing process wiring and a command factory:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/gfa-inc/xflow/execution"
	_ "github.com/gfa-inc/xflow/nodes/node"
	"github.com/gfa-inc/xflow/service/protocol"
	runnersvc "github.com/gfa-inc/xflow/service/runner"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

type runnerConfig struct {
	configPath        string
	serverURL         string
	runnerID          string
	concurrency       int
	changed           map[string]bool
	capabilities      []protocol.Capability
	heartbeatInterval string
	pollWait          string
	logLevel          string
	logFormat         string
}

func newRunCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the xflow task runner",
		RunE: func(cmd *cobra.Command, _ []string) error {
			recordChangedFlags(cmd, cfg)
			resolved, err := resolveRunnerConfig(*cfg)
			if err != nil {
				return err
			}
			return opts.runFunc(resolved)
		},
	}
	bindRunnerFlags(cmd, cfg)
	return cmd
}

func bindRunnerFlags(cmd *cobra.Command, cfg *runnerConfig) {
	cmd.Flags().StringVar(&cfg.serverURL, "server", cfg.serverURL, "xflow-server base URL")
	cmd.Flags().StringVar(&cfg.runnerID, "id", cfg.runnerID, "Runner ID")
	cmd.Flags().IntVar(&cfg.concurrency, "concurrency", cfg.concurrency, "Runner concurrency")
	cmd.Flags().String("cap", "xflow.function", "Comma-separated node type capabilities")
	cmd.Flags().StringVar(&cfg.heartbeatInterval, "heartbeat-interval", cfg.heartbeatInterval, "Heartbeat interval")
	cmd.Flags().StringVar(&cfg.pollWait, "poll-wait", cfg.pollWait, "Poll wait duration when no task is available")
	cmd.Flags().StringVar(&cfg.logLevel, "log-level", cfg.logLevel, "Log level: debug, info, warn, error")
	cmd.Flags().StringVar(&cfg.logFormat, "log-format", cfg.logFormat, "Log format: text, json")
}

func recordChangedFlags(cmd *cobra.Command, cfg *runnerConfig) {
	if cfg.changed == nil {
		cfg.changed = map[string]bool{}
	}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		cfg.changed[f.Name] = true
	})
}

func parseCapabilities(raw string) []protocol.Capability {
	parts := strings.Split(raw, ",")
	capabilities := make([]protocol.Capability, 0, len(parts))
	for _, part := range parts {
		nodeType := strings.TrimSpace(part)
		if nodeType == "" {
			continue
		}
		capabilities = append(capabilities, protocol.Capability{NodeType: nodeType})
	}
	return capabilities
}

func runRunner(ctx context.Context, cfg runnerConfig) error {
	client := protocol.NewClient(cfg.serverURL, http.DefaultClient)
	registry := execution.NewRegistry()
	runner := runnersvc.New(client, registry, runnersvc.Config{
		RunnerID:     cfg.runnerID,
		Concurrency:  cfg.concurrency,
		Capabilities: cfg.capabilities,
	})
	return runner.Run(ctx)
}

func runWithSignals(cfg runnerConfig) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if cfg.runnerID == "" {
		cfg.runnerID = fmt.Sprintf("runner-%d", os.Getpid())
	}
	return runRunner(ctx, cfg)
}
```

- [ ] **Step 5: Add `command.go` and wire root command**

Create `cmd/runner/command.go`:

```go
package main

import (
	"io"
	"os"

	"github.com/spf13/cobra"
)

type commandOptions struct {
	runFunc func(runnerConfig) error
	out     io.Writer
	err     io.Writer
}

func newRootCommand(opts commandOptions) *cobra.Command {
	if opts.runFunc == nil {
		opts.runFunc = runWithSignals
	}
	if opts.out == nil {
		opts.out = os.Stdout
	}
	if opts.err == nil {
		opts.err = os.Stderr
	}

	cfg := defaultRunnerConfig()
	cfg.configPath = os.Getenv("XFLOW_RUNNER_CONFIG")
	root := &cobra.Command{
		Use:           "xflow-runner",
		Short:         "XFlow task runner",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			recordChangedFlags(cmd, &cfg)
			resolved, err := resolveRunnerConfig(cfg)
			if err != nil {
				return err
			}
			return opts.runFunc(resolved)
		},
	}
	root.SetOut(opts.out)
	root.SetErr(opts.err)
	root.PersistentFlags().StringVarP(&cfg.configPath, "config", "c", cfg.configPath, "Runner config file")
	bindRunnerFlags(root, &cfg)
	root.AddCommand(newRunCommand(opts, &cfg))
	return root
}

func executeRoot(args ...string) error {
	cmd := newRootCommand(commandOptions{})
	cmd.SetArgs(args)
	return cmd.Execute()
}
```

- [ ] **Step 6: Simplify `main.go`**

Replace `cmd/runner/main.go` with:

```go
package main

import (
	"log"
	"os"
)

func main() {
	if err := executeRoot(os.Args[1:]...); err != nil {
		log.Fatal(err)
	}
}
```

- [ ] **Step 7: Add temporary default config resolver**

Create `cmd/runner/config.go` with enough implementation for Task 1:

```go
package main

import (
	"fmt"
	"os"
)

func defaultRunnerConfig() runnerConfig {
	return runnerConfig{
		serverURL:         "http://localhost:8080",
		runnerID:          fmt.Sprintf("runner-%d", os.Getpid()),
		concurrency:       1,
		capabilities:      parseCapabilities("xflow.function"),
		heartbeatInterval: "5s",
		pollWait:          "1s",
		logLevel:          "info",
		logFormat:         "text",
	}
}

func resolveRunnerConfig(cfg runnerConfig) (runnerConfig, error) {
	return cfg, nil
}
```

- [ ] **Step 8: Fix `--cap` flag assignment**

In `bindRunnerFlags`, store the raw cap flag in a string field. Update `runnerConfig`:

```go
type runnerConfig struct {
	configPath        string
	serverURL         string
	runnerID          string
	concurrency       int
	changed           map[string]bool
	capRaw            string
	capabilities      []protocol.Capability
	heartbeatInterval string
	pollWait          string
	logLevel          string
	logFormat         string
}
```

Update defaults:

```go
capRaw:       "xflow.function",
capabilities: parseCapabilities("xflow.function"),
```

Update flag binding:

```go
cmd.Flags().StringVar(&cfg.capRaw, "cap", cfg.capRaw, "Comma-separated node type capabilities")
```

Update `resolveRunnerConfig`:

```go
func resolveRunnerConfig(cfg runnerConfig) (runnerConfig, error) {
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	return cfg, nil
}
```

- [ ] **Step 9: Run tests**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestRunCommandParsesExistingFlags|TestRootDefaultsToRunCommand'
```

Expected: PASS.

- [ ] **Step 10: Commit**

Run:

```bash
git add go.mod go.sum cmd/runner/main.go cmd/runner/command.go cmd/runner/config.go cmd/runner/run.go cmd/runner/main_test.go
git commit -m "feat(runner): add cobra run command"
```

---

### Task 2: YAML, Environment, Flag Precedence, and Validation

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `cmd/runner/config.go`
- Create: `cmd/runner/config_test.go`

**Interfaces:**
- Consumes: `runnerConfig`, `defaultRunnerConfig() runnerConfig`, and `parseCapabilities(raw string) []protocol.Capability`.
- Produces:
  - `func loadRunnerConfig(path string) (runnerConfig, error)`
  - `func applyEnvOverrides(cfg runnerConfig, getenv func(string) string) runnerConfig`
  - `func validateRunnerConfig(cfg runnerConfig) error`
  - `func resolveRunnerConfig(base runnerConfig) (runnerConfig, error)`
  - `func sampleRunnerConfigYAML() string`

- [ ] **Step 1: Write failing precedence and validation tests**

Create `cmd/runner/config_test.go`:

```go
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadRunnerConfigFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.yaml")
	data := []byte(`
runner:
  id: file-runner
  concurrency: 3
  capabilities:
    - xflow.http
server:
  url: http://file-server:8080
poll:
  wait: 2s
heartbeat:
  interval: 7s
logging:
  level: debug
  format: json
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadRunnerConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.serverURL != "http://file-server:8080" || cfg.runnerID != "file-runner" || cfg.concurrency != 3 {
		t.Fatalf("config = %+v", cfg)
	}
	if cfg.capRaw != "xflow.http" {
		t.Fatalf("capRaw = %q", cfg.capRaw)
	}
	if cfg.pollWait != "2s" || cfg.heartbeatInterval != "7s" || cfg.logLevel != "debug" || cfg.logFormat != "json" {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestRunCommandUsesConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.yaml")
	data := []byte(`
runner:
  id: file-runner
  concurrency: 3
  capabilities:
    - xflow.http
server:
  url: http://file-server:8080
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	var ran runnerConfig
	cmd := newRootCommand(commandOptions{
		runFunc: func(cfg runnerConfig) error {
			ran = cfg
			return nil
		},
		out: &bytes.Buffer{},
		err: &bytes.Buffer{},
	})
	cmd.SetArgs([]string{"run", "--config", path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ran.serverURL != "http://file-server:8080" || ran.runnerID != "file-runner" || ran.concurrency != 3 {
		t.Fatalf("config = %+v", ran)
	}
	if len(ran.capabilities) != 1 || ran.capabilities[0].NodeType != "xflow.http" {
		t.Fatalf("capabilities = %+v", ran.capabilities)
	}
}

func TestEnvOverridesFileConfig(t *testing.T) {
	cfg := runnerConfig{
		serverURL:         "http://file-server:8080",
		runnerID:          "file-runner",
		concurrency:       2,
		capRaw:            "xflow.http",
		heartbeatInterval: "5s",
		pollWait:          "1s",
		logLevel:          "info",
		logFormat:         "text",
	}
	env := map[string]string{
		"XFLOW_RUNNER_SERVER":             "http://env-server:8080",
		"XFLOW_RUNNER_ID":                 "env-runner",
		"XFLOW_RUNNER_CONCURRENCY":        "4",
		"XFLOW_RUNNER_CAP":                "xflow.function,xflow.http",
		"XFLOW_RUNNER_HEARTBEAT_INTERVAL": "9s",
		"XFLOW_RUNNER_POLL_WAIT":          "3s",
		"XFLOW_RUNNER_LOG_LEVEL":          "warn",
		"XFLOW_RUNNER_LOG_FORMAT":         "json",
	}
	got := applyEnvOverrides(cfg, func(key string) string { return env[key] })
	if got.serverURL != "http://env-server:8080" || got.runnerID != "env-runner" || got.concurrency != 4 {
		t.Fatalf("config = %+v", got)
	}
	if got.capRaw != "xflow.function,xflow.http" || got.heartbeatInterval != "9s" || got.pollWait != "3s" {
		t.Fatalf("config = %+v", got)
	}
	if got.logLevel != "warn" || got.logFormat != "json" {
		t.Fatalf("config = %+v", got)
	}
}

func TestValidateRunnerConfigRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		mut  func(*runnerConfig)
		want string
	}{
		{name: "server", mut: func(c *runnerConfig) { c.serverURL = "localhost:8080" }, want: "server URL"},
		{name: "id", mut: func(c *runnerConfig) { c.runnerID = "" }, want: "runner id"},
		{name: "concurrency", mut: func(c *runnerConfig) { c.concurrency = 0 }, want: "concurrency"},
		{name: "capabilities", mut: func(c *runnerConfig) { c.capRaw = "," }, want: "capabilities"},
		{name: "heartbeat", mut: func(c *runnerConfig) { c.heartbeatInterval = "-1s" }, want: "heartbeat interval"},
		{name: "poll", mut: func(c *runnerConfig) { c.pollWait = "bad" }, want: "poll wait"},
		{name: "level", mut: func(c *runnerConfig) { c.logLevel = "trace" }, want: "log level"},
		{name: "format", mut: func(c *runnerConfig) { c.logFormat = "pretty" }, want: "log format"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := defaultRunnerConfig()
			tt.mut(&cfg)
			err := validateRunnerConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestLoadRunnerConfigFromYAML|TestRunCommandUsesConfigFile|TestEnvOverridesFileConfig|TestValidateRunnerConfigRejectsInvalidValues'
```

Expected: FAIL because config loading and validation functions are missing.

- [ ] **Step 3: Add YAML dependency**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go get gopkg.in/yaml.v3@v3.0.1
```

Expected: `go.mod` contains `gopkg.in/yaml.v3 v3.0.1` and `go.sum` is updated.

- [ ] **Step 4: Implement YAML config structs and loader**

Replace `cmd/runner/config.go` with:

```go
package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type runnerConfigFile struct {
	Runner struct {
		ID           string   `yaml:"id"`
		Concurrency  int      `yaml:"concurrency"`
		Capabilities []string `yaml:"capabilities"`
	} `yaml:"runner"`
	Server struct {
		URL string `yaml:"url"`
	} `yaml:"server"`
	Poll struct {
		Wait string `yaml:"wait"`
	} `yaml:"poll"`
	Heartbeat struct {
		Interval string `yaml:"interval"`
	} `yaml:"heartbeat"`
	Logging struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"logging"`
}

func defaultRunnerConfig() runnerConfig {
	return runnerConfig{
		serverURL:         "http://localhost:8080",
		runnerID:          fmt.Sprintf("runner-%d", os.Getpid()),
		concurrency:       1,
		capRaw:            "xflow.function",
		capabilities:      parseCapabilities("xflow.function"),
		heartbeatInterval: "5s",
		pollWait:          "1s",
		logLevel:          "info",
		logFormat:         "text",
	}
}

func loadRunnerConfig(path string) (runnerConfig, error) {
	cfg := defaultRunnerConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runnerConfig{}, err
	}
	var file runnerConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return runnerConfig{}, err
	}
	if file.Server.URL != "" {
		cfg.serverURL = file.Server.URL
	}
	if file.Runner.ID != "" {
		cfg.runnerID = file.Runner.ID
	}
	if file.Runner.Concurrency != 0 {
		cfg.concurrency = file.Runner.Concurrency
	}
	if len(file.Runner.Capabilities) > 0 {
		cfg.capRaw = strings.Join(file.Runner.Capabilities, ",")
	}
	if file.Poll.Wait != "" {
		cfg.pollWait = file.Poll.Wait
	}
	if file.Heartbeat.Interval != "" {
		cfg.heartbeatInterval = file.Heartbeat.Interval
	}
	if file.Logging.Level != "" {
		cfg.logLevel = file.Logging.Level
	}
	if file.Logging.Format != "" {
		cfg.logFormat = file.Logging.Format
	}
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	return cfg, nil
}
```

- [ ] **Step 5: Implement env overrides**

Append to `cmd/runner/config.go`:

```go
func applyEnvOverrides(cfg runnerConfig, getenv func(string) string) runnerConfig {
	if v := getenv("XFLOW_RUNNER_SERVER"); v != "" {
		cfg.serverURL = v
	}
	if v := getenv("XFLOW_RUNNER_ID"); v != "" {
		cfg.runnerID = v
	}
	if v := getenv("XFLOW_RUNNER_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.concurrency = n
		}
	}
	if v := getenv("XFLOW_RUNNER_CAP"); v != "" {
		cfg.capRaw = v
	}
	if v := getenv("XFLOW_RUNNER_HEARTBEAT_INTERVAL"); v != "" {
		cfg.heartbeatInterval = v
	}
	if v := getenv("XFLOW_RUNNER_POLL_WAIT"); v != "" {
		cfg.pollWait = v
	}
	if v := getenv("XFLOW_RUNNER_LOG_LEVEL"); v != "" {
		cfg.logLevel = v
	}
	if v := getenv("XFLOW_RUNNER_LOG_FORMAT"); v != "" {
		cfg.logFormat = v
	}
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	return cfg
}
```

- [ ] **Step 6: Implement validation**

Append to `cmd/runner/config.go`:

```go
func validateRunnerConfig(cfg runnerConfig) error {
	u, err := url.Parse(cfg.serverURL)
	if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("server URL must be an absolute http or https URL: %q", cfg.serverURL)
	}
	if strings.TrimSpace(cfg.runnerID) == "" {
		return errors.New("runner id is required")
	}
	if cfg.concurrency <= 0 {
		return fmt.Errorf("concurrency must be greater than zero: %d", cfg.concurrency)
	}
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	if len(cfg.capabilities) == 0 {
		return errors.New("capabilities must contain at least one node type")
	}
	if err := validatePositiveDuration("heartbeat interval", cfg.heartbeatInterval); err != nil {
		return err
	}
	if err := validatePositiveDuration("poll wait", cfg.pollWait); err != nil {
		return err
	}
	switch cfg.logLevel {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("log level must be debug, info, warn, or error: %q", cfg.logLevel)
	}
	switch cfg.logFormat {
	case "text", "json":
	default:
		return fmt.Errorf("log format must be text or json: %q", cfg.logFormat)
	}
	return nil
}

func validatePositiveDuration(name, raw string) error {
	d, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("%s must be a valid duration: %w", name, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s must be positive: %s", name, raw)
	}
	return nil
}
```

- [ ] **Step 7: Implement final resolver and sample YAML**

Append to `cmd/runner/config.go`:

```go
func resolveRunnerConfig(base runnerConfig) (runnerConfig, error) {
	cfg, err := loadRunnerConfig(base.configPath)
	if err != nil {
		return runnerConfig{}, err
	}
	cfg = applyEnvOverrides(cfg, os.Getenv)
	if base.changed["server"] {
		cfg.serverURL = base.serverURL
	}
	if base.changed["id"] {
		cfg.runnerID = base.runnerID
	}
	if base.changed["concurrency"] {
		cfg.concurrency = base.concurrency
	}
	if base.changed["cap"] {
		cfg.capRaw = base.capRaw
	}
	if base.changed["heartbeat-interval"] {
		cfg.heartbeatInterval = base.heartbeatInterval
	}
	if base.changed["poll-wait"] {
		cfg.pollWait = base.pollWait
	}
	if base.changed["log-level"] {
		cfg.logLevel = base.logLevel
	}
	if base.changed["log-format"] {
		cfg.logFormat = base.logFormat
	}
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	if err := validateRunnerConfig(cfg); err != nil {
		return runnerConfig{}, err
	}
	return cfg, nil
}

func sampleRunnerConfigYAML() string {
	return `runner:
  id: "runner-1"
  concurrency: 2
  capabilities:
    - "xflow.function"

server:
  url: "http://localhost:8080"

poll:
  wait: "1s"

heartbeat:
  interval: "5s"

logging:
  level: "info"
  format: "text"
`
}
```

- [ ] **Step 8: Run tests**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestLoadRunnerConfigFromYAML|TestRunCommandUsesConfigFile|TestEnvOverridesFileConfig|TestValidateRunnerConfigRejectsInvalidValues'
```

Expected: PASS.

- [ ] **Step 9: Commit**

Run:

```bash
git add go.mod go.sum cmd/runner/config.go cmd/runner/config_test.go
git commit -m "feat(runner): add config resolution"
```

---

### Task 3: Config Commands and Verify Command

**Files:**
- Modify: `cmd/runner/command.go`
- Create: `cmd/runner/config_command.go`
- Create: `cmd/runner/verify.go`
- Create: `cmd/runner/verify_test.go`
- Modify: `cmd/runner/main_test.go`

**Interfaces:**
- Consumes: `resolveRunnerConfig(base runnerConfig) (runnerConfig, error)`, `sampleRunnerConfigYAML() string`, `protocol.NewClient`, `protocol.RegisterRunnerRequest`, `protocol.HeartbeatRequest`.
- Produces:
  - `func newConfigCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command`
  - `func newVerifyCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command`
  - `func verifyRunner(ctx context.Context, cfg runnerConfig) error`

- [ ] **Step 1: Add tests for `config sample`, `config validate`, and `verify`**

Append to `cmd/runner/main_test.go`:

```go
func TestConfigSamplePrintsYAML(t *testing.T) {
	var out bytes.Buffer
	cmd := newRootCommand(commandOptions{out: &out, err: &bytes.Buffer{}})
	cmd.SetArgs([]string{"config", "sample"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "runner:") || !strings.Contains(out.String(), "server:") {
		t.Fatalf("sample output = %q", out.String())
	}
	if _, err := loadRunnerConfigFromBytes([]byte(out.String())); err != nil {
		t.Fatalf("sample is not parseable: %v", err)
	}
}

func TestConfigValidateRejectsInvalidFlagConfig(t *testing.T) {
	cmd := newRootCommand(commandOptions{out: &bytes.Buffer{}, err: &bytes.Buffer{}})
	cmd.SetArgs([]string{"config", "validate", "--server", "localhost:8080"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "server URL") {
		t.Fatalf("error = %v, want server URL validation", err)
	}
}
```

Create `cmd/runner/verify_test.go`:

```go
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gfa-inc/xflow/service/protocol"
)

func TestVerifyCommandRegistersAndHeartbeats(t *testing.T) {
	var registered protocol.RegisterRunnerRequest
	var heartbeat protocol.HeartbeatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case protocol.RegisterRunnerPath:
			if err := json.NewDecoder(r.Body).Decode(&registered); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(protocol.RegisterRunnerResponse{RunnerID: registered.RunnerID})
		case protocol.HeartbeatPath:
			if err := json.NewDecoder(r.Body).Decode(&heartbeat); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(protocol.HeartbeatResponse{ServerTime: 1})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var out bytes.Buffer
	cmd := newRootCommand(commandOptions{out: &out, err: &bytes.Buffer{}})
	cmd.SetArgs([]string{
		"verify",
		"--server", server.URL,
		"--id", "runner-verify",
		"--concurrency", "2",
		"--cap", "xflow.function,xflow.http",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if registered.RunnerID != "runner-verify" || registered.Concurrency != 2 {
		t.Fatalf("registered = %+v", registered)
	}
	if len(registered.Capabilities) != 2 {
		t.Fatalf("registered capabilities = %+v", registered.Capabilities)
	}
	if heartbeat.RunnerID != "runner-verify" || heartbeat.Capacity != 2 {
		t.Fatalf("heartbeat = %+v", heartbeat)
	}
	if !strings.Contains(out.String(), "runner verified") {
		t.Fatalf("output = %q", out.String())
	}
}
```

- [ ] **Step 2: Add missing imports in tests**

Ensure `cmd/runner/main_test.go` imports:

```go
import (
	"bytes"
	"strings"
	"testing"
)
```

Ensure `cmd/runner/verify_test.go` imports `strings`:

```go
import "strings"
```

- [ ] **Step 3: Run tests to verify they fail**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestConfigSamplePrintsYAML|TestConfigValidateRejectsInvalidFlagConfig|TestVerifyCommandRegistersAndHeartbeats'
```

Expected: FAIL because commands and `loadRunnerConfigFromBytes` are missing.

- [ ] **Step 4: Add parse-from-bytes helper**

In `cmd/runner/config.go`, extract YAML parsing so tests and file loading can share it:

```go
func loadRunnerConfigFromBytes(data []byte) (runnerConfig, error) {
	cfg := defaultRunnerConfig()
	var file runnerConfigFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return runnerConfig{}, err
	}
	if file.Server.URL != "" {
		cfg.serverURL = file.Server.URL
	}
	if file.Runner.ID != "" {
		cfg.runnerID = file.Runner.ID
	}
	if file.Runner.Concurrency != 0 {
		cfg.concurrency = file.Runner.Concurrency
	}
	if len(file.Runner.Capabilities) > 0 {
		cfg.capRaw = strings.Join(file.Runner.Capabilities, ",")
	}
	if file.Poll.Wait != "" {
		cfg.pollWait = file.Poll.Wait
	}
	if file.Heartbeat.Interval != "" {
		cfg.heartbeatInterval = file.Heartbeat.Interval
	}
	if file.Logging.Level != "" {
		cfg.logLevel = file.Logging.Level
	}
	if file.Logging.Format != "" {
		cfg.logFormat = file.Logging.Format
	}
	cfg.capabilities = parseCapabilities(cfg.capRaw)
	return cfg, nil
}
```

Change `loadRunnerConfig` to:

```go
func loadRunnerConfig(path string) (runnerConfig, error) {
	if path == "" {
		return defaultRunnerConfig(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return runnerConfig{}, err
	}
	return loadRunnerConfigFromBytes(data)
}
```

- [ ] **Step 5: Implement config commands**

Create `cmd/runner/config_command.go`:

```go
package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newConfigCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Manage runner configuration",
	}

	validate := &cobra.Command{
		Use:   "validate",
		Short: "Validate runner configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			recordChangedFlags(cmd, cfg)
			resolved, err := resolveRunnerConfig(*cfg)
			if err != nil {
				return err
			}
			fmt.Fprintf(opts.out, "runner config valid: %s\n", resolved.runnerID)
			return nil
		},
	}
	bindRunnerFlags(validate, cfg)

	sample := &cobra.Command{
		Use:   "sample",
		Short: "Print a sample runner configuration",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprint(opts.out, sampleRunnerConfigYAML())
			return nil
		},
	}

	cmd.AddCommand(validate, sample)
	return cmd
}
```

- [ ] **Step 6: Implement verify command**

Create `cmd/runner/verify.go`:

```go
package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gfa-inc/xflow/service/protocol"
	"github.com/spf13/cobra"
)

func newVerifyCommand(opts commandOptions, cfg *runnerConfig) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify runner configuration and server reachability",
		RunE: func(cmd *cobra.Command, _ []string) error {
			recordChangedFlags(cmd, cfg)
			resolved, err := resolveRunnerConfig(*cfg)
			if err != nil {
				return err
			}
			if err := verifyRunner(cmd.Context(), resolved); err != nil {
				return err
			}
			fmt.Fprintf(opts.out, "runner verified: %s\n", resolved.runnerID)
			return nil
		},
	}
	bindRunnerFlags(cmd, cfg)
	return cmd
}

func verifyRunner(ctx context.Context, cfg runnerConfig) error {
	client := protocol.NewClient(cfg.serverURL, http.DefaultClient)
	if _, err := client.Register(ctx, protocol.RegisterRunnerRequest{
		RunnerID:     cfg.runnerID,
		Concurrency:  cfg.concurrency,
		Capabilities: cfg.capabilities,
	}); err != nil {
		return err
	}
	_, err := client.Heartbeat(ctx, protocol.HeartbeatRequest{
		RunnerID:  cfg.runnerID,
		Capacity:  cfg.concurrency,
		InFlight:  0,
		Timestamp: time.Now().Unix(),
	})
	return err
}
```

- [ ] **Step 7: Register commands in root**

In `cmd/runner/command.go`, add the new commands:

```go
root.AddCommand(newRunCommand(opts, &cfg))
root.AddCommand(newVerifyCommand(opts, &cfg))
root.AddCommand(newConfigCommand(opts, &cfg))
```

- [ ] **Step 8: Run tests**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run 'TestConfigSamplePrintsYAML|TestConfigValidateRejectsInvalidFlagConfig|TestVerifyCommandRegistersAndHeartbeats'
```

Expected: PASS.

- [ ] **Step 9: Commit**

Run:

```bash
git add cmd/runner/command.go cmd/runner/config.go cmd/runner/config_command.go cmd/runner/verify.go cmd/runner/main_test.go cmd/runner/verify_test.go
git commit -m "feat(runner): add config and verify commands"
```

---

### Task 4: Full Runner CLI Verification and Process Smoke

**Files:**
- Modify: `cmd/runner/run.go`
- Modify: `cmd/runner/config.go`
- Modify: `cmd/runner/main_test.go`

**Interfaces:**
- Consumes: all command/config functions from Tasks 1-3.
- Produces: final CLI behavior that passes unit tests, full repo tests, and real server-runner smoke.

- [ ] **Step 1: Add final compatibility tests for env and flag precedence**

Append to `cmd/runner/main_test.go`:

```go
func TestCLIFlagsOverrideEnvironment(t *testing.T) {
	t.Setenv("XFLOW_RUNNER_SERVER", "http://env-server:8080")
	t.Setenv("XFLOW_RUNNER_ID", "env-runner")
	t.Setenv("XFLOW_RUNNER_CONCURRENCY", "5")
	t.Setenv("XFLOW_RUNNER_CAP", "xflow.http")

	var ran runnerConfig
	cmd := newRootCommand(commandOptions{
		runFunc: func(cfg runnerConfig) error {
			ran = cfg
			return nil
		},
		out: &bytes.Buffer{},
		err: &bytes.Buffer{},
	})
	cmd.SetArgs([]string{
		"run",
		"--server", "http://flag-server:8080",
		"--id", "flag-runner",
		"--concurrency", "2",
		"--cap", "xflow.function",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if ran.serverURL != "http://flag-server:8080" || ran.runnerID != "flag-runner" || ran.concurrency != 2 {
		t.Fatalf("config = %+v", ran)
	}
	if len(ran.capabilities) != 1 || ran.capabilities[0].NodeType != "xflow.function" {
		t.Fatalf("capabilities = %+v", ran.capabilities)
	}
}
```

- [ ] **Step 2: Run test to verify it fails if precedence is wrong**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -run TestCLIFlagsOverrideEnvironment
```

Expected: PASS. This proves the `recordChangedFlags` calls from Tasks 1 and 3 are wired into `resolveRunnerConfig`.

- [ ] **Step 3: Run all runner command tests**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner -count=1 -v
```

Expected: PASS.

- [ ] **Step 4: Run focused server-runner package tests**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache go test ./cmd/runner ./service/runner ./service/protocol ./service/control -count=1
```

Expected: PASS.

- [ ] **Step 5: Run full repo test**

Run:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache make test
```

Expected: PASS. This runs `go test ./... -race -count=1 -timeout 120s`.

- [ ] **Step 6: Run real process smoke with existing Podman Redis**

Use the existing Redis container if it is running:

```bash
podman ps --format '{{.Names}} {{.Ports}}' | rg 'xflow-test-redis'
```

Start a server on a free port:

```bash
SERVER_PORT="$(python3 - <<'PY'
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
print(s.getsockname()[1])
s.close()
PY
)"
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache \
go run ./cmd/server -addr "127.0.0.1:${SERVER_PORT}" -redis 127.0.0.1:35327
```

In another shell or background session, run the cobra runner:

```bash
GOCACHE=$PWD/.tmp/gocache GOMODCACHE=$PWD/.tmp/gomodcache \
go run ./cmd/runner run \
  --server "http://127.0.0.1:${SERVER_PORT}" \
  --id runner-smoke \
  --concurrency 1 \
  --cap xflow.function
```

Submit a one-node workflow through the server API and inspect until the execution
status is `success` with output `{"result":"TCK-3-runner"}`. Stop both processes
with SIGINT after the smoke passes.

- [ ] **Step 7: Commit**

Run:

```bash
git add cmd/runner/run.go cmd/runner/config.go cmd/runner/main_test.go
git commit -m "test(runner): verify cobra cli behavior"
```

---

## Self-Review

- Spec coverage: `run`, `verify`, `config validate`, `config sample`, YAML config, env overrides, CLI precedence, validation rules, and full test/smoke verification are covered.
- Deferred scope: durable `register`, `list`, `unregister`, service lifecycle commands, hot reload, drain mode, and multi-runner config are explicitly excluded.
- Type consistency: all planned functions use `runnerConfig`, `commandOptions`, `protocol.Capability`, and cobra command constructors defined in this plan.
- Dependency boundary: all cobra/YAML changes remain in `cmd/runner`; no core package imports `cmd` or `service`.
