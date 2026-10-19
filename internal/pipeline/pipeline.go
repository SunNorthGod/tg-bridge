// Package pipeline routes Telegram updates and web sends through the
// per-chat translation pipeline. Each chat has one worker goroutine fed
// by one channel: messages in a chat are processed strictly in order,
// chats run concurrently, and the shared *store.Chat window state is
// only ever mutated inside the owning worker.
package pipeline

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"tgbridge/internal/ctxengine"
	"tgbridge/internal/store"
	"tgbridge/internal/tggw"
)

// Sender is the MTProto send capability; implemented by tggw.Gateway.
type Sender interface {
	SendText(ctx context.Context, chat store.Chat, text string) (int, error)
}

// OutResult is the synchronous reply to a web send request.
type OutResult struct {
	RowID  int64
	MsgID  int
	Target string
	Err    error
}

type incomingJob struct {
	rowID     int64
	chatID    int64
	translate bool // false = append to context window only
}

type outgoingJob struct {
	chatID int64
	text   string
	reply  chan OutResult
}

type job struct {
	incoming *incomingJob
	outgoing *outgoingJob
}

type Pipeline struct {
	ctx     context.Context
	store   *store.Store
	engine  *ctxengine.Engine
	bus     *Bus
	sender  Sender
	mu      sync.Mutex
	workers map[int64]chan job
}

// New wires the pipeline. The MTProto sender is attached later via
// SetSender because the gateway and the pipeline reference each other.
func New(ctx context.Context, st *store.Store, eng *ctxengine.Engine, bus *Bus, sender Sender) *Pipeline {
	return &Pipeline{
		ctx:     ctx,
		store:   st,
		engine:  eng,
		bus:     bus,
		sender:  sender,
		workers: make(map[int64]chan job),
	}
}

// SetSender attaches the MTProto gateway after the event loop is wired.
// Must be called before any outgoing job can be processed.
func (p *Pipeline) SetSender(s Sender) {
	p.mu.Lock()
	p.sender = s
	p.mu.Unlock()
}

func (p *Pipeline) senderNow() Sender {
	p.mu.Lock()
	s := p.sender
	p.mu.Unlock()
	return s
}

// OnTGMessage implements tggw.Sink. It does only fast store work —
// dedup, discovery, row insert — and hands everything else to the chat
// worker. Idempotency key: (chat, tg_msg_id); replays are dropped,
// incoming edits continue under a fresh revision.
func (p *Pipeline) OnTGMessage(m tggw.TGMessage) {
	chatID, err := p.store.UpsertChatFromTG("user", m.TgUserID, m.AccessHash, m.Username, m.Title)
	if err != nil {
		slog.Error("chat upsert failed", "tg_user_id", m.TgUserID, "err", err)
		return
	}
	chat, err := p.store.GetChat(chatID)
	if err != nil {
		slog.Error("chat load failed", "chat_id", chatID, "err", err)
		return
	}

	// Idempotency covers both directions: the server may echo our own
	// API sends back as UpdateNewMessage, which must not double-insert.
	existing, _ := p.store.GetByTgMsg(chatID, int64(m.MsgID))
	if existing != nil && !m.Edit {
		return
	}
	rev := 0
	if existing != nil {
		rev = existing.EditRev + 1
	}

	direction := "in"
	if m.Out {
		direction = "out"
	}
	// Own-device sends (typed in the Telegram client instead of the
	// web UI) were already delivered verbatim; they are recorded as
	// context only. Same for chats where translation is disabled.
	translate := chat.Enabled && !m.Out
	status := "pending"
	if !translate {
		status = "skipped"
	}
	row := &store.Message{
		ChatID:     chatID,
		TgMsgID:    int64(m.MsgID),
		EditRev:    rev,
		Direction:  direction,
		SourceText: m.Text,
		Status:     status,
	}
	rowID, err := p.store.InsertMessage(row)
	if err != nil {
		slog.Error("message insert failed", "chat_id", chatID, "err", err)
		return
	}
	p.bus.Publish(Event{Type: "chats"})

	if chat.Enabled || m.Out {
		p.enqueue(chatID, job{incoming: &incomingJob{rowID: rowID, chatID: chatID, translate: translate}})
	}
}

// RequeuePending re-dispatches inbound messages stuck in pending by a
// previous crash. Chats disabled since then are resolved as skipped.
func (p *Pipeline) RequeuePending() {
	rows, err := p.store.ListPendingIncoming()
	if err != nil {
		slog.Error("pending recovery query failed", "err", err)
		return
	}
	for _, row := range rows {
		chat, err := p.store.GetChat(row.ChatID)
		if err != nil || !chat.Enabled {
			_ = p.store.SetMessageStatus(row.ID, "skipped")
			continue
		}
		p.enqueue(chat.ID, job{incoming: &incomingJob{rowID: row.ID, chatID: chat.ID, translate: true}})
	}
	if len(rows) > 0 {
		slog.Info("requeued pending messages", "count", len(rows))
	}
}

// SendOutgoing translates and sends a message from the web UI, blocking
// until the chat worker replies (HTTP handler holds the request open).
func (p *Pipeline) SendOutgoing(ctx context.Context, chatID int64, text string) OutResult {
	ch := p.workerFor(chatID)
	reply := make(chan OutResult, 1)
	select {
	case ch <- job{outgoing: &outgoingJob{chatID: chatID, text: text, reply: reply}}:
	default:
		return OutResult{Err: errors.New("该对话处理队列已满,请稍后重试")}
	}
	select {
	case r := <-reply:
		return r
	case <-ctx.Done():
		return OutResult{Err: ctx.Err()}
	}
}

// enqueue hands a job to the chat worker. A full queue drops the
// translation (the row stays stored and is shown as-is) instead of
// blocking the gotd update handler.
func (p *Pipeline) enqueue(chatID int64, j job) {
	ch := p.workerFor(chatID)
	select {
	case ch <- j:
	default:
		slog.Warn("chat queue full, dropping job", "chat_id", chatID)
		if ij := j.incoming; ij != nil && ij.translate {
			p.store.SetMessageStatus(ij.rowID, "skipped")
		}
	}
}

func (p *Pipeline) workerFor(chatID int64) chan job {
	p.mu.Lock()
	defer p.mu.Unlock()
	if ch, ok := p.workers[chatID]; ok {
		return ch
	}
	ch := make(chan job, 64)
	p.workers[chatID] = ch
	go p.workerLoop(chatID, ch)
	return ch
}

func (p *Pipeline) workerLoop(chatID int64, jobs <-chan job) {
	for {
		select {
		case <-p.ctx.Done():
			return
		case j := <-jobs:
			if j.incoming != nil {
				p.processIncoming(j.incoming)
			} else if j.outgoing != nil {
				p.processOutgoing(j.outgoing)
			}
		}
	}
}

func (p *Pipeline) processIncoming(ij *incomingJob) {
	row, err := p.store.GetMessageByID(ij.rowID)
	if err != nil {
		slog.Error("incoming row lost", "row", ij.rowID, "err", err)
		return
	}
	chat, err := p.store.GetChat(row.ChatID)
	if err != nil {
		slog.Error("chat lost", "chat_id", row.ChatID, "err", err)
		return
	}

	if ij.translate && ctxengine.IsChinese(row.SourceText) {
		// Peer wrote Chinese directly — nothing to translate.
		row.LangDetected = "zh"
		p.store.UpdateMessageResult(row.ID, "skipped", row.TargetText, 0, 0, 0, 0)
		row.Status = "skipped"
	} else if ij.translate {
		res, translation, err := p.engine.Translate(p.ctx, chat, row)
		if err != nil {
			slog.Error("translate failed", "chat_id", chat.ID, "err", err)
			p.store.UpdateMessageResult(row.ID, "failed", row.TargetText, 0, 0, 0, res.DurationMS)
			row.Status = "failed"
		} else {
			p.store.UpdateMessageResult(row.ID, "done", translation,
				res.Usage.PromptTokens, res.Usage.CachedTokens, res.Usage.CompletionTokens, res.DurationMS)
			row.TargetText = translation
			row.Status = "done"
		}
	}

	if chat.Enabled {
		// Window appends happen exclusively in this worker so the
		// maybeCompact/AppendWindow pair cannot interleave.
		if err := p.engine.AppendWindow(chat, row.ID); err != nil {
			slog.Error("window append failed", "chat_id", chat.ID, "err", err)
		}
	}
	p.publishMessage(row)
}

func (p *Pipeline) processOutgoing(oj *outgoingJob) {
	reply := func(r OutResult) {
		select {
		case oj.reply <- r:
		default:
		}
	}
	chat, err := p.store.GetChat(oj.chatID)
	if err != nil {
		reply(OutResult{Err: errors.New("对话不存在")})
		return
	}
	if !chat.Enabled {
		reply(OutResult{Err: errors.New("该对话未启用翻译")})
		return
	}
	sender := p.senderNow()
	if sender == nil {
		reply(OutResult{Err: errors.New("Telegram 尚未连接")})
		return
	}

	// Non-Chinese input (Ukrainian, English, emoji) is delivered as-is.
	if !ctxengine.IsChinese(oj.text) {
		msgID, err := sender.SendText(p.ctx, *chat, oj.text)
		if err != nil {
			reply(OutResult{Err: err})
			return
		}
		row := &store.Message{
			ChatID:     chat.ID,
			TgMsgID:    int64(msgID),
			Direction:  "out",
			SourceText: oj.text,
			TargetText: oj.text,
			Status:     "skipped",
		}
		rowID, err := p.store.InsertMessage(row)
		if err == nil {
			_ = p.engine.AppendWindow(chat, rowID)
			p.publishMessage(row)
		}
		reply(OutResult{RowID: rowID, MsgID: msgID, Target: oj.text})
		return
	}

	// Placeholder row with tg_msg_id=0: the partial unique index leaves
	// local-only rows unconstrained; the real id is backfilled after
	// the send succeeds.
	row := &store.Message{
		ChatID:     chat.ID,
		Direction:  "out",
		SourceText: oj.text,
		Status:     "translating",
	}
	rowID, err := p.store.InsertMessage(row)
	if err != nil {
		reply(OutResult{Err: err})
		return
	}
	row.ID = rowID

	res, translation, err := p.engine.Translate(p.ctx, chat, row)
	if err != nil {
		p.store.UpdateMessageResult(rowID, "failed", "", 0, 0, 0, res.DurationMS)
		reply(OutResult{RowID: rowID, Err: err})
		return
	}
	msgID, err := sender.SendText(p.ctx, *chat, translation)
	if err != nil {
		p.store.UpdateMessageResult(rowID, "failed", translation,
			res.Usage.PromptTokens, res.Usage.CachedTokens, res.Usage.CompletionTokens, res.DurationMS)
		reply(OutResult{RowID: rowID, Err: err})
		return
	}
	p.store.UpdateMessageResult(rowID, "done", translation,
		res.Usage.PromptTokens, res.Usage.CachedTokens, res.Usage.CompletionTokens, res.DurationMS)
	p.store.SetMessageTgMsgID(rowID, int64(msgID))
	row.TgMsgID = int64(msgID)
	row.TargetText = translation
	row.Status = "done"
	_ = p.engine.AppendWindow(chat, rowID)
	p.publishMessage(row)
	reply(OutResult{RowID: rowID, MsgID: msgID, Target: translation})
}

func (p *Pipeline) publishMessage(m *store.Message) {
	payload := PayloadFromMessage(m)
	p.bus.Publish(Event{Type: "message", ChatID: m.ChatID, Message: &payload})
}
