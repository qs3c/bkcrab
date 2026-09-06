package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/qs3c/bkcrab/internal/store"
)

// fakeToolRecallStore 记录每次查询实际带上的作用域,好让测试断言"模型传
// 什么都改不了三元组"这件事,而不只是断言返回值。
type fakeToolRecallStore struct {
	rows map[int64]store.SessionToolMessage
	refs []store.ToolMsgRef

	gotUserID     string
	gotAgentID    string
	gotSessionKey string
	gotChatter    string
	gotSeq        int64
}

func (s *fakeToolRecallStore) ListSessionToolRefs(ctx context.Context, userID, agentID, sessionKey, chatterUserID string) ([]store.ToolMsgRef, error) {
	s.gotUserID, s.gotAgentID, s.gotSessionKey, s.gotChatter = userID, agentID, sessionKey, chatterUserID
	return s.refs, nil
}

func (s *fakeToolRecallStore) GetSessionToolMessage(ctx context.Context, userID, agentID, sessionKey, chatterUserID string, seq int64) (*store.SessionToolMessage, error) {
	s.gotUserID, s.gotAgentID, s.gotSessionKey, s.gotChatter, s.gotSeq = userID, agentID, sessionKey, chatterUserID, seq
	rec, ok := s.rows[seq]
	if !ok {
		return nil, store.ErrNotFound
	}
	return &rec, nil
}

func newRecallRegistry(t *testing.T, st ToolRecallStore) *Registry {
	t.Helper()
	r := NewRegistry("", "")
	r.SetToolRecallStore(st, "agent-a")
	r.SetOwnerUserID("user-a")
	r.SetRecallSessionKey("session-a")
	return r
}

func callRecall(t *testing.T, r *Registry, args string) (string, error) {
	t.Helper()
	fn := r.GetFunc("recall_tool_result")
	if fn == nil {
		t.Fatal("recall_tool_result was not registered")
	}
	return fn(context.Background(), json.RawMessage(args))
}

func assertToolOutputContains(t *testing.T, haystack string, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if !strings.Contains(haystack, needle) {
			t.Fatalf("tool output %q does not contain %q", haystack, needle)
		}
	}
}

func TestRecallToolResultReturnsOriginalByRef(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		41: {
			Seq:        41,
			ToolCallID: "call-a",
			Name:       "exec",
			Content:    "original tool output\nline two",
			CreatedAt:  time.Now().UTC(),
		},
	}}
	got, err := callRecall(t, newRecallRegistry(t, st), `{"ref":41}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	assertToolOutputContains(t, got,
		"[Recalled Tool Result]",
		"ref: 41",
		"tool: exec",
		"tool_call_id: call-a",
		"original tool output",
		"line two",
	)
}

// TestRecallToolResultPinsScopeToCurrentTurn 是这个工具的核心安全断言:
// 无论模型传什么,查询带的 (user, agent, session) 始终是服务端注入的当轮
// 三元组。模型能影响的只有 seq——而 seq 是每会话从 0 开始的计数器,离开
// 三元组没有寻址能力。
func TestRecallToolResultPinsScopeToCurrentTurn(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		7: {Seq: 7, Name: "exec", Content: "mine"},
	}}
	r := newRecallRegistry(t, st)

	// 参数里塞满想要越界的字段。它们在 schema 里根本不存在,
	// json.Unmarshal 直接丢弃。
	_, err := callRecall(t, r, `{"ref":7,
		"user_id":"victim-user","userID":"victim-user",
		"agent_id":"victim-agent","agentID":"victim-agent",
		"session_key":"victim-session","sessionKey":"victim-session",
		"chatter_user_id":"victim-chatter",
		"table":"users","role":"user","sql":"SELECT * FROM session_messages"}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if st.gotUserID != "user-a" || st.gotAgentID != "agent-a" || st.gotSessionKey != "session-a" {
		t.Fatalf("model args leaked into query scope: user=%q agent=%q session=%q",
			st.gotUserID, st.gotAgentID, st.gotSessionKey)
	}
	if st.gotSeq != 7 {
		t.Fatalf("seq = %d, want 7", st.gotSeq)
	}
}

// TestRecallToolResultRefusesWithoutTurnScope 锁住 fail-closed:没有绑定
// session_key 的回合(共享 registry 未经 ForTurn 绑定、后台任务)必须直接
// 拒绝,而不是拿一个空 session_key 去查——那会命中别的会话的行。
func TestRecallToolResultRefusesWithoutTurnScope(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{7: {Seq: 7}}}
	r := NewRegistry("", "")
	r.SetToolRecallStore(st, "agent-a")
	r.SetOwnerUserID("user-a")
	// 刻意不调 SetRecallSessionKey。

	if _, err := callRecall(t, r, `{"ref":7}`); err == nil {
		t.Fatal("expected refusal when the turn has no session scope")
	}
	if st.gotSeq != 0 || st.gotSessionKey != "" {
		t.Fatalf("store was queried despite missing scope: %+v", st)
	}
}

// TestRecallToolResultScopesNonOwnerChatter 覆盖群聊/共享 agent:同一个
// session_key 下有多个发言人时,非拥有者只能看到自己的行。
func TestRecallToolResultScopesNonOwnerChatter(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{7: {Seq: 7, Content: "x"}}}

	r := newRecallRegistry(t, st)
	r.SetChatterUserID("chatter-b")
	if _, err := callRecall(t, r, `{"ref":7}`); err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if st.gotChatter != "chatter-b" {
		t.Fatalf("chatter filter = %q, want %q", st.gotChatter, "chatter-b")
	}

	// 频道管理员身份不解除隔离:管理员管的是 agent 配置,读别人的工具输出
	// 是净新增的越权面。
	admin := newRecallRegistry(t, st)
	admin.SetChatterUserID("chatter-b")
	admin.SetCallerIsAdmin(true)
	if _, err := callRecall(t, admin, `{"ref":7}`); err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if st.gotChatter != "chatter-b" {
		t.Fatalf("callerIsAdmin lifted the chatter filter: %q", st.gotChatter)
	}

	// 拥有者本人不加聊天者约束——他在 UI 里本来就能翻完整归档。
	owner := newRecallRegistry(t, st)
	owner.SetChatterUserID("user-a")
	if _, err := callRecall(t, owner, `{"ref":7}`); err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if st.gotChatter != "" {
		t.Fatalf("owner query was chatter-scoped: %q", st.gotChatter)
	}
}

// TestRecallToolResultMissingRefDoesNotLeakExistence 未命中时的措辞不能
// 区分"不存在"和"存在但不属于你",否则这个工具就成了探测别人会话的探针。
func TestRecallToolResultMissingRefDoesNotLeakExistence(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{}}
	_, err := callRecall(t, newRecallRegistry(t, st), `{"ref":999}`)
	if err == nil {
		t.Fatal("expected an error for an unknown ref")
	}
	msg := strings.ToLower(err.Error())
	for _, leak := range []string{"another", "other session", "permission", "denied", "forbidden"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("error message leaks existence (%q): %q", leak, err)
		}
	}
	if !strings.Contains(msg, "999") {
		t.Fatalf("error should name the ref it could not find: %q", err)
	}
}

func TestRecallToolResultRejectsNegativeRef(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{}}
	if _, err := callRecall(t, newRecallRegistry(t, st), `{"ref":-1}`); err == nil {
		t.Fatal("expected an error for a negative ref")
	}
}

// TestRecallToolResultRefZeroIsGetNotList 是指针型 Ref 存在的理由:seq 从
// 0 开始,所以 ref=0 是合法的"取第一条",不能被当成"没传 ref"。
func TestRecallToolResultRefZeroIsGetNotList(t *testing.T) {
	st := &fakeToolRecallStore{
		rows: map[int64]store.SessionToolMessage{0: {Seq: 0, Name: "exec", Content: "first message"}},
		refs: []store.ToolMsgRef{{Seq: 0, Name: "exec"}},
	}
	got, err := callRecall(t, newRecallRegistry(t, st), `{"ref":0}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	assertToolOutputContains(t, got, "[Recalled Tool Result]", "first message")
}

func TestRecallToolResultTruncatesAtHardCap(t *testing.T) {
	huge := strings.Repeat("x", recallMaxLimit*3)
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		5: {Seq: 5, Name: "exec", Content: huge},
	}}
	// 模型要 100 万字符也只给到硬上限。
	got, err := callRecall(t, newRecallRegistry(t, st), `{"ref":5,"limit":1000000}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if len(got) > recallMaxLimit+2000 {
		t.Fatalf("returned %d chars, want roughly the %d hard cap", len(got), recallMaxLimit)
	}
	assertToolOutputContains(t, got, "truncated", "\"offset\":")
}

func TestRecallToolResultOffsetContinues(t *testing.T) {
	content := strings.Repeat("ab", recallDefaultLimit) // 2 * default
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		5: {Seq: 5, Name: "exec", Content: content},
	}}
	r := newRecallRegistry(t, st)

	first, err := callRecall(t, r, `{"ref":5}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	assertToolOutputContains(t, first, fmt.Sprintf("\"offset\":%d", recallDefaultLimit))

	last, err := callRecall(t, r, fmt.Sprintf(`{"ref":5,"offset":%d}`, len(content)-10))
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	if strings.Contains(last, "truncated") {
		t.Fatalf("final page should not advertise more data: %q", last)
	}
}

func TestRecallToolResultGrepReturnsOnlyMatches(t *testing.T) {
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, fmt.Sprintf("filler line %03d", i))
	}
	lines[120] = "FAIL: TestSomething (0.01s)"
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		5: {Seq: 5, Name: "exec", Content: strings.Join(lines, "\n")},
	}}
	got, err := callRecall(t, newRecallRegistry(t, st), `{"ref":5,"grep":"fail:"}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	assertToolOutputContains(t, got, "TestSomething", "121: ", "1 matching lines")
	if strings.Contains(got, "filler line 005") {
		t.Fatalf("grep returned non-matching regions: %q", got)
	}
}

func TestRecallToolResultGrepNoMatch(t *testing.T) {
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		5: {Seq: 5, Name: "exec", Content: "alpha\nbeta\n"},
	}}
	got, err := callRecall(t, newRecallRegistry(t, st), `{"ref":5,"grep":"gamma"}`)
	if err != nil {
		t.Fatalf("recall tool result: %v", err)
	}
	assertToolOutputContains(t, got, "no matching lines")
}

func TestRecallToolResultGrepHonorsLimit(t *testing.T) {
	content := "needle " + strings.Repeat("文🙂", recallMaxLimit)
	for _, tc := range []struct {
		name  string
		limit int
		want  int
	}{
		{"explicit", 100, 100},
		{"default", 0, recallDefaultLimit},
		{"negative", -1, recallDefaultLimit},
		{"hard_cap", recallMaxLimit + 1, recallMaxLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, note := renderRecallBody(content, recallToolResultArgs{Grep: "needle", Limit: tc.limit})
			if n := utf8.RuneCountInString(body); n != tc.want {
				t.Fatalf("returned %d characters, want %d", n, tc.want)
			}
			if !utf8.ValidString(body) || !strings.Contains(note, "truncated") {
				t.Fatalf("invalid pagination: utf8=%v note=%q", utf8.ValidString(body), note)
			}
		})
	}
}

func TestRecallToolResultGrepPagesPastHardCap(t *testing.T) {
	content := "needle " + strings.Repeat("文🙂", recallMaxLimit)
	expected := "1: " + content + "\n"
	st := &fakeToolRecallStore{rows: map[int64]store.SessionToolMessage{
		5: {Seq: 5, Name: "exec", Content: content},
	}}
	r := newRecallRegistry(t, st)
	args := `{"ref":5,"grep":"needle","limit":7000}`
	var joined strings.Builder
	for page := 0; page < 20; page++ {
		got, err := callRecall(t, r, args)
		if err != nil {
			t.Fatal(err)
		}
		header, body, ok := strings.Cut(got, "\n\n")
		if !ok || !utf8.ValidString(body) || utf8.RuneCountInString(body) > 7000 {
			t.Fatalf("invalid page %d", page)
		}
		joined.WriteString(body)
		if !strings.Contains(header, "truncated") {
			if joined.String() != expected {
				t.Fatalf("pages did not reconstruct grep output: got %d chars, want %d", utf8.RuneCountInString(joined.String()), utf8.RuneCountInString(expected))
			}
			return
		}
		// Follow the advertised continuation exactly, including grep and limit.
		_, continuation, found := strings.Cut(header, "continue with ")
		if !found {
			t.Fatalf("missing continuation in %q", header)
		}
		if err := json.Unmarshal([]byte(continuation), new(recallToolResultArgs)); err != nil {
			t.Fatalf("invalid continuation %q: %v", continuation, err)
		}
		args = continuation
	}
	t.Fatal("grep pagination did not finish")
}

func TestRecallToolResultGrepOffsetBounds(t *testing.T) {
	content := "one\nmatch 文🙂\nthree"
	full, _ := renderRecallBody(content, recallToolResultArgs{Grep: "match"})
	negative, _ := renderRecallBody(content, recallToolResultArgs{Grep: "match", Offset: -1})
	if negative != full {
		t.Fatal("negative offset should start at zero")
	}
	for _, offset := range []int{utf8.RuneCountInString(full), utf8.RuneCountInString(full) + 100} {
		body, note := renderRecallBody(content, recallToolResultArgs{Grep: "match", Offset: offset})
		if body != "" || !strings.Contains(note, "past the end") {
			t.Fatalf("out-of-range offset %d returned body=%q note=%q", offset, body, note)
		}
	}
}

func TestRecallToolResultListsWhenRefOmitted(t *testing.T) {
	st := &fakeToolRecallStore{refs: []store.ToolMsgRef{
		{Seq: 3, Name: "exec", Chars: 4200},
		{Seq: 9, Name: "read_file", Chars: 800},
	}}
	got, err := callRecall(t, newRecallRegistry(t, st), `{}`)
	if err != nil {
		t.Fatalf("recall tool result list: %v", err)
	}
	assertToolOutputContains(t, got, "[Recallable Tool Results]", "3\texec\t4200", "9\tread_file\t800")
}

func TestRecallToolResultListFiltersByTool(t *testing.T) {
	st := &fakeToolRecallStore{refs: []store.ToolMsgRef{
		{Seq: 3, Name: "exec", Chars: 4200},
		{Seq: 9, Name: "read_file", Chars: 800},
	}}
	got, err := callRecall(t, newRecallRegistry(t, st), `{"tool":"read_file"}`)
	if err != nil {
		t.Fatalf("recall tool result list: %v", err)
	}
	if strings.Contains(got, "exec") {
		t.Fatalf("tool filter did not apply: %q", got)
	}
	assertToolOutputContains(t, got, "9\tread_file\t800")
}

func TestRecallToolResultUnavailableWithoutStore(t *testing.T) {
	r := NewRegistry("", "")
	r.SetOwnerUserID("user-a")
	r.SetRecallSessionKey("session-a")
	if _, err := callRecall(t, r, `{"ref":1}`); err == nil {
		t.Fatal("expected an error when no recall store is installed")
	}
}

func TestRecallToolResultReplacesArchiveTools(t *testing.T) {
	r := NewRegistry("", "")
	for _, reg := range []*Registry{r, r.ForTurn()} {
		if reg.GetFunc("recall_tool_result") == nil {
			t.Fatal("database recall tool is missing")
		}
		for _, obsolete := range []string{"retrieve_compacted_tool_result", "memory_search"} {
			if reg.GetFunc(obsolete) != nil {
				t.Fatalf("obsolete archive tool is still registered: %s", obsolete)
			}
		}
	}
}
