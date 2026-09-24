package executor

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// A stream can return without a usage event when its consumer disconnects or
// the upstream completes without token counts. Keep those requests in the
// journal; UsageReporter's once guard preserves any previously published result.
func slimproxyFinishCodexStream(ctx context.Context, reporter *helps.UsageReporter, completed bool) {
	if completed {
		reporter.EnsurePublished(ctx)
	} else if err := ctx.Err(); err != nil {
		reporter.PublishFailure(ctx, err)
	}
}
