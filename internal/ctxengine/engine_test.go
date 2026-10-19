package ctxengine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"tgbridge/internal/config"
	"tgbridge/internal/llm"
	"tgbridge/internal/store"
)

// fakeLLM records every request and answers with canned payloads.
type fakeLLM struct {
	mu       sync.Mutex
	requests []llm.ChatRequest
}

func (f *fakeLLM) Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	n := len(f.requests)
	f.mu.Unlock()
	if req.Kind == "summarize" {
		return llm.ChatResponse{
			Content: "新摘要", Endpoint: "fake", Model: "fake",
			Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5}, DurationMS: 3,
		}, nil
	}
	return llm.ChatResponse{
		Content: fmt.Sprintf("переклад-%d", n), Endpoint: "fake", Model: "fake",
		Usage: llm.Usage{PromptTokens: 100, CachedTokens: 60, CompletionTokens: 20}, DurationMS: 7,
	}, nil
}

func (f *fakeLLM) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	kinds := make([]string, len(f.requests))
	for i, r := range f.requests {
		kinds[i] = r.Kind
	}
	return kinds
}

func (f *fakeLLM) last() llm.ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func testEngine(t *testing.T, cfg config.Context) (*Engine, *store.Store, *fakeLLM) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "engine.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeLLM{}
	return New(st, f, cfg), st, f
}

func TestLanguageDetection(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"你好,明天见", true},
		{"你好,帮我问一下 prices", true},
		{"Привіт, як справи?", false},
		{"Hello there my friend", false},
		{"OK", false},
		{"123 😊", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsChinese(c.in); got != c.want {
			t.Errorf("IsChinese(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWindowRoundtrip(t *testing.T) {
	if ids := ParseWindow(EncodeWindow([]int64{3, 1, 2})); len(ids) != 3 || ids[0] != 3 || ids[2] != 2 {
		t.Errorf("window roundtrip broken: %v", ids)
	}
	if ids := ParseWindow(""); ids != nil {
		t.Errorf("empty window should be nil: %v", ids)
	}
	if s := EncodeWindow(nil); s != "" {
		t.Errorf("nil window should encode to empty: %q", s)
	}
}

func TestSplitBySentence(t *testing.T) {
	long := strings.Repeat("这是测试句子。", 40) // 240 runes
	max := 50
	chunks := splitBySentence(long, max)
	if len(chunks) < 4 {
		t.Fatalf("expected several chunks, got %d", len(chunks))
	}
	for _, c := range chunks {
		if len([]rune(c)) > max {
			t.Errorf("chunk exceeds limit: %d runes", len([]rune(c)))
		}
	}
	if strings.Join(chunks, "") != long {
		t.Error("chunks lost content")
	}
	if got := splitBySentence("短句", 50); len(got) != 1 || got[0] != "短句" {
		t.Errorf("short text must stay whole: %v", got)
	}
}

// TestTranslateCompactsOverWatermark verifies the core cache-alignment
// contract: translating with an oversized window must first fold the
// older half into the summary, and the assembled prompt must contain
// the kept half but not the evicted messages.
func TestTranslateCompactsOverWatermark(t *testing.T) {
	cfg := config.Context{
		WindowWatermark: 200, WindowKeepRatio: 0.5,
		SummaryMax: 300, MaxInputChars: 500, IdleRecompactHrs: 12,
	}
	eng, st, fake := testEngine(t, cfg)
	chatID, err := st.UpsertChatFromTG("user", 9, 1, "oleh", "Oleh")
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for i := 0; i < 6; i++ {
		m := &store.Message{
			ChatID: chatID, TgMsgID: int64(100 + i), Direction: "in",
			SourceText: fmt.Sprintf("повідомлення номер %d %s", i, strings.Repeat("тест ", 8)),
			TargetText: fmt.Sprintf("消息%d %s", i, strings.Repeat("测试", 8)),
			Status:     "done",
		}
		id, err := st.InsertMessage(m)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := st.UpdateChatContext(chatID, "", EncodeWindow(ids)); err != nil {
		t.Fatal(err)
	}
	chat, err := st.GetChat(chatID)
	if err != nil {
		t.Fatal(err)
	}

	m := &store.Message{ChatID: chatID, TgMsgID: 999, Direction: "in", SourceText: "Як справи?", Status: "pending"}
	rowID, err := st.InsertMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	m.ID = rowID

	_, content, err := eng.Translate(context.Background(), chat, m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "переклад-") {
		t.Errorf("unexpected translation: %q", content)
	}

	kinds := fake.kinds()
	if len(kinds) != 2 || kinds[0] != "summarize" || kinds[1] != "translate" {
		t.Fatalf("expected [summarize, translate], got %v", kinds)
	}

	// Window halved: 6 → 3 ids.
	if got := len(ParseWindow(chat.Window)); got != 3 {
		t.Errorf("window after compaction has %d ids, want 3", got)
	}
	// Summary persisted.
	fresh, _ := st.GetChat(chatID)
	if fresh.Summary != "新摘要" {
		t.Errorf("summary not persisted: %q", fresh.Summary)
	}

	// Prompt layout: system carries summary; window turn carries kept
	// messages only; final turn carries the new text.
	req := fake.last()
	if len(req.Messages) != 3 {
		t.Fatalf("prompt layers = %d, want 3", len(req.Messages))
	}
	if !strings.Contains(req.Messages[0].Content, "新摘要") {
		t.Error("system layer missing summary")
	}
	if strings.Contains(req.Messages[1].Content, "повідомлення номер 0") {
		t.Error("evicted message leaked into window layer")
	}
	if !strings.Contains(req.Messages[1].Content, "повідомлення номер 5") {
		t.Error("kept message missing from window layer")
	}
	if !strings.Contains(req.Messages[2].Content, "Як справи?") {
		t.Error("final layer missing the text to translate")
	}
}

// TestTranslateUnderWatermarkSkipsSummary pins the no-compaction path:
// a window under the watermark must not trigger a summarize call, and
// the window layer must be the stable prefix (contains everything).
func TestTranslateUnderWatermarkSkipsSummary(t *testing.T) {
	cfg := config.Context{
		WindowWatermark: 100000, WindowKeepRatio: 0.5,
		SummaryMax: 300, MaxInputChars: 500, IdleRecompactHrs: 12,
	}
	eng, st, fake := testEngine(t, cfg)
	chatID, _ := st.UpsertChatFromTG("user", 5, 5, "ika", "Ika")
	id, err := st.InsertMessage(&store.Message{ChatID: chatID, TgMsgID: 1, Direction: "in", SourceText: "Привіт", TargetText: "你好", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	st.UpdateChatContext(chatID, "", EncodeWindow([]int64{id}))
	chat, _ := st.GetChat(chatID)

	m := &store.Message{ChatID: chatID, TgMsgID: 2, Direction: "out", SourceText: "你好呀", Status: "pending"}
	rowID, _ := st.InsertMessage(m)
	m.ID = rowID

	if _, _, err := eng.Translate(context.Background(), chat, m); err != nil {
		t.Fatal(err)
	}
	if kinds := fake.kinds(); len(kinds) != 1 || kinds[0] != "translate" {
		t.Fatalf("unexpected summarize under watermark: %v", kinds)
	}
	req := fake.last()
	if !strings.Contains(req.Messages[0].Content, "翻译成乌克兰语") {
		t.Error("outgoing direction must use the Ukrainian system prompt")
	}
	if !strings.Contains(req.Messages[1].Content, "Привіт") {
		t.Error("window layer missing prior message")
	}
}

func TestAppendWindow(t *testing.T) {
	cfg := config.Context{WindowWatermark: 100000, SummaryMax: 300, MaxInputChars: 500, IdleRecompactHrs: 12}
	eng, st, _ := testEngine(t, cfg)
	chatID, _ := st.UpsertChatFromTG("user", 7, 7, "", "")
	st.UpdateChatContext(chatID, "", EncodeWindow([]int64{11}))
	chat, _ := st.GetChat(chatID)

	if err := eng.AppendWindow(chat, 12); err != nil {
		t.Fatal(err)
	}
	fresh, _ := st.GetChat(chatID)
	ids := ParseWindow(fresh.Window)
	if len(ids) != 2 || ids[0] != 11 || ids[1] != 12 {
		t.Fatalf("window after append: %v", ids)
	}
}

func TestGlossaryLayer(t *testing.T) {
	cfg := config.Context{WindowWatermark: 100000, SummaryMax: 300, MaxInputChars: 500, IdleRecompactHrs: 12}
	eng, st, fake := testEngine(t, cfg)
	chatID, _ := st.UpsertChatFromTG("user", 8, 8, "", "")
	chat, _ := st.GetChat(chatID)

	m := &store.Message{ChatID: chatID, TgMsgID: 1, Direction: "in", SourceText: "Київ", Status: "pending"}
	rowID, _ := st.InsertMessage(m)
	m.ID = rowID
	if _, _, err := eng.Translate(context.Background(), chat, m); err != nil {
		t.Fatal(err)
	}
	// No glossary configured: layer absent — sanity check only.
	req := fake.last()
	if strings.Contains(req.Messages[0].Content, "【术语表】") {
		t.Error("empty glossary should not emit the layer")
	}
}
