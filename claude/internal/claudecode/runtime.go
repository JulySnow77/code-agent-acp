package claudecode

import (
	"context"
)

// Runtime implements the bridge.Claude factory by launching real CLI child
// processes. It is intentionally thin so tests can substitute a fake factory.
type Runtime struct{}

// Open starts one Claude CLI process for one ACP session.
func (Runtime) Open(ctx context.Context, cfg Config) (Session, error) {
	return Launch(ctx, cfg)
}
