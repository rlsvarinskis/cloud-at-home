package llm

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// waitForCalls polls until f has recorded at least n calls, failing the test after two seconds.
func waitForCalls(t *testing.T, f *Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.Calls)
		f.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d calls recorded, want %d", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFakeScriptedReplies(t *testing.T) {
	f := &Fake{Script: []FakeReply{
		Say("hi there"),
		CallTool("bash", map[string]any{"command": "ls"}),
	}}
	ctx := context.Background()

	m1, err := f.Chat(ctx, "m1", []Message{{Role: "user", Content: "hello"}}, nil, ChatOptions{}, nil)
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if m1.Role != "assistant" || m1.Content != "hi there" {
		t.Errorf("first reply = %+v", m1)
	}

	m2, err := f.Chat(ctx, "m1", nil, nil, ChatOptions{}, nil)
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	want := []ToolCall{{Name: "bash", Arguments: map[string]any{"command": "ls"}}}
	if !reflect.DeepEqual(m2.ToolCalls, want) {
		t.Errorf("second reply tool calls = %+v, want %+v", m2.ToolCalls, want)
	}
	if len(f.Script) != 0 {
		t.Errorf("script has %d replies left, want 0", len(f.Script))
	}
}

func TestFakeDeliversChunksInOrder(t *testing.T) {
	f := &Fake{Script: []FakeReply{{
		Message: Message{Role: "assistant", Content: "ab"},
		Chunks:  []Chunk{{Content: "a"}, {Thinking: "t"}, {Content: "b"}},
	}}}
	var got []Chunk
	msg, err := f.Chat(context.Background(), "m", nil, nil, ChatOptions{}, func(c Chunk) {
		if !c.Done {
			got = append(got, c)
		}
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	want := []Chunk{{Content: "a"}, {Thinking: "t"}, {Content: "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %+v, want %+v", got, want)
	}
	if msg.Content != "ab" {
		t.Errorf("Content = %q", msg.Content)
	}
}

func TestFakeNilOnChunkAccepted(t *testing.T) {
	f := &Fake{Script: []FakeReply{{Chunks: []Chunk{{Content: "x"}}, Message: Message{Content: "x"}}}}
	if _, err := f.Chat(context.Background(), "m", nil, nil, ChatOptions{}, nil); err != nil {
		t.Fatalf("Chat with nil onChunk: %v", err)
	}
}

func TestFakeExhausted(t *testing.T) {
	f := &Fake{Script: []FakeReply{Say("only one")}}
	ctx := context.Background()
	if _, err := f.Chat(ctx, "m", nil, nil, ChatOptions{}, nil); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := f.Chat(ctx, "m", nil, nil, ChatOptions{}, nil); err == nil {
		t.Fatal("second Chat on exhausted script: expected error")
	}
	if n := len(f.Calls); n != 2 {
		t.Errorf("recorded %d calls, want 2", n)
	}
}

func TestFakeRecordsCallsWithDeepCopies(t *testing.T) {
	f := &Fake{Script: []FakeReply{Say("ok")}}
	msgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "bash", Arguments: map[string]any{
			"command": "ls",
			"opts":    map[string]any{"n": 1},
			"list":    []any{"a"},
		}}}},
	}
	tools := []Tool{{Name: "bash", Description: "d", Parameters: map[string]any{"type": "object"}}}

	if _, err := f.Chat(context.Background(), "model-x", msgs, tools, ChatOptions{}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Mutate everything the caller passed in, including nested values.
	msgs[0].Content = "changed"
	msgs[1].ToolCalls[0].Arguments["command"] = "rm -rf"
	msgs[1].ToolCalls[0].Arguments["opts"].(map[string]any)["n"] = 99
	msgs[1].ToolCalls[0].Arguments["list"].([]any)[0] = "z"
	tools[0].Parameters["type"] = "array"

	if len(f.Calls) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(f.Calls))
	}
	call := f.Calls[0]
	if call.Model != "model-x" {
		t.Errorf("Model = %q", call.Model)
	}
	wantMsgs := []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", ToolCalls: []ToolCall{{Name: "bash", Arguments: map[string]any{
			"command": "ls",
			"opts":    map[string]any{"n": 1},
			"list":    []any{"a"},
		}}}},
	}
	if !reflect.DeepEqual(call.Messages, wantMsgs) {
		t.Errorf("recorded messages = %+v, want %+v", call.Messages, wantMsgs)
	}
	wantTools := []Tool{{Name: "bash", Description: "d", Parameters: map[string]any{"type": "object"}}}
	if !reflect.DeepEqual(call.Tools, wantTools) {
		t.Errorf("recorded tools = %+v, want %+v", call.Tools, wantTools)
	}
}

func TestFakeErrReply(t *testing.T) {
	boom := errors.New("boom")
	f := &Fake{Script: []FakeReply{{
		Chunks: []Chunk{{Content: "partial"}},
		Err:    boom,
	}}}
	var got []Chunk
	_, err := f.Chat(context.Background(), "m", nil, nil, ChatOptions{}, func(c Chunk) {
		if !c.Done {
			got = append(got, c)
		}
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want boom", err)
	}
	if len(got) != 1 || got[0].Content != "partial" {
		t.Errorf("chunks delivered before error = %+v", got)
	}
}

func TestFakeBlockCancelled(t *testing.T) {
	block := make(chan struct{}) // never closed
	defer close(block)
	f := &Fake{Script: []FakeReply{{Message: Message{Content: "never"}, Block: block}}}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := f.Chat(ctx, "m", nil, nil, ChatOptions{}, nil)
		errCh <- err
	}()
	waitForCalls(t, f, 1) // the call is now blocked inside Chat

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked Chat did not return after cancel")
	}
}

func TestFakeBlockReleased(t *testing.T) {
	block := make(chan struct{})
	f := &Fake{Script: []FakeReply{{Message: Message{Role: "assistant", Content: "released"}, Block: block}}}

	type result struct {
		msg Message
		err error
	}
	resCh := make(chan result, 1)
	go func() {
		msg, err := f.Chat(context.Background(), "m", nil, nil, ChatOptions{}, nil)
		resCh <- result{msg, err}
	}()
	waitForCalls(t, f, 1)
	close(block)

	select {
	case r := <-resCh:
		if r.err != nil || r.msg.Content != "released" {
			t.Errorf("result = %+v, %v", r.msg, r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Chat did not return after block was released")
	}
}

func TestFakeCancelledBeforeCallConsumesNothing(t *testing.T) {
	f := &Fake{Script: []FakeReply{Say("kept")}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Chat(ctx, "m", nil, nil, ChatOptions{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.Script) != 1 || len(f.Calls) != 0 {
		t.Errorf("script=%d calls=%d, want 1 and 0", len(f.Script), len(f.Calls))
	}
}

func TestFakeListModels(t *testing.T) {
	models := []ModelInfo{{Name: "gemma4:12b", SizeBytes: 7516192768}}
	f := &Fake{Models: models}
	got, err := f.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if !reflect.DeepEqual(got, models) {
		t.Errorf("ListModels = %+v, want %+v", got, models)
	}
}

func TestFakeConcurrentUse(t *testing.T) {
	const n = 50
	script := make([]FakeReply, n)
	for i := range script {
		script[i] = Say(fmt.Sprintf("r%d", i))
	}
	f := &Fake{Script: script, Models: []ModelInfo{{Name: "m"}}}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = make(map[string]int)
	)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			msg, err := f.Chat(context.Background(), "m", []Message{{Role: "user", Content: "x"}}, nil, ChatOptions{}, func(Chunk) {})
			if err != nil {
				t.Errorf("Chat: %v", err)
				return
			}
			mu.Lock()
			seen[msg.Content]++
			mu.Unlock()
		}()
		go func() {
			defer wg.Done()
			if _, err := f.ListModels(context.Background()); err != nil {
				t.Errorf("ListModels: %v", err)
			}
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Errorf("got %d distinct replies, want %d", len(seen), n)
	}
	for content, count := range seen {
		if count != 1 {
			t.Errorf("reply %q handed out %d times", content, count)
		}
	}
	if len(f.Calls) != n {
		t.Errorf("recorded %d calls, want %d", len(f.Calls), n)
	}
}
