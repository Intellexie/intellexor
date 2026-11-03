package server

import (
	"os"
	"testing"

	"github.com/Intellexie/intellexor/internal/config"
)

// TestMain isolates the suite from the machine's own global config files
// (~/.config/intellexor, /app/*.json) so agent/provider listings are
// deterministic.
func TestMain(m *testing.M) {
	config.SetGlobalCandidatesForTest([]string{})
	code := m.Run()
	config.SetGlobalCandidatesForTest(nil)
	os.Exit(code)
}
