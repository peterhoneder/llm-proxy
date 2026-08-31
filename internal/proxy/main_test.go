package proxy

import (
	"os"
	"testing"
)

// See the note in internal/config: the suite must not inherit the operator's
// LLM_PROXY_TOKENS, or every test would get a 401 from its own proxy.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("LLM_PROXY_TOKENS")
	os.Exit(m.Run())
}
