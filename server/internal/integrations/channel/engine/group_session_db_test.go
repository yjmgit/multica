package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/integrations/channel"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestGroupSessionIdleBoundaryAndPendingWork(t *testing.T) {
	pool := sessionPersistenceTestDB(t)
	f := seedSessionPersistenceFixture(t, pool)
	ctx := context.Background()
	// One transaction fixes PostgreSQL now(), including the exact TTL boundary.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	q := db.New(tx)
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(want bool) {
		t.Helper()
		got, err := q.IsChannelSessionIdle(ctx, db.IsChannelSessionIdleParams{ChatSessionID: f.sessionID, IdleSeconds: 7200})
		if err != nil || got != want {
			t.Fatalf("idle=%v want=%v err=%v", got, want, err)
		}
	}
	exec(`UPDATE chat_session SET created_at=now()-interval '2 hours'+interval '1 microsecond' WHERE id=$1`, f.sessionID)
	expect(false)
	exec(`UPDATE chat_session SET created_at=now()-interval '2 hours' WHERE id=$1`, f.sessionID)
	expect(true)
	var taskID pgtype.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO agent_task_queue(agent_id,chat_session_id,status,completed_at) VALUES($1,$2,'completed',now()-interval '1 hour') RETURNING id`, f.agentID, f.sessionID).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	expect(false)
	exec(`UPDATE agent_task_queue SET completed_at=now()-interval '3 hours' WHERE id=$1`, taskID)
	expect(true)
	for _, status := range []string{"queued", "dispatched", "running", "waiting_local_directory", "deferred"} {
		exec(`UPDATE agent_task_queue SET status=$2 WHERE id=$1`, taskID, status)
		expect(false)
	}
	exec(`UPDATE agent_task_queue SET status='failed',completed_at=now() WHERE id=$1`, taskID)
	expect(false)
	exec(`UPDATE agent_task_queue SET status='completed',completed_at=now()-interval '3 hours' WHERE id=$1`, taskID)
	var messageID pgtype.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO chat_message(chat_session_id,role,content,channel_ingested,task_id,created_at) VALUES($1,'user','recent question',true,$2,now()-interval '1 hour') RETURNING id`, f.sessionID, taskID).Scan(&messageID); err != nil {
		t.Fatal(err)
	}
	expect(false)
	exec(`UPDATE chat_message SET created_at=now()-interval '3 hours' WHERE id=$1`, messageID)
	expect(true)
	exec(`UPDATE chat_message SET task_id=NULL WHERE id=$1`, messageID)
	expect(false)
}

func TestGroupSessionConcurrentExpiryAndRestart(t *testing.T) {
	pool := sessionPersistenceTestDB(t)
	f := seedSessionPersistenceFixture(t, pool)
	ctx := context.Background()
	q := db.New(pool)
	fx := testutil.New(pool, utilUUID(f.workspaceID), utilUUID(f.userID))
	fx.Exec(t, `UPDATE chat_session SET created_at=now()-interval '3 hours' WHERE id=$1`, f.sessionID)
	in := EnsureSessionInput{WorkspaceID: f.workspaceID, AgentID: f.agentID, InstallationID: f.installationID, Sender: f.userID, BindingKey: f.channelChatID, ChatType: channel.ChatTypeGroup, IdleTTL: 2 * time.Hour}
	ids := make(chan pgtype.UUID, 12)
	errs := make(chan error, 12)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := NewChatSession(q, pool, channel.Type("lark"), SessionTitles{})
			for attempt := 0; attempt < 12; attempt++ {
				id, err := s.EnsureSession(ctx, in)
				if errors.Is(err, ErrRouteChanged) {
					continue
				}
				ids <- id
				errs <- err
				return
			}
			errs <- ErrRouteChanged
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var current pgtype.UUID
	for id := range ids {
		if id == f.sessionID {
			t.Fatal("expired session reused")
		}
		if current.Valid && id != current {
			t.Fatal("concurrent requests created different current sessions")
		}
		current = id
	}
	if got := fx.Count(t, `SELECT count(*) FROM channel_chat_session_binding WHERE installation_id=$1 AND channel_chat_id=$2`, f.installationID, f.channelChatID); got != 2 {
		t.Fatalf("route count=%d", got)
	}
	// Rebuilding the service models process restart; persistence controls reuse.
	restarted := NewChatSession(db.New(pool), pool, channel.Type("lark"), SessionTitles{})
	if got, err := restarted.EnsureSession(ctx, in); err != nil || got != current {
		t.Fatal("restart lost current route", got, err)
	}
	fx.Exec(t, `UPDATE chat_session SET created_at=now()-interval '3 hours' WHERE id=$1`, current)
	in.IdleTTL = 0
	if got, err := restarted.EnsureSession(ctx, in); err != nil || got != current {
		t.Fatal("TTL zero did not disable expiry", err)
	}
}

func TestGroupSessionNewWaitsForOwnPredecessorOnly(t *testing.T) {
	pool := sessionPersistenceTestDB(t)
	f := seedSessionPersistenceFixture(t, pool)
	ctx := context.Background()
	q := db.New(pool)
	fx := testutil.New(pool, utilUUID(f.workspaceID), utilUUID(f.userID))
	fx.Exec(t, `UPDATE channel_chat_session_binding SET channel_type='feishu',config='{"chat_id":"oc_test","member_open_id":"ou_a"}' WHERE chat_session_id=$1`, f.sessionID)
	s := NewChatSession(q, pool, channel.TypeFeishu, SessionTitles{})
	base := EnsureSessionInput{WorkspaceID: f.workspaceID, AgentID: f.agentID, InstallationID: f.installationID, Sender: f.userID, BindingKey: f.channelChatID, ChatType: channel.ChatTypeGroup, BindingConfig: []byte(`{"chat_id":"oc_test","member_open_id":"ou_a"}`)}
	agent, err := q.GetAgent(ctx, f.agentID)
	if err != nil {
		t.Fatal(err)
	}
	fx.Exec(t, `UPDATE agent_runtime SET status='online',last_seen_at=now() WHERE id=$1`, agent.RuntimeID)
	oldTask := fx.Task(t, utilUUID(f.agentID), testutil.Cols{"chat_session_id": utilUUID(f.sessionID), "runtime_id": utilUUID(agent.RuntimeID), "status": "running"})
	started, err := s.StartSession(ctx, StartSessionInput{EnsureSessionInput: base, MessageID: "om_new", SenderChannelID: "ou_a"})
	if err != nil {
		t.Fatal(err)
	}
	newTask := fx.Task(t, utilUUID(f.agentID), testutil.Cols{"chat_session_id": utilUUID(started.SessionID), "runtime_id": utilUUID(agent.RuntimeID), "priority": 100})
	base.BindingKey = "member:oc_test:ou_b"
	base.BindingConfig = []byte(`{"chat_id":"oc_test","member_open_id":"ou_b"}`)
	other, err := s.EnsureSession(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	otherTask := fx.Task(t, utilUUID(f.agentID), testutil.Cols{"chat_session_id": utilUUID(other), "runtime_id": utilUUID(agent.RuntimeID)})
	claim := db.ClaimAgentTaskParams{AgentID: f.agentID, RuntimeID: agent.RuntimeID, RuntimeStaleSecs: 600, PrepareLeaseSecs: 30}
	got, err := q.ClaimAgentTask(ctx, claim)
	if err != nil || utilUUID(got.ID) != otherTask {
		t.Fatal("new route bypassed old task or blocked other member", got.ID, err)
	}
	fx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, otherTask)
	for _, status := range []string{"running", "deferred"} {
		fx.Exec(t, `UPDATE agent_task_queue SET status=$2 WHERE id=$1`, oldTask, status)
		pending, err := q.HasPendingChannelPredecessor(ctx, started.SessionID)
		if err != nil || !pending {
			t.Fatal("lost predecessor", status, err)
		}
		if _, err := q.ClaimAgentTask(ctx, claim); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatal("new task ran before predecessor", status, err)
		}
	}
	fx.Exec(t, `UPDATE agent_task_queue SET status='completed',completed_at=now() WHERE id=$1`, oldTask)
	got, err = q.ClaimAgentTask(ctx, claim)
	if err != nil || got.ID != util.MustParseUUID(newTask) {
		t.Fatal("new task did not resume", got.ID, err)
	}
}

func TestGroupSessionConcurrentManualNewStaysFresh(t *testing.T) {
	pool := sessionPersistenceTestDB(t)
	f := seedSessionPersistenceFixture(t, pool)
	ctx := context.Background()
	q := db.New(pool)
	fx := testutil.New(pool, utilUUID(f.workspaceID), utilUUID(f.userID))
	fx.Exec(t, `UPDATE chat_session SET session_id='old-claude-session' WHERE id=$1`, f.sessionID)
	s := NewChatSession(q, pool, channel.TypeFeishu, SessionTitles{})
	in := StartSessionInput{EnsureSessionInput: EnsureSessionInput{WorkspaceID: f.workspaceID, AgentID: f.agentID, InstallationID: f.installationID, Sender: f.userID, BindingKey: f.channelChatID, ChatType: channel.ChatTypeGroup}}
	results := make(chan StartSessionResult, 8)
	errs := make(chan error, 8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < maxRouteChangeRetries; attempt++ {
				r, e := s.StartSession(ctx, in)
				if errors.Is(e, ErrRouteChanged) {
					continue
				}
				results <- r
				errs <- e
				return
			}
			errs <- ErrRouteChanged
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	ids := map[pgtype.UUID]bool{}
	for r := range results {
		if ids[r.SessionID] {
			t.Fatal("different commands reused one new session")
		}
		ids[r.SessionID] = true
		cs, e := q.GetChatSession(ctx, r.SessionID)
		if e != nil || cs.SessionID.Valid {
			t.Fatal("new session inherited provider pointer", e)
		}
		if _, e = q.GetLastChatTaskSession(ctx, db.GetLastChatTaskSessionParams{ChatSessionID: r.SessionID}); !errors.Is(e, pgx.ErrNoRows) {
			t.Fatal("new session resumed old task history", e)
		}
	}
	if got := fx.Count(t, `SELECT count(*) FROM channel_chat_session_binding WHERE installation_id=$1 AND channel_chat_id=$2 AND retired_at IS NULL`, f.installationID, f.channelChatID); got != 1 {
		t.Fatalf("current route count=%d", got)
	}
}
