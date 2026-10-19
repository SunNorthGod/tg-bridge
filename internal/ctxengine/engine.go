// Package ctxengine assembles LLM prompts with layered, cache-friendly
// context: stable layers (system, glossary, summary) sit at the front
// so provider-side prefix caches stay warm, and the message window is
// append-only until a watermark compaction folds its older half into
// the rolling summary.
package ctxengine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"tgbridge/internal/config"
	"tgbridge/internal/llm"
	"tgbridge/internal/store"
)

// LLM is the subset of *llm.Client the engine depends on; the same
// interface is faked in unit tests.
type LLM interface {
	Chat(ctx context.Context, req llm.ChatRequest) (llm.ChatResponse, error)
}

type Engine struct {
	store *store.Store
	llm   LLM
	cfg   config.Context
}

func New(st *store.Store, client LLM, cfg config.Context) *Engine {
	return &Engine{store: st, llm: client, cfg: cfg}
}

const sysOutgoing = `你是专业翻译,为一个中文使用者(称呼为"我")与乌克兰语使用者(称呼为"对方")的私聊做翻译。
任务:把「待翻译消息」里我写的中文翻译成乌克兰语。
规则:
- 只输出译文本身,禁止任何解释、注释、引号包裹或复述原文
- 语气延续对话记录中的风格,自然地道,符合乌克兰语口语习惯;原文书面则译得书面
- 人称、称呼、专有名词的译法必须与对话记录中此前的译法保持一致
- 术语表列出的条目必须按给定译法使用
- 原文中夹带的英文单词也要融进乌克兰语译文`

const sysIncoming = `你是专业翻译,为一个中文使用者(称呼为"我")与乌克兰语使用者(称呼为"对方")的私聊做翻译。
任务:把「待翻译消息」里对方发来的消息翻译成简体中文。
规则:
- 只输出译文本身,禁止任何解释、注释、引号包裹或复述原文
- 消息可能是乌克兰语、英语或俄语,也可能混杂,一律翻译成中文
- 语气贴合原消息:口语保持口语,正式保持正式
- 人称、称呼、专有名词的译法必须与对话记录中此前的译法保持一致
- 术语表列出的条目必须按给定译法使用`

const sysSummarize = `你会把一份旧的私聊摘要与新移出的对话记录合并成一份新摘要,供后续翻译提供背景。
输出结构(保留全部四个小节,每节一行或多行):
话题状态:当前聊到哪里
双方立场与情绪:
关键事实:人名、地名、数字、日期、金额、约定的时间点必须逐字保留
约定与未决事项:
只输出摘要正文,长度不超过 %d 字,信息取舍时数字与专名优先。`

// Translate renders the translation for message m (whose source text is
// already stored), running compaction first when the window is over
// the watermark. Direction: "out" = user's Chinese → Ukrainian,
// "in" = peer message → Chinese.
func (e *Engine) Translate(ctx context.Context, chat *store.Chat, m *store.Message) (llm.ChatResponse, string, error) {
	e.maybeCompact(ctx, chat)
	total, content, err := e.translateChunks(ctx, chat, m)
	return total, content, err
}

func (e *Engine) translateChunks(ctx context.Context, chat *store.Chat, m *store.Message) (llm.ChatResponse, string, error) {
	chunks := splitBySentence(m.SourceText, e.cfg.MaxInputChars)
	var total llm.ChatResponse
	var out strings.Builder
	for _, chunk := range chunks {
		msgs := e.buildMessages(ctx, chat, m.Direction, chunk)
		res, err := e.llm.Chat(ctx, llm.ChatRequest{Messages: msgs, Kind: "translate"})
		e.logCall(m.ID, "translate", res, err)
		if err != nil {
			return total, "", err
		}
		total.Usage.PromptTokens += res.Usage.PromptTokens
		total.Usage.CachedTokens += res.Usage.CachedTokens
		total.Usage.CompletionTokens += res.Usage.CompletionTokens
		total.DurationMS += res.DurationMS
		out.WriteString(strings.TrimSpace(res.Content))
	}
	return total, out.String(), nil
}

func (e *Engine) buildMessages(ctx context.Context, chat *store.Chat, direction, source string) []llm.Message {
	var sys strings.Builder
	if direction == "out" {
		sys.WriteString(sysOutgoing)
	} else {
		sys.WriteString(sysIncoming)
	}
	if chat.Persona != "" {
		sys.WriteString("\n\n【身份与语气】\n")
		sys.WriteString(chat.Persona)
	}
	if terms, err := e.store.ListGlossary(chat.ID); err == nil && len(terms) > 0 {
		sys.WriteString("\n\n【术语表】\n")
		for _, t := range terms {
			fmt.Fprintf(&sys, "%s → %s\n", t[0], t[1])
		}
	}
	if chat.Summary != "" {
		sys.WriteString("\n\n【对话背景摘要】\n")
		sys.WriteString(chat.Summary)
	}

	var win strings.Builder
	ids := ParseWindow(chat.Window)
	if len(ids) > 0 {
		msgs, err := e.store.GetMessagesByIDs(chat.ID, ids)
		if err == nil {
			win.WriteString("对话记录(→ 后是对应译文):\n")
			for _, wm := range msgs {
				who := "对方"
				if wm.Direction == "out" {
					who = "我"
				}
				tgt := wm.TargetText
				if tgt == "" {
					tgt = wm.SourceText
				}
				fmt.Fprintf(&win, "[%s] %s → %s\n", who, wm.SourceText, tgt)
			}
		}
	}

	return []llm.Message{
		{Role: "system", Content: sys.String()},
		{Role: "user", Content: win.String()},
		{Role: "user", Content: "待翻译消息(只翻译这一条,只输出译文):\n" + source},
	}
}

// AppendWindow adds a finished message to the L3 window. Window is
// append-only; compaction is lazy and happens at the next Translate,
// so this stays O(1) on the hot path.
func (e *Engine) AppendWindow(chat *store.Chat, msgID int64) error {
	ids := ParseWindow(chat.Window)
	ids = append(ids, msgID)
	return e.store.UpdateChatContext(chat.ID, chat.Summary, EncodeWindow(ids))
}

// maybeCompact folds the older half of the window into the summary when
// the estimated window size crosses the watermark, or when the last
// window message is older than the idle threshold. A failed summarize
// call leaves summary and window untouched — translation proceeds on
// the oversized window and compaction retries next time.
func (e *Engine) maybeCompact(ctx context.Context, chat *store.Chat) {
	ids := ParseWindow(chat.Window)
	if len(ids) < 4 {
		return
	}
	msgs, err := e.store.GetMessagesByIDs(chat.ID, ids)
	if err != nil || len(msgs) == 0 {
		return
	}
	windowTokens := 0
	for _, m := range msgs {
		windowTokens += EstimateTokens(m.SourceText + m.TargetText)
	}
	idle := time.Duration(e.cfg.IdleRecompactHrs) * time.Hour
	last, _ := time.Parse(time.RFC3339, msgs[len(msgs)-1].UpdatedAt)
	idleDue := !last.IsZero() && time.Since(last) > idle && windowTokens > e.cfg.WindowWatermark/5
	if windowTokens < e.cfg.WindowWatermark && !idleDue {
		return
	}

	// Keep the newest keepRatio share of the window; clamp keeps at
	// least one and evicts at least one. Multiplying the ratio as a
	// float first — int(0.5) truncates to zero and would evict nothing.
	keepCount := int(float64(len(ids)) * e.cfg.WindowKeepRatio)
	if keepCount < 1 {
		keepCount = 1
	}
	if keepCount > len(ids)-1 {
		keepCount = len(ids) - 1
	}
	splitAt := len(ids) - keepCount
	evicted, kept := msgs[:splitAt], ids[splitAt:]

	var rec strings.Builder
	for _, m := range evicted {
		who := "对方"
		if m.Direction == "out" {
			who = "我"
		}
		tgt := m.TargetText
		if tgt == "" {
			tgt = m.SourceText
		}
		fmt.Fprintf(&rec, "[%s] %s → %s\n", who, m.SourceText, tgt)
	}
	sysMsgs := []llm.Message{
		{Role: "system", Content: fmt.Sprintf(sysSummarize, e.cfg.SummaryMax)},
		{Role: "user", Content: fmt.Sprintf("【旧摘要】\n%s\n\n【新移出的对话记录】\n%s", chat.Summary, rec.String())},
	}
	res, err := e.llm.Chat(ctx, llm.ChatRequest{Messages: sysMsgs, Kind: "summarize", MaxTokens: e.cfg.SummaryMax * 2})
	e.logCall(0, "summarize", res, err)
	if err != nil {
		return
	}
	chat.Summary = strings.TrimSpace(res.Content)
	chat.Window = EncodeWindow(kept)
	_ = e.store.UpdateChatContext(chat.ID, chat.Summary, chat.Window)
}

func (e *Engine) logCall(messageID int64, kind string, res llm.ChatResponse, err error) {
	c := store.LLMCall{
		MessageID:        messageID,
		Kind:             kind,
		Endpoint:         res.Endpoint,
		Model:            res.Model,
		PromptTokens:     res.Usage.PromptTokens,
		CachedTokens:     res.Usage.CachedTokens,
		CompletionTokens: res.Usage.CompletionTokens,
		DurationMS:       res.DurationMS,
	}
	if err != nil {
		c.Error = err.Error()
	}
	_ = e.store.InsertLLMCall(c)
}

// splitBySentence breaks text into chunks of at most max runes, cutting
// at sentence punctuation when possible.
func splitBySentence(text string, max int) []string {
	if max <= 0 || len([]rune(text)) <= max {
		return []string{text}
	}
	runes := []rune(text)
	var chunks []string
	for len(runes) > 0 {
		if len(runes) <= max {
			chunks = append(chunks, string(runes))
			break
		}
		cut := max
		for i := max; i > max/2; i-- {
			if strings.ContainsRune("。!?;!?;\n", runes[i-1]) {
				cut = i
				break
			}
		}
		chunks = append(chunks, string(runes[:cut]))
		runes = runes[cut:]
	}
	return chunks
}

// ParseWindow decodes the persisted window id list; empty string → nil.
func ParseWindow(s string) []int64 {
	if s == "" {
		return nil
	}
	var ids []int64
	if err := json.Unmarshal([]byte(s), &ids); err != nil {
		return nil
	}
	return ids
}

// EncodeWindow is the inverse of ParseWindow.
func EncodeWindow(ids []int64) string {
	if len(ids) == 0 {
		return ""
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return ""
	}
	return string(b)
}
