package config

import (
	"os"
	"testing"
)

// The suite must not depend on the developer's shell. LLM_PROXY_TOKENS is the
// documented zero-config way to switch auth on, so anyone who actually uses
// llm-proxy is likely to have it exported — and without this every test that
// assumes an unguarded proxy would fail on their machine and pass in CI.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("LLM_PROXY_TOKENS")
	os.Exit(m.Run())
}
