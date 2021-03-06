package testutils

import (
	"fmt"
	"strings"

	assert "github.com/stretchr/testify/require"
	"go.skia.org/infra/go/exec"
)

// Run runs the given command in the given dir and asserts that it succeeds.
func Run(t assert.TestingT, dir string, cmd ...string) string {
	out, err := exec.RunCwd(dir, cmd...)
	assert.NoError(t, err, fmt.Sprintf("Command %q failed:\n%s", strings.Join(cmd, " "), out))
	return out
}
