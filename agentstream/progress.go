package agentstream

import (
	"fmt"
	"sync/atomic"

	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
)

// progressTagger tags the progress and log messages of one instruction.
//
// When the instruction payload has "progressTags": true, each message carries
// the tag "<instruction tag>/<n>", n counting from 1, so nodehub can route it
// to the replica that sent the instruction (nodehub DESIGN.md sections 6 and
// 12). Without the field every message gets a fresh tag, as before FPL315.
// Nodeward sets the field only behind nodehub, so a direct nodeward never sees
// a tag it would read as a reply.
type progressTagger struct {
	instructionTag string
	enabled        bool
	n              atomic.Uint64
}

func newProgressTagger(instructionTag, payloadJsonB64 string) *progressTagger {
	var payload struct {
		ProgressTags bool `json:"progressTags"`
	}
	// A payload that does not decode as a JSON object means "no progress tags".
	enabled := commons.JsonB64Decode(payloadJsonB64, &payload) == nil && payload.ProgressTags
	return &progressTagger{instructionTag: instructionTag, enabled: enabled}
}

// nextTag returns the tag for the next message. An empty tag makes
// SendToServer pick a fresh UUID.
func (p *progressTagger) nextTag() string {
	if !p.enabled {
		return ""
	}
	return fmt.Sprintf("%s/%d", p.instructionTag, p.n.Add(1))
}

// send streams one progress or log message for the instruction.
func (p *progressTagger) send(msgType, jsonB64 string) error {
	return SendToServer(&l2sec.FromClusterAgent{Type: msgType, JsonB64: jsonB64, Tag: p.nextTag()})
}

// streamingHandler adapts a handler from instructions.StreamingMap to the
// plain handler shape, with a progress sender bound to this instruction.
func streamingHandler(instruction *l2sec.ToClusterAgent) (func(string) (string, string, error), bool) {
	h, ok := instructions.StreamingMap[instruction.Type]
	if !ok {
		return nil, false
	}
	tagger := newProgressTagger(instruction.Tag, instruction.JsonB64)
	return func(payload string) (string, string, error) {
		return h(payload, tagger.send)
	}, true
}
