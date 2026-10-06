package appconf

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// project makes a directory tree that looks like the repo: a root with .env and
// dist, and a binary dir underneath it.
func project(t *testing.T, env string) (root, bin string) {
	t.Helper()
	root = t.TempDir()
	bin = filepath.Join(root, "server-go", "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if env != "" {
		if err := os.WriteFile(filepath.Join(root, ".env"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, bin
}

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BACKEND", "UPSTREAM_URL", "VLLM_UPSTREAM", "PORT", "HOST", "UI_DIST",
		"UPSTREAM_API_KEY", "VLLM_API_KEY", "OPENAI_API_KEY", "VLLM_MODEL",
		"VLLM_N_CTX", "VLLM_MODALITY_VISION", "VLLM_ENGINE_TIMINGS",
		"VLLM_KEEP_UNSUPPORTED", "VLLM_PROBE_VISION", "VLLM_PROBE_THINKING",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

func TestDefaults(t *testing.T) {
	clearEnv(t)
	root, bin := project(t, "")
	cfg, err := load(nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "auto" || cfg.Port != 8080 || cfg.Host != "0.0.0.0" {
		t.Errorf("cfg = %+v", cfg)
	}
	if cfg.Upstream != "http://localhost:8000" {
		t.Errorf("upstream = %q", cfg.Upstream)
	}
	if !cfg.EngineTimings || cfg.KeepUnsupported || !cfg.ProbeVision || !cfg.ProbeThinking {
		t.Errorf("switches = %+v", cfg)
	}
	if cfg.Vision != "auto" {
		t.Errorf("vision = %q", cfg.Vision)
	}
	// The dist default sits in the project root, above the binary dir.
	if filepath.Clean(cfg.Dist) != filepath.Clean(filepath.Join(root, "dist")) {
		t.Errorf("dist = %q, want %q", cfg.Dist, filepath.Join(root, "dist"))
	}
	if cfg.UsingDotEnv {
		t.Error("no .env file here, nothing to report in the boot log")
	}
}

func TestDotEnvFile(t *testing.T) {
	clearEnv(t)
	root, bin := project(t, strings.Join([]string{
		"# a comment line",
		"BACKEND=Strata",
		"UPSTREAM_URL=http://engine.invalid:9000///",
		`VLLM_MODEL="quoted model"`,
		"VLLM_N_CTX=4096",
		"VLLM_MODALITY_VISION=1",
		"VLLM_ENGINE_TIMINGS=0",
		"VLLM_KEEP_UNSUPPORTED=1",
		"VLLM_PROBE_THINKING=0",
		`PORT='9999'`,
		"  HOST = 127.0.0.1 ",
		"NO_VALUE=",
		"not a pair",
	}, "\n"))
	cfg, err := load(nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "strata" {
		t.Errorf("backend = %q, want the value lower-cased", cfg.Backend)
	}
	if cfg.Upstream != "http://engine.invalid:9000" {
		t.Errorf("upstream = %q, want trailing slashes gone", cfg.Upstream)
	}
	if cfg.Model != "quoted model" {
		t.Errorf("model = %q, want the quotes stripped", cfg.Model)
	}
	if cfg.NCtx != 4096 || cfg.Port != 9999 || cfg.Host != "127.0.0.1" {
		t.Errorf("numbers and host = %+v", cfg)
	}
	if forced, on := cfg.VisionForced(); !forced || !on {
		t.Error("VLLM_MODALITY_VISION=1 forces vision on")
	}
	if cfg.EngineTimings || !cfg.KeepUnsupported || cfg.ProbeThinking {
		t.Errorf("switches = %+v", cfg)
	}
	if !cfg.UsingDotEnv {
		t.Error("the boot log says the .env was read")
	}
	if os.Getenv("HOST") != "127.0.0.1" {
		t.Error("the file feeds the environment other code reads")
	}
	_ = root
}

// TestEnvironmentWins: .env holds the machine's secrets, but a shell variable set
// on purpose beats it.
func TestEnvironmentWins(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND", "vllm")
	_, bin := project(t, "BACKEND=strata\n")
	cfg, err := load(nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Backend != "vllm" {
		t.Errorf("backend = %q, want the environment to win", cfg.Backend)
	}
}

func TestLegacyNames(t *testing.T) {
	clearEnv(t)
	t.Setenv("VLLM_UPSTREAM", "http://old.host:1/")
	t.Setenv("VLLM_API_KEY", "k1")
	_, bin := project(t, "")
	cfg, err := load(nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "http://old.host:1" || cfg.APIKey != "k1" {
		t.Errorf("cfg = %+v", cfg)
	}

	clearEnv(t)
	t.Setenv("UPSTREAM_URL", "http://new.host:2")
	t.Setenv("VLLM_UPSTREAM", "http://old.host:1")
	t.Setenv("OPENAI_API_KEY", "k3")
	cfg, err = load(nil, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream != "http://new.host:2" {
		t.Errorf("the new name must win, got %q", cfg.Upstream)
	}
	if cfg.APIKey != "k3" {
		t.Errorf("OPENAI_API_KEY is the last alias, got %q", cfg.APIKey)
	}
}

func TestFlagsBeatEnvironment(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "7000")
	t.Setenv("BACKEND", "auto")
	_, bin := project(t, "")
	cfg, err := load([]string{"-port", "8123", "-upstream", "http://flagged:1", "-backend", "STRATA", "-vision", "0"}, bin)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 8123 || cfg.Upstream != "http://flagged:1" || cfg.Backend != "strata" {
		t.Errorf("cfg = %+v", cfg)
	}
	if forced, on := cfg.VisionForced(); !forced || on {
		t.Errorf("-vision 0 forces vision off, got forced=%v on=%v", forced, on)
	}
}

// TestUnknownBackend: the message and the exit are the contract an operator
// reads when they typed BACKEND wrong.
func TestUnknownBackend(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND", "x")
	_, bin := project(t, "")
	_, err := load(nil, bin)
	if err == nil {
		t.Fatal("an unknown BACKEND must stop the adapter")
	}
	if err.Error() != `BACKEND must be auto, vllm, strata. Got "x".` {
		t.Errorf("message = %q", err.Error())
	}
}

func TestUpstreamHost(t *testing.T) {
	c := &Config{Upstream: "http://vllm.internal:8000/v1"}
	if got := c.UpstreamHost(); got != "vllm.internal:8000" {
		t.Errorf("host = %q", got)
	}
}
