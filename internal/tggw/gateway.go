// Package tggw wraps the MTProto client (gotd): session login driven by
// the web UI, update normalization, and sending messages as the
// logged-in user.
//
// First-time login does not touch the terminal: an unauthorized session
// parks in webAuthLoop and the panel submits phone/code/password via
// SubmitAuth. This keeps the service startable under systemd (no TTY)
// and removes any SSH interaction from the onboarding path.
package tggw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgbridge/internal/config"
	"tgbridge/internal/store"
)

// Auth states surfaced to the web panel.
const (
	AuthStateOK           = "ok"
	AuthStateNeedPhone    = "need_phone"
	AuthStateNeedCode     = "need_code"
	AuthStateNeedPassword = "need_password"
)

type authRequest struct {
	action         string // "code" | "verify" | "password"
	phone, code, password string
	resultCh       chan authResult
}

type authResult struct {
	state string
	err   string
}

// TGMessage is the normalized private-chat event handed to the sink.
// AccessHash is zero unless the update batch carried the peer's user
// record; the pipeline re-resolves it on demand when sending.
type TGMessage struct {
	TgUserID   int64
	AccessHash int64
	Username   string
	Title      string
	MsgID      int
	Text       string
	Out        bool // true = sent by our own account from any device
	Edit       bool
}

// Sink receives normalized updates; implemented by pipeline.Pipeline.
type Sink interface {
	OnTGMessage(msg TGMessage)
}

// Gateway owns the MTProto connection. It is safe for concurrent use;
// Handle is called by gotd on its own goroutine.
type Gateway struct {
	cfg     config.Telegram
	sink    Sink
	client  *telegram.Client
	sender  *message.Sender
	selfID  int64
	mu      sync.RWMutex
	started chan struct{}

	authMu    sync.Mutex
	authState string
	authReqs  chan authRequest
	authPhone string
	authHash  string
}

func New(cfg config.Telegram, sink Sink) *Gateway {
	return &Gateway{
		cfg:       cfg,
		sink:      sink,
		started:   make(chan struct{}),
		authState: AuthStateNeedPhone,
		authReqs:  make(chan authRequest),
	}
}

// Run blocks until ctx is done; gotd reconnects internally on network
// errors. The callback parks in webAuthLoop when the session is not
// authorized yet, so the process stays up under systemd.
func (g *Gateway) Run(ctx context.Context) error {
	client := telegram.NewClient(g.cfg.APIID, g.cfg.APIHash, telegram.Options{
		SessionStorage: &session.FileStorage{Path: g.cfg.SessionFile},
		UpdateHandler:  g,
	})
	g.mu.Lock()
	g.client = client
	g.mu.Unlock()

	return client.Run(ctx, func(ctx context.Context) error {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return fmt.Errorf("telegram auth status: %w", err)
		}
		if !st.Authorized {
			g.setAuthState(AuthStateNeedPhone)
			slog.Info("session not authorized, waiting for web login")
			if err := g.webAuthLoop(ctx, client); err != nil {
				return err
			}
			slog.Info("telegram login completed via web")
		} else {
			g.setAuthState(AuthStateOK)
		}
		self, err := client.Self(ctx)
		if err != nil {
			return fmt.Errorf("resolve self: %w", err)
		}
		g.mu.Lock()
		g.selfID = self.ID
		g.sender = message.NewSender(client.API())
		close(g.started)
		g.mu.Unlock()
		slog.Info("telegram connected", "self_id", self.ID, "username", self.Username)
		<-ctx.Done()
		return ctx.Err()
	})
}

// WaitReady blocks until the MTProto session is logged in.
func (g *Gateway) WaitReady(ctx context.Context) error {
	select {
	case <-g.started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// AuthStateInfo reports the login state for the panel wizard.
// Implements web.TGAuth.
func (g *Gateway) AuthStateInfo() string {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	return g.authState
}

// SubmitAuth performs one login step and returns the next state plus a
// user-readable error. Implements web.TGAuth. Blocking; the state loop
// always answers unless the process dies.
func (g *Gateway) SubmitAuth(action, phone, code, password string) (string, string) {
	req := authRequest{
		action: action, phone: phone, code: code, password: password,
		resultCh: make(chan authResult, 1),
	}
	select {
	case g.authReqs <- req:
	default:
		return "", "授权请求处理中,请稍候再试"
	}
	res := <-req.resultCh
	return res.state, res.err
}

func (g *Gateway) setAuthState(s string) {
	g.authMu.Lock()
	g.authState = s
	g.authMu.Unlock()
}

func (g *Gateway) currentState() string {
	g.authMu.Lock()
	defer g.authMu.Unlock()
	return g.authState
}

// webAuthLoop serializes web login steps on the MTProto connection
// goroutine — auth RPCs must run inside client.Run's callback.
func (g *Gateway) webAuthLoop(ctx context.Context, client *telegram.Client) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case req := <-g.authReqs:
			next, err := g.handleAuth(ctx, client, req)
			g.setAuthState(next)
			msg := ""
			if err != nil {
				msg = friendlyAuthError(err)
				slog.Error("web auth step failed", "action", req.action, "err", err)
			}
			req.resultCh <- authResult{state: next, err: msg}
			if err == nil && next == AuthStateOK {
				return nil
			}
		}
	}
}

func (g *Gateway) handleAuth(ctx context.Context, client *telegram.Client, req authRequest) (string, error) {
	switch req.action {
	case "code":
		req.phone = strings.TrimSpace(req.phone)
		if req.phone == "" {
			// Empty phone means "resend to the number already used".
			g.authMu.Lock()
			req.phone = g.authPhone
			g.authMu.Unlock()
		}
		if req.phone == "" {
			return g.currentState(), errors.New("手机号不能为空")
		}
		// AllowAppHash lets Telegram deliver the code inside the app on
		// the user's phone instead of an SMS when possible.
		sent, err := client.Auth().SendCode(ctx, req.phone, auth.SendCodeOptions{AllowAppHash: true})
		if err != nil {
			return g.currentState(), err
		}
		sc, ok := sent.(*tg.AuthSentCode)
		if !ok {
			return g.currentState(), errors.New("验证码请求响应异常")
		}
		g.authMu.Lock()
		g.authPhone, g.authHash = req.phone, sc.PhoneCodeHash
		g.authMu.Unlock()
		return AuthStateNeedCode, nil

	case "verify":
		g.authMu.Lock()
		phone, hash := g.authPhone, g.authHash
		g.authMu.Unlock()
		if hash == "" {
			return g.currentState(), errors.New("请先获取验证码")
		}
		_, err := client.Auth().SignIn(ctx, phone, strings.TrimSpace(req.code), hash)
		if err == nil {
			return AuthStateOK, nil
		}
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			return AuthStateNeedPassword, nil
		}
		return AuthStateNeedCode, err

	case "password":
		_, err := client.Auth().Password(ctx, req.password)
		if err != nil {
			return AuthStateNeedPassword, err
		}
		return AuthStateOK, nil

	default:
		return g.currentState(), errors.New("未知授权操作")
	}
}

func friendlyAuthError(err error) string {
	switch {
	case tgerr.Is(err, "PHONE_CODE_INVALID"):
		return "验证码不对,请检查后重试"
	case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
		return "验证码已过期,请重新发送"
	case tgerr.Is(err, "PHONE_NUMBER_INVALID"):
		return "手机号格式不对,需要国际格式,如 +8613800138000"
	case tgerr.Is(err, "FLOOD_WAIT"):
		return "操作太频繁,Telegram 要求等待几分钟后再试"
	case err == nil:
		return ""
	default:
		return "Telegram 返回错误: " + err.Error()
	}
}

// Handle implements telegram.UpdateHandler.
func (g *Gateway) Handle(ctx context.Context, class tg.UpdatesClass) error {
	switch u := class.(type) {
	case *tg.Updates:
		g.handleUpdates(u.Users, u.Updates)
	case *tg.UpdatesCombined:
		g.handleUpdates(u.Users, u.Updates)
	case *tg.UpdateShortMessage:
		g.sink.OnTGMessage(TGMessage{
			TgUserID: u.UserID,
			MsgID:    u.ID,
			Text:     u.Message,
			Out:      u.Out,
		})
	case *tg.UpdateShort:
		g.handleUpdates(nil, []tg.UpdateClass{u.Update})
	}
	return nil
}

// handleUpdates extracts the user records batched with the updates so
// display names, usernames and access hashes reach the store. Short
// update forms carry no user records — those chats keep whatever the
// DB already holds.
func (g *Gateway) handleUpdates(users []tg.UserClass, updates []tg.UpdateClass) {
	peers := make(map[int64]*tg.User, len(users))
	for _, uc := range users {
		if user, ok := uc.(*tg.User); ok {
			peers[user.ID] = user
		}
	}
	for _, u := range updates {
		switch v := u.(type) {
		case *tg.UpdateNewMessage:
			g.onMessage(v.Message, false, peers)
		case *tg.UpdateEditMessage:
			g.onMessage(v.Message, true, peers)
		}
	}
}

// onMessage filters raw message updates down to private text messages.
// Service messages, non-user peers, Saved Messages echoes and empty
// bodies (stickers, photos without caption) are dropped — they carry
// no translation value for v1.
func (g *Gateway) onMessage(class tg.MessageClass, edit bool, peers map[int64]*tg.User) {
	m, ok := class.(*tg.Message)
	if !ok {
		return
	}
	peer, ok := m.PeerID.(*tg.PeerUser)
	if !ok {
		return
	}
	if m.Out && peer.UserID == g.selfID {
		return // Saved Messages echo
	}
	if strings.TrimSpace(m.Message) == "" {
		return
	}
	var username, title string
	var hash int64
	if user, ok := peers[peer.UserID]; ok {
		username = user.Username
		title = strings.TrimSpace(user.FirstName + " " + user.LastName)
		hash = user.AccessHash
	}
	g.sink.OnTGMessage(TGMessage{
		TgUserID:   peer.UserID,
		AccessHash: hash,
		Username:   username,
		Title:      title,
		MsgID:      m.ID,
		Text:       m.Message,
		Out:        m.Out,
		Edit:       edit,
	})
}

// peerInput builds the InputPeer for a chat, resolving an unknown
// access hash on demand.
func (g *Gateway) peerInput(ctx context.Context, client *telegram.Client, chat store.Chat) (*tg.InputPeerUser, error) {
	hash := chat.AccessHash
	if hash == 0 {
		resolved, err := g.resolveAccessHash(ctx, client, chat.TgUserID)
		if err != nil {
			return nil, fmt.Errorf("resolve access hash: %w", err)
		}
		hash = resolved
	}
	return &tg.InputPeerUser{UserID: chat.TgUserID, AccessHash: hash}, nil
}

// MarkRead clears the unread badge on Telegram's side so the peer sees
// the read receipt when the user reads the chat in the panel. Best
// effort: errors are logged by the caller, never user-facing.
func (g *Gateway) MarkRead(ctx context.Context, chat store.Chat) error {
	g.mu.RLock()
	client := g.client
	g.mu.RUnlock()
	if client == nil {
		return errors.New("telegram: not connected")
	}
	peer, err := g.peerInput(ctx, client, chat)
	if err != nil {
		return err
	}
	_, err = client.API().MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{Peer: peer, MaxID: 0})
	return err
}

// SendText delivers text as the logged-in user and returns the Telegram
// message id. FLOOD_WAIT is retried once after 5 seconds; other errors
// propagate.
func (g *Gateway) SendText(ctx context.Context, chat store.Chat, text string) (int, error) {
	g.mu.RLock()
	sender := g.sender
	client := g.client
	g.mu.RUnlock()
	if sender == nil || client == nil {
		return 0, errors.New("telegram: not connected")
	}
	peer, err := g.peerInput(ctx, client, chat)
	if err != nil {
		return 0, err
	}

	res, err := sender.To(peer).Text(ctx, text)
	if err != nil && strings.Contains(err.Error(), "FLOOD_WAIT") {
		slog.Warn("flood wait, retrying in 5s")
		select {
		case <-time.After(5 * time.Second):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		res, err = sender.To(peer).Text(ctx, text)
	}
	if err != nil {
		return 0, fmt.Errorf("send message: %w", err)
	}
	return extractMsgID(res), nil
}

func (g *Gateway) resolveAccessHash(ctx context.Context, client *telegram.Client, userID int64) (int64, error) {
	users, err := client.API().UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUser{UserID: userID}})
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && user.ID == userID && user.AccessHash != 0 {
			return user.AccessHash, nil
		}
	}
	return 0, errors.New("user not resolvable")
}

func extractMsgID(class tg.UpdatesClass) int {
	switch v := class.(type) {
	case *tg.UpdateShortSentMessage:
		return v.ID
	case *tg.Updates:
		return findMsgID(v.Updates)
	case *tg.UpdatesCombined:
		return findMsgID(v.Updates)
	}
	return 0
}

func findMsgID(updates []tg.UpdateClass) int {
	for _, u := range updates {
		switch v := u.(type) {
		case *tg.UpdateMessageID:
			return v.ID
		case *tg.UpdateNewMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				return m.ID
			}
		}
	}
	return 0
}
