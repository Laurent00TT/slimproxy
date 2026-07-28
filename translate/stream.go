package translate

import (
	"bufio"
	"context"
	"fmt"
	"io"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// defaultScanBuffer matches the 50 MB line buffer the CLIProxyAPI executors
// use. Provider frames carrying inline images or large tool arguments routinely
// exceed bufio.Scanner's 64 KB default, and the failure mode is a truncated
// stream rather than an error, so the ceiling is deliberately high.
const defaultScanBuffer = 50 * 1024 * 1024

// Stream converts provider response lines into client frames, carrying the
// per-request state that stateful transforms need across chunks.
//
// State ownership is the point of this type. The underlying registry takes a
// caller-supplied *any and type-asserts it without comma-ok, so passing one
// *any to two different pairs panics inside the transform. A Stream is bound to
// exactly one Translator at construction and owns its state privately, which
// makes that misuse unrepresentable.
//
// A Stream is single-request and NOT safe for concurrent use. Create one per
// upstream response.
type Stream struct {
	t     *Translator
	model string

	originalRequest   []byte
	translatedRequest []byte

	// state is the per-request scratch space the transforms cast to their own
	// params struct. It must never be shared with another pair.
	state any

	done bool
}

// NewStream begins a streaming response conversion.
//
// originalRequest is the client's pre-translation body and translatedRequest is
// the post-translation upstream body. Both are re-read by several transforms on
// nearly every chunk (to rebuild tool-name maps, for instance), so they must
// remain valid for the life of the Stream.
func (t *Translator) NewStream(model string, originalRequest, translatedRequest []byte) (*Stream, error) {
	if !t.caps.StreamResponse && !t.opts.AllowResponsePassthrough {
		return nil, fmt.Errorf("%w: %s (stream)", ErrNoTransformer, t.pair)
	}
	return &Stream{
		t:                 t,
		model:             model,
		originalRequest:   originalRequest,
		translatedRequest: translatedRequest,
	}, nil
}

// Line converts one RAW LINE of the upstream response into zero or more client
// frames.
//
// This is per line, not per SSE event: the transforms expect to see the raw
// bytes between newlines and recover the event name from the payload's "type"
// field rather than from a preceding "event:" line. Feeding whole SSE events,
// or pre-stripping the "data: " prefix, breaks transforms that check for it.
//
// Returning zero frames is normal and means "consumed, nothing to emit yet".
func (s *Stream) Line(ctx context.Context, line []byte) [][]byte {
	if s.done {
		return nil
	}
	if !s.t.caps.StreamResponse {
		// Passthrough was opted into at construction.
		return [][]byte{line}
	}
	// Response lookups take (provider, client) -- flipped relative to registration.
	return sdktranslator.TranslateStream(ctx,
		s.t.pair.Provider.sdk(), s.t.pair.Client.sdk(),
		s.model, s.originalRequest, s.translatedRequest, line, &s.state)
}

// Finish marks the stream complete and releases the transform state.
//
// It does NOT synthesize a terminal sentinel. Whether the client dialect needs
// one, and what it looks like, is a transport concern this package does not
// own: the OpenAI chat dialect expects "data: [DONE]", OpenAI Responses expects
// a bare newline, and Claude and Gemini expect nothing at all. Emit it in the
// layer that owns the HTTP response.
func (s *Stream) Finish() {
	s.done = true
	s.state = nil
}

// ReadFrom drives an entire upstream response body through Line, invoking emit
// for each produced frame. It applies the 50 MB line buffer the executors use.
//
// emit returning an error aborts the scan and that error is returned. A scan
// error is returned as-is; note that a truncated upstream stream surfaces here
// rather than as a translated error frame, because error shaping is
// dialect-specific and belongs to the caller.
func (s *Stream) ReadFrom(ctx context.Context, r io.Reader, emit func(frame []byte) error) error {
	defer s.Finish()

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), defaultScanBuffer)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, frame := range s.Line(ctx, scanner.Bytes()) {
			if err := emit(frame); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
