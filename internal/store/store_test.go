package store

import (
	"path/filepath"
	"testing"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestChatUpsertKeepsKnownPeer(t *testing.T) {
	st := testStore(t)
	id1, err := st.UpsertChatFromTG("user", 100, 111, "alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	// Echos sometimes carry no access hash or display name; those must
	// not wipe previously stored values.
	id2, err := st.UpsertChatFromTG("user", 100, 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("same peer mapped to two chats: %d vs %d", id1, id2)
	}
	c, err := st.GetChat(id1)
	if err != nil {
		t.Fatal(err)
	}
	if c.AccessHash != 111 || c.Username != "alice" || c.Title != "Alice" {
		t.Errorf("zero hash / empty names overwrote stored peer: %+v", c)
	}
	// A non-zero hash always wins (hashes rotate server-side).
	if _, err := st.UpsertChatFromTG("user", 100, 222, "", ""); err != nil {
		t.Fatal(err)
	}
	if c, _ = st.GetChat(id1); c.AccessHash != 222 {
		t.Errorf("non-zero hash did not overwrite: %+v", c)
	}
}

func TestMessageIdempotencyAndRevisions(t *testing.T) {
	st := testStore(t)
	chatID, err := st.UpsertChatFromTG("user", 1, 1, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: 42, Direction: "in", SourceText: "привіт", Status: "done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: 42, Direction: "in", SourceText: "dup", Status: "skipped"}); err == nil {
		t.Fatal("duplicate (chat, tg_msg_id, rev) insert must fail")
	}
	if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: 42, EditRev: 1, Direction: "in", SourceText: "редагування", Status: "pending"}); err != nil {
		t.Fatalf("edited revision must insert: %v", err)
	}
	latest, err := st.GetByTgMsg(chatID, 42)
	if err != nil || latest.EditRev != 1 {
		t.Fatalf("latest revision not found: %+v %v", latest, err)
	}
	// Placeholder rows (tg_msg_id=0) are exempt from uniqueness: two
	// failed outgoing attempts may coexist.
	if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: 0, Direction: "out", SourceText: "a", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: 0, Direction: "out", SourceText: "b", Status: "failed"}); err != nil {
		t.Fatalf("second zero-id row must be allowed: %v", err)
	}
}

func TestListMessagesPaging(t *testing.T) {
	st := testStore(t)
	chatID, _ := st.UpsertChatFromTG("user", 2, 2, "", "")
	for i := 0; i < 6; i++ {
		if _, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: int64(i), Direction: "in", SourceText: "m", Status: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := st.ListMessages(chatID, 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 4 || first[0].TgMsgID != 2 || first[3].TgMsgID != 5 {
		t.Fatalf("latest page wrong: %+v", first)
	}
	older, err := st.ListMessages(chatID, first[0].ID, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(older) != 2 || older[0].TgMsgID != 0 || older[1].TgMsgID != 1 {
		t.Fatalf("older page wrong: %+v", older)
	}
}

func TestGetMessagesByIDsKeepsOrder(t *testing.T) {
	st := testStore(t)
	chatID, _ := st.UpsertChatFromTG("user", 3, 3, "", "")
	var ids []int64
	for i := 0; i < 4; i++ {
		id, err := st.InsertMessage(&Message{ChatID: chatID, TgMsgID: int64(i), Direction: "in", SourceText: "m", Status: "done"})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	msgs, err := st.GetMessagesByIDs(chatID, []int64{ids[3], ids[1], ids[2]})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 || msgs[0].ID != ids[3] || msgs[1].ID != ids[1] || msgs[2].ID != ids[2] {
		t.Fatalf("window order not preserved: %+v", msgs)
	}
}
