package app

import (
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/config"
)

// TestMain isolates the suite from the machine's own global config files
// (~/.config/intellexor, /app/*.json): a global `agent.build.model` override
// would otherwise redirect test prompts away from the fake providers.
func TestMain(m *testing.M) {
	config.SetGlobalCandidatesForTest([]string{})
	code := m.Run()
	config.SetGlobalCandidatesForTest(nil)
	os.Exit(code)
}
