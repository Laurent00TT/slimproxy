package forkcheck

import "testing"

// Codex compatibility must be exercised by the same command as the Claude
// patches. These existing executor tests use local fake upstreams; passing
// them does not establish account access or public-tunnel compatibility.
// Explicit names also make a removed test fail rather than quietly shrinking
// the baseline when the fork is upgraded.
func TestForkCodexCompatibilityBaseline(t *testing.T) {
	runForkGuard(t, "Codex compatibility baseline",
		[]string{"./internal/runtime/executor/"},
		[]string{
			"TestCodexExecutorExecuteStreamNormalizesNullInstructions",
			"TestCodexExecutorExecuteStreamSanitizesOverlongInputItemIDs",
			"TestNormalizeCodexParallelToolCallsForTools_PreservesWhenToolsPresent",
			"TestCodexExecutorExecuteStreamResponsesLiteHeaderForcesParallelToolCallsFalse",
			"TestCodexExecutorCompactAddsDefaultInstructionsWithoutInjectingImageTool",
			"TestSlimproxyCodexCompactFailureCooldown",
			"TestSlimproxyCodexStreamUsageOutcomes",
			"TestCodexExecutorExecuteStreamMissingCompletionIsRequestScoped",
			"TestCodexExecutorExecuteStreamExplicitTerminalFailureIsNotSuccessful",
			"TestCodexExecutorExecuteStreamIgnoresTransportErrorAfterCompletion",
			"TestCodexExecutorExecuteStream_EmptyStreamCompletionOutputUsesOutputItemDone",
			"TestCodexTerminalStreamErrHandlesUsageLimitResponseFailed",
			"TestCodexWebsocketsExecutePreservesPreviousResponseIDUpstream",
			"TestCodexAutoExecutorRequiredUpstreamWebsocketRejectsHTTPFallback",
			"TestCodexWebsocketsExecuteStreamPassesThroughUpstreamWebsocketPayloadForDownstreamWebsocket",
			"TestCodexWebsocketsExecuteStreamPropagatesUpstreamErrorForDownstreamWebsocket",
			"TestCodexWebsocketHandshakeFailureReleasesSessionRequestLock",
			"TestCodexWebsocketTerminalFailureInvalidatesRetainedLifecycle",
		})
}
