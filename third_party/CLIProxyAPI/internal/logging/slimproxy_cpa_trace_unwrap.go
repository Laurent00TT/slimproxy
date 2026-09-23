// slimproxy patch: the CPA trace writer unwraps for http.ResponseController
// (SLIMPROXY_PATCHES.md 第 17 条).
//
// CPATraceIDMiddleware installs this writer ahead of every embedder
// middleware, so it sits between the embedder's writers and net/http. It
// embeds gin.ResponseWriter, whose interface has no Unwrap, so a
// ResponseController walking down from an embedder's writer stopped here with
// ErrNotSupported. slimproxy's early-flush middleware needs EnableFullDuplex
// to reach net/http: without it, an SSE preamble written while the request
// body is still uploading makes net/http discard the unread rest of the body
// (under 256KB left) or force Connection: close -- 20 requests killed that
// way on 2026-09-23, each after 80-124s of uploading.
//
// In its own file so an upstream rebase does not conflict with it; if
// upstream adds its own Unwrap, the build fails on the duplicate method and
// this file is deleted.
package logging

import "net/http"

// Unwrap returns the writer underneath. Write-side controller calls still
// stop at this writer, because it implements Flush and the promoted Hijack;
// only calls it does not implement -- full duplex, deadlines -- pass through.
func (w *cpaTraceResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
