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
