package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"

	"github.com/oxcom/agents-mcp-bridge/schema"
)

// maxConfigBytes bounds the parser's input. An unbounded YAML document is a
// denial-of-service primitive before any rule below gets to run.
const maxConfigBytes = 1 << 20 // 1 MiB

// Options carries what the loader cannot discover for itself.
type Options struct {
	// Host is the agent_id declared by --host. The adapter with this id is
	// dropped: an agent may never delegate to itself.
	Host string
	// SkipPermissionCheck disables the ownership and mode checks. Tests only;
	// never set from production code.
	SkipPermissionCheck bool
	// LookPath resolves an executable name. Injectable for tests.
	LookPath func(string) (string, error)
}

// Load reads, validates and resolves the configuration at path.
//
// Order matters: file permissions are checked before the bytes are parsed, the
// schema runs before the semantic rules, and executables are resolved last so a
// structurally invalid config never reaches the filesystem.
func Load(path string, opts Options) (*Config, error) {
	if opts.LookPath == nil {
		opts.LookPath = exec.LookPath
	}
	raw, err := readConfig(path, opts.SkipPermissionCheck)
	if err != nil {
		return nil, err
	}
	// Parse to a generic document first so the schema sees the same shape a
	// JSON validator expects, then decode into the typed struct.
	doc, err := decodeYAML(raw)
	if err != nil {
		return nil, err
	}
	if err := validateSchema(doc); err != nil {
		return nil, err
	}

	var cfg Config
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true) // an unknown key is a fault, not a silently ignored setting
	if err := dec.Decode(&cfg); err != nil {
		return nil, errf("", "decode: %v", err)
	}
	applyDefaults(&cfg)

	for id, a := range cfg.Agents {
		a.ID = id
	}
	if err := cfg.validateSemantics(opts); err != nil {
		return nil, err
	}
	cfg.dropHostAdapter(opts.Host)
	return &cfg, nil
}

// readConfig opens the file once and validates the descriptor it is about to
// read, so there is no check-then-open window. The parent directory is checked
// by path, which is unavoidable, but a hostile directory swap cannot change the
// bytes already being read through this descriptor.
func readConfig(path string, skipPermissionCheck bool) ([]byte, error) {
	if !skipPermissionCheck {
		if err := checkParentDir(path); err != nil {
			return nil, err
		}
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errf("", "open config: %v", err)
	}
	defer f.Close()

	if !skipPermissionCheck {
		if err := checkOpenFile(f); err != nil {
			return nil, err
		}
	}

	raw, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err != nil {
		return nil, errf("", "read config: %v", err)
	}
	if len(raw) > maxConfigBytes {
		return nil, errf("", "config exceeds %d bytes", maxConfigBytes)
	}
	return raw, nil
}

// decodeYAML rejects multi-document input and duplicate keys. yaml.v3 treats a
// duplicate mapping key as an error, which is what makes a shadowed security
// setting impossible.
func decodeYAML(raw []byte) (any, error) {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var first any
	if err := dec.Decode(&first); err != nil {
		return nil, errf("", "parse: %v", err)
	}
	var second any
	if err := dec.Decode(&second); err == nil {
		return nil, errf("", "multiple YAML documents: exactly one is allowed")
	} else if err != io.EOF {
		return nil, errf("", "parse: %v", err)
	}
	// Round-trip through JSON so the validator sees canonical types
	// (yaml.v3 already produces map[string]any for string keys).
	buf, err := json.Marshal(first)
	if err != nil {
		return nil, errf("", "normalise: %v", err)
	}
	var doc any
	if err := json.Unmarshal(buf, &doc); err != nil {
		return nil, errf("", "normalise: %v", err)
	}
	return doc, nil
}

func validateSchema(doc any) error {
	compiler := jsonschema.NewCompiler()
	var sch any
	if err := json.Unmarshal(schema.Config, &sch); err != nil {
		return errf("", "embedded schema is unreadable: %v", err)
	}
	if err := compiler.AddResource("config.schema.json", sch); err != nil {
		return errf("", "embedded schema is invalid: %v", err)
	}
	compiled, err := compiler.Compile("config.schema.json")
	if err != nil {
		return errf("", "embedded schema does not compile: %v", err)
	}
	if err := compiled.Validate(doc); err != nil {
		var ve *jsonschema.ValidationError
		if ok := asValidationError(err, &ve); ok {
			return errf(instancePath(ve), "%s", leafMessage(ve))
		}
		return errf("", "schema: %v", err)
	}
	return nil
}

var credentialPattern = regexp.MustCompile(
	`(?i)(API_?KEY|_TOKEN|^TOKEN$|SECRET|PASSWORD|CREDENTIAL|_KEY$|SESSION_KEY|AUTH)`)

func applyDefaults(c *Config) {
	d := &c.Defaults
	setInt(&d.TimeoutS, 900)
	setInt(&d.MaxOutputBytes, 32768)
	setInt(&d.MaxPromptBytes, 30720)
	setInt(&d.MaxEventBytes, 262144)
	setInt64(&d.MaxTranscriptBytes, 52428800)
	setInt(&d.MaxConcurrentRuns, 4)
	setInt(&d.MaxAgentSteers, 3)
	setInt(&d.MaxTurns, 40)
	setInt(&d.QuestionTimeoutS, 300)
	setInt(&d.RunRetentionS, 3600)
	setInt(&d.RatePerMinute, 20)
	if d.OnConcurrencyLimit == "" {
		d.OnConcurrencyLimit = "reject"
	}
	// MaxDepth and MaxContinuations are deliberately left alone: both are
	// pointers so that an explicit 0 (delegation disabled / no continuations)
	// survives, while absent falls back to their *OrDefault methods.

	for _, a := range c.Agents {
		if a.Worktree == "" {
			a.Worktree = WorktreeRequired
		}
		if a.Capabilities.Steer == "" {
			a.Capabilities.Steer = SteerNone
		}
	}
}

func setInt(p *int, def int) {
	if *p == 0 {
		*p = def
	}
}

func setInt64(p *int64, def int64) {
	if *p == 0 {
		*p = def
	}
}

// dropHostAdapter removes every adapter an agent could use to call itself.
//
// The primary key is agent_id == host. Executable identity is only a secondary
// check because npm installs are shims whose resolved path never matches the
// host's own process image, so it can ADD an exclusion but never remove one.
func (c *Config) dropHostAdapter(host string) {
	if host == "" {
		return
	}
	hostExe := ""
	if a, ok := c.Agents[host]; ok {
		hostExe = a.ResolvedCommand
		delete(c.Agents, host)
	}
	if hostExe == "" {
		return
	}
	for id, a := range c.Agents {
		if a.ResolvedCommand == hostExe {
			delete(c.Agents, id)
		}
	}
}

func asValidationError(err error, out **jsonschema.ValidationError) bool {
	return errors.As(err, out)
}

func instancePath(ve *jsonschema.ValidationError) string {
	leaf := leafError(ve)
	if len(leaf.InstanceLocation) == 0 {
		return ""
	}
	return strings.Join(leaf.InstanceLocation, ".")
}

// leafMessage renders the innermost schema failure in human terms. The raw
// ErrorKind has no useful String method, so the localised form is used.
func leafMessage(ve *jsonschema.ValidationError) string {
	leaf := leafError(ve)
	if leaf.ErrorKind == nil {
		return "does not satisfy the schema"
	}
	return leaf.ErrorKind.LocalizedString(message.NewPrinter(language.English))
}

func leafError(ve *jsonschema.ValidationError) *jsonschema.ValidationError {
	for len(ve.Causes) > 0 {
		ve = ve.Causes[0]
	}
	return ve
}
