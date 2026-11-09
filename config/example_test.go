package config_test

// The runnable examples of the config spec's normative sections
// (docs/specs/config.md). Each loads only what it names: no .env file and no
// process environment.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/credo-go/credo/config"
)

// hermetic are the options that keep an example's load to its own document.
var hermetic = []config.Option{config.WithoutDotenv(), config.WithoutProcessEnv()}

type AppConfig struct {
	Name   string
	Port   int
	APIKey string // auto → "api_key"
	Region string `credo:"aws_region"`
}

var AppSection = config.Section[AppConfig]("app")

// A section's key and type are named once, by the descriptor; Decode reads
// it from any RawConfig.
func ExampleSection_Decode() {
	cfg, err := config.LoadBytes([]byte(`
app:
  name: checkout
  port: 8080
  api_key: k-1
  aws_region: eu-west-1
`), config.FormatYAML, hermetic...)
	if err != nil {
		panic(err)
	}

	app, err := AppSection.Decode(cfg)
	fmt.Printf("%+v %v\n", app, err)

	_, err = config.Section[AppConfig]("missing").Decode(cfg)
	fmt.Println(err)
	// Output:
	// {Name:checkout Port:8080 APIKey:k-1 Region:eu-west-1} <nil>
	// config: section "missing": config: key "missing" not found
}

// Decoding is strict by default: an unknown key and a conversion across kinds
// fail with the key path and the opt-in; a string parses as its target type.
func ExampleWithWeakDecoding() {
	doc := []byte(`{"app": {"name": "checkout", "port": "9090", "prot": 1}}`)

	strict, err := config.LoadBytes(doc, config.FormatJSON, hermetic...)
	if err != nil {
		panic(err)
	}
	_, err = AppSection.Decode(strict)
	fmt.Println(err)

	weak, err := config.LoadBytes(doc, config.FormatJSON, append(hermetic, config.WithWeakDecoding())...)
	if err != nil {
		panic(err)
	}
	app, err := AppSection.Decode(weak)
	fmt.Println(app.Port, err)
	// Output:
	// config: section "app": config: unknown key "app.prot": no field of the target decodes it; fix the configuration, or opt in to weak coercion with config.WithWeakDecoding()
	// 9090 <nil>
}

type DatabaseConfig struct {
	Host         string
	Port         int
	MaxOpenConns int
}

func DefaultDatabaseConfig() DatabaseConfig {
	return DatabaseConfig{Host: "localhost", Port: 5432, MaxOpenConns: 25}
}

func (c *DatabaseConfig) Validate() error {
	if c.Host == "" {
		return errors.New("databases.default.host is required")
	}
	return nil
}

// Unmarshal sets only the keys a source provides, so a pre-initialized value
// keeps its defaults; an explicit zero overrides one. Validate runs after the
// value is populated.
func ExampleConfig_Unmarshal() {
	rc, err := config.LoadBytes([]byte(`{"databases": {"default": {"host": "db", "max_open_conns": 0}}}`),
		config.FormatJSON, hermetic...)
	if err != nil {
		panic(err)
	}

	dbCfg := DefaultDatabaseConfig()
	err = rc.Unmarshal("databases.default", &dbCfg)
	fmt.Printf("%+v %v\n", dbCfg, err)

	rc, err = config.LoadBytes([]byte(`{"databases": {"default": {"host": ""}}}`), config.FormatJSON, hermetic...)
	if err != nil {
		panic(err)
	}
	dbCfg = DefaultDatabaseConfig()
	fmt.Println(rc.Unmarshal("databases.default", &dbCfg))
	// Output:
	// {Host:db Port:5432 MaxOpenConns:0} <nil>
	// config: validation: databases.default.host is required
}

type LimitsConfig struct {
	RPS   int
	Burst int
}

// A live section is replaced whole by a reload that changes it, after it
// validated; its OnChange callbacks run after the new value is published.
// Every other change is reported as needing a restart.
func ExampleSection_Live() {
	dir, err := os.MkdirTemp("", "credo-config-example")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "config.json")
	write := func(doc string) {
		if werr := os.WriteFile(file, []byte(doc), 0o600); werr != nil {
			panic(werr)
		}
	}

	write(`{"app": {"name": "checkout"}, "limits": {"rps": 10, "burst": 20}}`)
	cfg, err := config.Load(append(hermetic, config.WithFiles(file))...)
	if err != nil {
		panic(err)
	}
	var Limits = config.Section[LimitsConfig]("limits")
	limits, err := Limits.Live(cfg)
	if err != nil {
		panic(err)
	}
	limits.OnChange(func(ctx context.Context, prev, next LimitsConfig) error {
		fmt.Println("changed:", prev.RPS, "→", next.RPS)
		return nil
	})
	fmt.Println(limits.Load().RPS)

	write(`{"app": {"name": "billing"}, "limits": {"rps": 50, "burst": 100}}`)
	res, err := cfg.Reload(context.Background())
	if err != nil {
		panic(err)
	}
	fmt.Println(limits.Load().RPS)
	out, _ := json.Marshal(res)
	fmt.Println(string(out))
	// Output:
	// 10
	// changed: 10 → 50
	// 50
	// {"published":true,"changed":["app.name","limits.burst","limits.rps"],"replaced":["limits"],"restart_required":["app.name"],"overridden":[]}
}

// Fixed is a handle whose value never changes, for a consumer's unit test.
func ExampleFixed() {
	limits := config.Fixed(LimitsConfig{RPS: 10, Burst: 20})
	fmt.Printf("%+v\n", limits.Load())
	// Output:
	// {RPS:10 Burst:20}
}

// Formatting a Config never shows its values or its keys.
func ExampleConfig_String() {
	cfg, err := config.LoadBytes([]byte(`{"db": {"password": "s3cret"}}`), config.FormatJSON, hermetic...)
	if err != nil {
		panic(err)
	}
	fmt.Println(cfg)
	fmt.Printf("%#v\n", *cfg)
	// Output:
	// config.Config(1 keys, values redacted)
	// config.Config(1 keys, values redacted)
}
