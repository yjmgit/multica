package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestHTTPClientUploadFileSendsMultipartAndReturnsKey(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("t-1", 7200)
	fake.mux.HandleFunc("/open-apis/im/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer t-1" {
			t.Errorf("auth = %q", got)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if got := r.FormValue("file_type"); got != "pdf" {
			t.Errorf("file_type = %q, want pdf", got)
		}
		if got := r.FormValue("file_name"); got != "report.pdf" {
			t.Errorf("file_name = %q", got)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("form file: %v", err)
		}
		data, _ := io.ReadAll(f)
		if string(data) != "%PDF-1.4" {
			t.Errorf("file body = %q", data)
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"file_key": "file_v2_x"}})
	})
	c := newTestClient(fake, time.Now)
	key, err := c.UploadFile(context.Background(), testCreds(), "report.pdf", []byte("%PDF-1.4"))
	if err != nil {
		t.Fatalf("UploadFile: %v", err)
	}
	if key != "file_v2_x" {
		t.Fatalf("key = %q", key)
	}
}

func TestHTTPClientUploadImageRefreshesRejectedTokenOnce(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("t-1", 7200)
	var calls atomic.Int32
	fake.mux.HandleFunc("/open-apis/im/v1/images", func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, map[string]any{"code": codeTenantTokenInvalid, "msg": "token expired"})
			return
		}
		if got := r.FormValue("image_type"); got != "message" {
			t.Errorf("image_type = %q", got)
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"image_key": "img_v2_x"}})
	})
	c := newTestClient(fake, time.Now)
	key, err := c.UploadImage(context.Background(), testCreds(), "a.png", []byte("png"))
	if err != nil {
		t.Fatalf("UploadImage: %v", err)
	}
	if key != "img_v2_x" || calls.Load() != 2 || fake.tokenN.Load() != 2 {
		t.Fatalf("key=%q calls=%d tokenMints=%d, want img_v2_x/2/2", key, calls.Load(), fake.tokenN.Load())
	}
}

func TestHTTPClientUploadRejectsOversizedFiles(t *testing.T) {
	c := newTestClient(newLarkFake(t), time.Now)
	if _, err := c.UploadImage(context.Background(), testCreds(), "big.png", make([]byte, maxMessageImageBytes+1)); err == nil {
		t.Fatal("oversized image: want error")
	}
	if _, err := c.UploadFile(context.Background(), testCreds(), "big.bin", make([]byte, maxMessageFileBytes+1)); err == nil {
		t.Fatal("oversized file: want error")
	}
}

func TestHTTPClientSendMessageTargets(t *testing.T) {
	for _, tc := range []struct {
		name       string
		target     MessageTarget
		wantPath   string
		wantIDType string
		wantRecv   string
	}{
		{name: "chat", target: MessageTarget{ChatID: "oc_1"}, wantPath: "/open-apis/im/v1/messages", wantIDType: "chat_id", wantRecv: "oc_1"},
		{name: "user", target: MessageTarget{OpenID: "ou_1"}, wantPath: "/open-apis/im/v1/messages", wantIDType: "open_id", wantRecv: "ou_1"},
		{name: "reply", target: MessageTarget{ChatID: "oc_1", Reply: ReplyTarget{MessageID: "om_1", InThread: true}}, wantPath: "/open-apis/im/v1/messages/om_1/reply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := newLarkFake(t)
			fake.stubToken("t-1", 7200)
			handler := func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, tc.wantPath)
				}
				if got := r.URL.Query().Get("receive_id_type"); got != tc.wantIDType {
					t.Errorf("receive_id_type = %q, want %q", got, tc.wantIDType)
				}
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				if tc.wantRecv != "" && body["receive_id"] != tc.wantRecv {
					t.Errorf("receive_id = %v", body["receive_id"])
				}
				if body["msg_type"] != "file" {
					t.Errorf("msg_type = %v", body["msg_type"])
				}
				writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"message_id": "om_new"}})
			}
			fake.mux.HandleFunc("/open-apis/im/v1/messages", handler)
			fake.mux.HandleFunc("/open-apis/im/v1/messages/om_1/reply", handler)
			c := newTestClient(fake, time.Now)
			id, err := c.SendMessage(context.Background(), testCreds(), tc.target, "file", `{"file_key":"k"}`)
			if err != nil || id != "om_new" {
				t.Fatalf("SendMessage = %q, %v", id, err)
			}
		})
	}
}

func TestHTTPClientReadsDocWikiAndSheet(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("t-1", 7200)
	fake.mux.HandleFunc("/open-apis/wiki/v2/spaces/get_node", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("token") != "wikcn1" {
			t.Errorf("token = %q", r.URL.Query().Get("token"))
		}
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"node": map[string]any{"obj_type": "docx", "obj_token": "doxcn1", "title": "Plan"}}})
	})
	fake.mux.HandleFunc("/open-apis/docx/v1/documents/doxcn1/raw_content", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"content": "hello doc"}})
	})
	fake.mux.HandleFunc("/open-apis/sheets/v3/spreadsheets/shtcn1/sheets/query", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"sheets": []any{map[string]any{"sheet_id": "s1", "title": "Sheet1"}}}})
	})
	fake.mux.HandleFunc("/open-apis/sheets/v2/spreadsheets/shtcn1/values/s1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"valueRange": map[string]any{
			"values": []any{[]any{"name", "qty"}, []any{"apple", 3.0}, []any{[]any{map[string]any{"text": "link"}}, nil}},
		}}})
	})
	c := newTestClient(fake, time.Now)
	ctx := context.Background()
	node, err := c.GetWikiNode(ctx, testCreds(), "wikcn1")
	if err != nil || node.ObjToken != "doxcn1" || node.Title != "Plan" {
		t.Fatalf("GetWikiNode = %+v, %v", node, err)
	}
	content, err := c.GetDocRawContent(ctx, testCreds(), "doxcn1")
	if err != nil || content != "hello doc" {
		t.Fatalf("GetDocRawContent = %q, %v", content, err)
	}
	sheets, err := c.ListSheets(ctx, testCreds(), "shtcn1")
	if err != nil || len(sheets) != 1 || sheets[0].SheetID != "s1" {
		t.Fatalf("ListSheets = %+v, %v", sheets, err)
	}
	rows, err := c.GetSheetValues(ctx, testCreds(), "shtcn1", "s1")
	if err != nil {
		t.Fatalf("GetSheetValues: %v", err)
	}
	want := [][]string{{"name", "qty"}, {"apple", "3"}, {"link", ""}}
	if len(rows) != len(want) {
		t.Fatalf("rows = %v", rows)
	}
	for i := range want {
		for j := range want[i] {
			if rows[i][j] != want[i][j] {
				t.Fatalf("rows = %v, want %v", rows, want)
			}
		}
	}
}

func TestHTTPClientChatManagement(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("t-1", 7200)
	fake.mux.HandleFunc("/open-apis/im/v1/chats", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if r.URL.Query().Get("user_id_type") != "open_id" {
				t.Errorf("user_id_type = %q", r.URL.Query().Get("user_id_type"))
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["name"] != "Team" || len(body["user_id_list"].([]any)) != 2 {
				t.Errorf("create body = %v", body)
			}
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"chat_id": "oc_new", "name": "Team"}})
		case http.MethodGet:
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"items": []any{map[string]any{"chat_id": "oc_1", "name": "A"}}, "has_more": false,
			}})
		}
	})
	fake.mux.HandleFunc("/open-apis/im/v1/chats/oc_new/members", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{"invalid_id_list": []any{"ou_bad"}}})
		case http.MethodGet:
			writeJSON(w, map[string]any{"code": 0, "data": map[string]any{
				"items": []any{map[string]any{"member_id": "ou_1", "name": "Wang"}}, "has_more": true, "page_token": "p2",
			}})
		}
	})
	c := newTestClient(fake, time.Now)
	ctx := context.Background()
	chat, err := c.CreateChat(ctx, testCreds(), CreateChatParams{Name: "Team", Members: []string{"ou_1", "ou_2"}})
	if err != nil || chat.ChatID != "oc_new" {
		t.Fatalf("CreateChat = %+v, %v", chat, err)
	}
	invalid, err := c.AddChatMembers(ctx, testCreds(), "oc_new", []string{"ou_1", "ou_bad"})
	if err != nil || len(invalid) != 1 || invalid[0] != "ou_bad" {
		t.Fatalf("AddChatMembers = %v, %v", invalid, err)
	}
	chats, next, err := c.ListChats(ctx, testCreds(), "")
	if err != nil || len(chats) != 1 || next != "" {
		t.Fatalf("ListChats = %v, %q, %v", chats, next, err)
	}
	members, next, err := c.ListChatMembers(ctx, testCreds(), "oc_new", "")
	if err != nil || len(members) != 1 || members[0].OpenID != "ou_1" || next != "p2" {
		t.Fatalf("ListChatMembers = %v, %q, %v", members, next, err)
	}
}

func TestHTTPClientSurfacesBusinessErrors(t *testing.T) {
	fake := newLarkFake(t)
	fake.stubToken("t-1", 7200)
	fake.mux.HandleFunc("/open-apis/docx/v1/documents/doxcn1/raw_content", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"code": 1770032, "msg": "forbidden"})
	})
	c := newTestClient(fake, time.Now)
	_, err := c.GetDocRawContent(context.Background(), testCreds(), "doxcn1")
	if larkErrorCode(err) != 1770032 {
		t.Fatalf("err = %v, want code 1770032", err)
	}
}

func TestMessageFileType(t *testing.T) {
	for name, want := range map[string]string{
		"a.PDF": "pdf", "b.docx": "doc", "c.xlsx": "xls", "d.pptx": "ppt", "e.mp4": "mp4", "f.opus": "opus", "g.zip": "stream", "noext": "stream",
	} {
		if got := messageFileType(name); got != want {
			t.Errorf("messageFileType(%q) = %q, want %q", name, got, want)
		}
	}
}
