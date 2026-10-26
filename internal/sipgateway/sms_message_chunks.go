package sipgateway

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/emiago/sipgo/sip"
)

const (
	mtMessageUTF16Limit = 16384
	mtMessageUTF8Limit  = 49152
	mtMessageChunkBytes = 280
)

// MT admission matches the desktop's UTF-16 length budget, not Go byte length.
// The early byte cap bounds validation/allocation even for rejected input. An
// astral scalar consumes two UTF-16 units; malformed UTF-8 is never replaced.
// Existing logical queue/recipient/4-MiB ledger limits remain unchanged.
func validMTMessageText(text string) bool {
	if len(text) == 0 || len(text) > mtMessageUTF8Limit || !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
		return false
	}
	units := 0
	for _, scalar := range text {
		units++
		if scalar > 0xffff {
			units++
		}
		if units > mtMessageUTF16Limit {
			return false
		}
	}
	return true
}

// A chunk contains at most 280 bytes of original UTF-8 content, exactly as the
// desktop splitter does. Human-readable numbering is additional framing and
// remains subject to the WHOLE 1300-byte packet cap in the existing MT wire.
// One-part text is unchanged, including any literal leading "[i/n]" text.
func splitMTMessageText(text string) ([]string, bool) {
	if !validMTMessageText(text) {
		return nil, false
	}
	chunks := make([]string, 0, (len(text)+mtMessageChunkBytes-1)/mtMessageChunkBytes)
	start, size := 0, 0
	for index, scalar := range text {
		width := utf8.RuneLen(scalar)
		if size+width > mtMessageChunkBytes {
			chunks = append(chunks, text[start:index])
			start, size = index, 0
		}
		size += width
	}
	chunks = append(chunks, text[start:])
	if len(chunks) > 1 {
		for index := range chunks {
			chunks[index] = fmt.Sprintf("[%d/%d] %s", index+1, len(chunks), chunks[index])
		}
	}
	return chunks, true
}

// This is one contact's COMPLETE logical result. Never aggregate individual
// chunks from different contacts into a falsely complete copy. All chunks use
// the original claim/source guards and caller deadline; deliverMessage gives
// each chunk a fresh transaction and at most one peer authentication retry.
func (r *runtime) deliverMTMessageParts(ctx context.Context, incoming *sip.Request, sender, receiver Account, entry deliveryTarget, text string, guards ...func() bool) messageOutcome {
	chunks, valid := splitMTMessageText(text)
	if !valid {
		return messageOutcome{513}
	}
	accepted, completeCode := false, 200
	for _, chunk := range chunks {
		outcome := r.deliverMessage(ctx, incoming, sender, receiver, entry, []byte(chunk), guards...)
		if outcome.code != 200 && outcome.code != 202 {
			if accepted {
				// A previous 2xx means a partial copy already exists. No later
				// rejection, packet-size failure or veto can turn it into a
				// definite whole-message failure or a replayable Queued record.
				return messageOutcome{504}
			}
			// Preserve definite first-part failures. This intentionally differs
			// from the desktop's more conservative Unknown for every MT failure.
			return outcome
		}
		accepted = true
		if outcome.code == 202 {
			completeCode = 202
		}
	}
	return messageOutcome{completeCode}
}
