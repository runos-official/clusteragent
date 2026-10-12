package agentstream

import (
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/runos-official/clusteragent/agentstream/instructions"
	"github.com/runos-official/clusteragent/agentstream/l2sec"
	"github.com/runos-official/clusteragent/commons"
	"google.golang.org/grpc"
)

func b64(t *testing.T, v any) string {
	t.Helper()
	s, err := commons.JsonB64Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestProgressTaggerTags(t *testing.T) {
	tag := uuid.New().String()

	on := newProgressTagger(tag, b64(t, map[string]any{"progressTags": true, "other": "x"}))
	for _, want := range []string{tag + "/1", tag + "/2", tag + "/3"} {
		if got := on.nextTag(); got != want {
			t.Fatalf("nextTag() = %q, want %q", got, want)
		}
	}

	for name, payload := range map[string]string{
		"field absent":     b64(t, map[string]any{"other": "x"}),
		"field false":      b64(t, map[string]any{"progressTags": false}),
		"field not a bool": b64(t, map[string]any{"progressTags": "true"}),
		"payload an array": b64(t, []int{1}),
		"payload empty":    "",
		"payload not b64":  "%%%",
	} {
		off := newProgressTagger(tag, payload)
		if got := off.nextTag(); got != "" {
			t.Errorf("%s: nextTag() = %q, want empty (today's fresh tag)", name, got)
		}
	}
}

// fakeStream captures what the agent sends on the control stream.
type fakeStream struct {
	grpc.ClientStream
	mu   sync.Mutex
	sent []*l2sec.FromClusterAgent
}

func (f *fakeStream) Send(m *l2sec.FromClusterAgent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeStream) Recv() (*l2sec.ToClusterAgent, error) { select {} }

func withFakeStream(t *testing.T) *fakeStream {
	t.Helper()
	f := &fakeStream{}
	globalStreamMu.Lock()
	prev := globalStream
	globalStream = f
	globalStreamMu.Unlock()
	t.Cleanup(func() {
		globalStreamMu.Lock()
		globalStream = prev
		globalStreamMu.Unlock()
	})
	return f
}

// TestStreamingInstructionProgressTags runs a streaming instruction through
// handleInstruction and checks the tags on what the agent sends.
func TestStreamingInstructionProgressTags(t *testing.T) {
	const testType = "TEST_STREAMING_INSTRUCTION"
	instructions.StreamingMap[testType] = func(_ string, progress instructions.ProgressFunc) (string, string, error) {
		for i := 0; i < 3; i++ {
			if err := progress("TestProgressLog", "bGluZQ=="); err != nil {
				return "", "", err
			}
		}
		return "DONE", "", nil
	}
	t.Cleanup(func() { delete(instructions.StreamingMap, testType) })

	t.Run("progressTags true numbers each message", func(t *testing.T) {
		f := withFakeStream(t)
		tag := uuid.New().String()
		handleInstruction(&l2sec.ToClusterAgent{Type: testType, Tag: tag, JsonB64: b64(t, map[string]any{"progressTags": true})})

		if len(f.sent) != 4 {
			t.Fatalf("sent %d messages, want 3 progress and 1 reply", len(f.sent))
		}
		for i, m := range f.sent[:3] {
			want := tag + "/" + string(rune('1'+i))
			if m.Tag != want || m.Type != "TestProgressLog" {
				t.Errorf("progress %d = %s %q, want TestProgressLog %q", i+1, m.Type, m.Tag, want)
			}
		}
		if reply := f.sent[3]; reply.Tag != tag || reply.Type != "DONE" {
			t.Errorf("reply = %s %q, want DONE %q", reply.Type, reply.Tag, tag)
		}
	})

	t.Run("without the field each message gets a fresh tag as today", func(t *testing.T) {
		f := withFakeStream(t)
		tag := uuid.New().String()
		handleInstruction(&l2sec.ToClusterAgent{Type: testType, Tag: tag, JsonB64: b64(t, map[string]any{})})

		if len(f.sent) != 4 {
			t.Fatalf("sent %d messages, want 3 progress and 1 reply", len(f.sent))
		}
		for i, m := range f.sent[:3] {
			if strings.Contains(m.Tag, "/") || m.Tag == tag {
				t.Errorf("progress %d tag = %q, want a fresh UUID", i+1, m.Tag)
			}
			if _, err := uuid.Parse(m.Tag); err != nil {
				t.Errorf("progress %d tag = %q is not a UUID: %v", i+1, m.Tag, err)
			}
		}
		if reply := f.sent[3]; reply.Tag != tag {
			t.Errorf("reply tag = %q, want %q", reply.Tag, tag)
		}
	})
}
