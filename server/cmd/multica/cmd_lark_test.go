package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newLarkSendTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "send"}
	addLarkSendFlags(cmd)
	return cmd
}

func TestRunLarkSendPostsMultipartForm(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(filePath, []byte("%PDF-1.4"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/lark/send" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		form := r.MultipartForm.Value
		if got := form["text"]; len(got) != 1 || got[0] != "见附件" {
			t.Errorf("text = %v", got)
		}
		if got := form["chat_id"]; len(got) != 1 || got[0] != "oc_1" {
			t.Errorf("chat_id = %v", got)
		}
		if got := form["mention"]; len(got) != 2 {
			t.Errorf("mention = %v", got)
		}
		if got := form["mention_requester"]; len(got) != 1 || got[0] != "true" {
			t.Errorf("mention_requester = %v", got)
		}
		files := r.MultipartForm.File["file"]
		if len(files) != 1 || files[0].Filename != "report.pdf" {
			t.Fatalf("files = %v", files)
		}
		f, _ := files[0].Open()
		data, _ := io.ReadAll(f)
		if string(data) != "%PDF-1.4" {
			t.Errorf("file data = %q", data)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message_ids": []string{"om_1", "om_2"}})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")

	cmd := newLarkSendTestCmd()
	for k, v := range map[string]string{"file": filePath, "chat": "oc_1", "mention-requester": "true"} {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	_ = cmd.Flags().Set("mention", "ou_a")
	_ = cmd.Flags().Set("mention", "ou_b")

	out, err := captureStdout(t, func() error { return runLarkSend(cmd, []string{"见附件"}) })
	if err != nil {
		t.Fatalf("runLarkSend: %v", err)
	}
	if !strings.Contains(out, "om_2") {
		t.Fatalf("stdout = %q", out)
	}
}

func TestRunLarkSendRequiresContent(t *testing.T) {
	if err := runLarkSend(newLarkSendTestCmd(), nil); err == nil || !strings.Contains(err.Error(), "nothing to send") {
		t.Fatalf("err = %v", err)
	}
}

func newLarkWakeupTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "wakeup"}
	addLarkWakeupFlags(cmd)
	return cmd
}

// A recurring wake-up is a run_only autopilot for this agent plus a schedule
// trigger, and its prompt tells the run how to post back to the chat.
func TestRunLarkWakeupRecurringCreatesAutopilotAndTrigger(t *testing.T) {
	var created, trigger map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/lark/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"current_chat": map[string]any{"chat_id": "oc_g", "requester_open_id": "ou_asker"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/autopilots":
			_ = json.NewDecoder(r.Body).Decode(&created)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ap-1"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/autopilots/ap-1/triggers":
			_ = json.NewDecoder(r.Body).Decode(&trigger)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "tr-1", "next_run_at": "2026-09-28T01:00:00Z"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")

	cmd := newLarkWakeupTestCmd()
	_ = cmd.Flags().Set("cron", "0 9 * * 1-5")
	_ = cmd.Flags().Set("mention-requester", "true")
	out, err := captureStdout(t, func() error { return runLarkWakeup(cmd, []string{"汇总昨天的讨论"}) })
	if err != nil {
		t.Fatalf("runLarkWakeup: %v", err)
	}
	if created["assignee_id"] != "agent-1" || created["execution_mode"] != "run_only" || created["title"] != "汇总昨天的讨论" {
		t.Fatalf("autopilot body = %v", created)
	}
	desc, _ := created["description"].(string)
	if !strings.HasPrefix(desc, "汇总昨天的讨论") || !strings.Contains(desc, "multica lark send --chat oc_g --mention ou_asker") {
		t.Fatalf("description = %q", desc)
	}
	if trigger["kind"] != "schedule" || trigger["cron_expression"] != "0 9 * * 1-5" || trigger["timezone"] != "Asia/Singapore" {
		t.Fatalf("trigger body = %v", trigger)
	}
	if !strings.Contains(out, "ap-1") || !strings.Contains(out, "2026-09-28T01:00:00Z") {
		t.Fatalf("stdout = %q", out)
	}
}

// A one-off wake-up whose scheduling fails must not leave an orphan autopilot.
func TestRunLarkWakeupOnceDeletesAutopilotWhenSchedulingFails(t *testing.T) {
	deleted := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/lark/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"current_chat": map[string]any{"chat_id": "oc_g"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/autopilots":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ap-2"})
		case r.URL.Path == "/api/lark/wakeups":
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "the wake-up time must be in the future"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/autopilots/ap-2":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")

	cmd := newLarkWakeupTestCmd()
	_ = cmd.Flags().Set("at", "2020-01-01T00:00:00+08:00")
	if err := runLarkWakeup(cmd, []string{"x"}); err == nil {
		t.Fatal("want error")
	}
	if !deleted {
		t.Fatal("the autopilot was not deleted after scheduling failed")
	}
}

func TestRunLarkWakeupNeedsExactlyOneTime(t *testing.T) {
	cmd := newLarkWakeupTestCmd()
	if err := runLarkWakeup(cmd, []string{"x"}); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("err = %v", err)
	}
}

func newLarkDelegateTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "delegate"}
	cmd.Flags().String("to", "", "")
	cmd.Flags().String("title", "", "")
	return cmd
}

// Delegating creates an issue for the other agent and registers the relay
// that posts its results back to this Feishu chat.
func TestRunLarkDelegateCreatesIssueAndRelay(t *testing.T) {
	const helper = "11111111-2222-4333-8444-555555555555"
	var issueBody, relayBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/lark/context":
			_ = json.NewEncoder(w).Encode(map[string]any{"current_chat": map[string]any{"chat_id": "oc_g"}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/issues":
			_ = json.NewDecoder(r.Body).Decode(&issueBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "issue-1", "identifier": "DATA-7"})
		case r.Method == http.MethodPost && r.URL.Path == "/api/lark/relays":
			_ = json.NewDecoder(r.Body).Decode(&relayBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"issue_id": "issue-1", "chat_id": "oc_g"})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	t.Setenv("MULTICA_AGENT_ID", "agent-1")

	cmd := newLarkDelegateTestCmd()
	_ = cmd.Flags().Set("to", helper)
	out, err := captureStdout(t, func() error { return runLarkDelegate(cmd, []string{"清洗销售表\n去重并统一日期格式"}) })
	if err != nil {
		t.Fatalf("runLarkDelegate: %v", err)
	}
	if issueBody["assignee_type"] != "agent" || issueBody["assignee_id"] != helper || issueBody["title"] != "清洗销售表" {
		t.Fatalf("issue body = %v", issueBody)
	}
	if desc, _ := issueBody["description"].(string); !strings.Contains(desc, "relayed to that conversation") {
		t.Fatalf("description = %q", desc)
	}
	if relayBody["issue_id"] != "issue-1" || !strings.Contains(out, "DATA-7") || !strings.Contains(out, "oc_g") {
		t.Fatalf("relay body = %v, stdout = %q", relayBody, out)
	}
}

func TestRunLarkDelegateRequiresFeishuChat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"bot_open_id": "ou_bot"})
	}))
	defer srv.Close()
	setCLITestServerEnv(t, srv.URL)
	t.Setenv("MULTICA_TOKEN", "mat_test-token")
	cmd := newLarkDelegateTestCmd()
	_ = cmd.Flags().Set("to", "11111111-2222-4333-8444-555555555555")
	if err := runLarkDelegate(cmd, []string{"x"}); err == nil || !strings.Contains(err.Error(), "Feishu conversation") {
		t.Fatalf("err = %v", err)
	}
}
