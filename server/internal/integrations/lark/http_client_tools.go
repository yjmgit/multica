package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// ToolAPIClient is the Lark Open Platform surface behind the agent-facing
// Feishu tools (`multica lark ...`): sending files and images, reading docs,
// and managing group chats. It is separate from APIClient so the inbound
// pipeline's test fakes do not have to implement calls they never make; the
// production httpAPIClient implements both.
type ToolAPIClient interface {
	// UploadImage uploads an image for use in a message and returns its
	// image_key. Lark caps message images at 10 MiB.
	UploadImage(ctx context.Context, creds InstallationCredentials, filename string, data []byte) (string, error)
	// UploadFile uploads a file for use in a message and returns its
	// file_key. Lark caps message files at 30 MiB.
	UploadFile(ctx context.Context, creds InstallationCredentials, filename string, data []byte) (string, error)
	// SendMessage posts one message of msgType with an already-encoded
	// content envelope and returns the new message_id.
	SendMessage(ctx context.Context, creds InstallationCredentials, target MessageTarget, msgType, content string) (string, error)
	// GetDocRawContent returns the plain-text body of a docx document.
	GetDocRawContent(ctx context.Context, creds InstallationCredentials, documentID string) (string, error)
	// GetWikiNode resolves a wiki node token to the document it wraps.
	GetWikiNode(ctx context.Context, creds InstallationCredentials, token string) (WikiNode, error)
	// CreateChat creates a group chat with the bot as owner and returns it.
	CreateChat(ctx context.Context, creds InstallationCredentials, p CreateChatParams) (ChatInfo, error)
	// AddChatMembers invites users (open_ids) into a chat the bot is in. It
	// returns the ids Lark rejected as invalid.
	AddChatMembers(ctx context.Context, creds InstallationCredentials, chatID string, openIDs []string) ([]string, error)
	// ListChats lists one page of the group chats the bot is a member of.
	ListChats(ctx context.Context, creds InstallationCredentials, pageToken string) ([]ChatInfo, string, error)
	// ListSheets lists a spreadsheet's sheets.
	ListSheets(ctx context.Context, creds InstallationCredentials, spreadsheetToken string) ([]SheetInfo, error)
	// GetSheetValues returns one sheet's cell values rendered as strings.
	GetSheetValues(ctx context.Context, creds InstallationCredentials, spreadsheetToken, sheetID string) ([][]string, error)
	// GetLegacyDocRawContent returns the plain-text body of a legacy (doc v2)
	// document.
	GetLegacyDocRawContent(ctx context.Context, creds InstallationCredentials, docToken string) (string, error)
	// ListBitableTables lists a base's tables.
	ListBitableTables(ctx context.Context, creds InstallationCredentials, appToken string) ([]BitableTable, error)
	// ListBitableFields lists a table's field names in display order.
	ListBitableFields(ctx context.Context, creds InstallationCredentials, appToken, tableID string) ([]string, error)
	// ListBitableRecords lists one page of a table's records.
	ListBitableRecords(ctx context.Context, creds InstallationCredentials, appToken, tableID, pageToken string) ([]map[string]any, string, error)
	// CreateDocFromMarkdown creates a docx document owned by the app and
	// fills it with markdown converted to document blocks. It returns the
	// document id.
	CreateDocFromMarkdown(ctx context.Context, creds InstallationCredentials, title, markdown string) (string, error)
	// ShareDoc grants each open_id perm ("view", "edit" or "full_access") on a
	// docx document and, when linkShare is non-empty, sets its link sharing
	// (e.g. "tenant_readable").
	ShareDoc(ctx context.Context, creds InstallationCredentials, documentID string, openIDs []string, perm, linkShare string) error
	// DocURL returns the browser URL of a docx document.
	DocURL(ctx context.Context, creds InstallationCredentials, documentID string) (string, error)
	// ListChatMembers lists one page of a chat's human members.
	ListChatMembers(ctx context.Context, creds InstallationCredentials, chatID, pageToken string) ([]ChatMember, string, error)
}

// Lark upload limits for message resources.
const (
	maxMessageImageBytes = 10 << 20
	maxMessageFileBytes  = 30 << 20
)

// MessageTarget addresses an outbound tool message. Exactly one of ChatID
// (a group or p2p chat) or OpenID (a user, delivered as a bot DM) is set.
// Reply, when set, routes the message as a reply to that message instead.
type MessageTarget struct {
	ChatID string
	OpenID string
	Reply  ReplyTarget
}

// WikiNode is the document a wiki node points at.
type WikiNode struct {
	ObjType  string `json:"obj_type"`
	ObjToken string `json:"obj_token"`
	Title    string `json:"title"`
}

// CreateChatParams describes a new group chat. Members are open_ids.
type CreateChatParams struct {
	Name        string
	Description string
	Members     []string
}

// ChatInfo is a group chat as the tools report it.
type ChatInfo struct {
	ChatID      string `json:"chat_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SheetInfo is one sheet of a spreadsheet.
type SheetInfo struct {
	SheetID string `json:"sheet_id"`
	Title   string `json:"title"`
}

// BitableTable is one table of a base (多维表格).
type BitableTable struct {
	TableID string `json:"table_id"`
	Name    string `json:"name"`
}

// ChatMember is one human member of a chat.
type ChatMember struct {
	OpenID string `json:"open_id"`
	Name   string `json:"name"`
}

// messageFileType maps a filename onto Lark's upload file_type enum. Lark
// renders opus/mp4 natively; everything else is a generic attachment, and the
// office/pdf types only change the icon.
func messageFileType(filename string) string {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".opus":
		return "opus"
	case ".mp4":
		return "mp4"
	case ".pdf":
		return "pdf"
	case ".doc", ".docx":
		return "doc"
	case ".xls", ".xlsx":
		return "xls"
	case ".ppt", ".pptx":
		return "ppt"
	default:
		return "stream"
	}
}

// IsMessageImage reports whether a content type is one Lark accepts as a
// message image; anything else is sent as a file.
func IsMessageImage(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "image/jpeg", "image/png", "image/gif", "image/webp", "image/bmp", "image/tiff", "image/x-icon", "image/vnd.microsoft.icon":
		return true
	}
	return false
}

func (c *httpAPIClient) UploadImage(ctx context.Context, creds InstallationCredentials, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("lark http client: empty image")
	}
	if len(data) > maxMessageImageBytes {
		return "", fmt.Errorf("lark http client: image %s is %d bytes, over Lark's 10 MiB limit", filename, len(data))
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ImageKey string `json:"image_key"`
		} `json:"data"`
	}
	fields := map[string]string{"image_type": "message"}
	if err := c.doAuthedMultipart(ctx, creds, "/open-apis/im/v1/images", fields, "image", filename, data, &resp); err != nil {
		return "", fmt.Errorf("lark http client: upload image: %w", err)
	}
	if resp.Code != 0 || resp.Data.ImageKey == "" {
		return "", &APIError{Op: "upload image", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.ImageKey, nil
}

func (c *httpAPIClient) UploadFile(ctx context.Context, creds InstallationCredentials, filename string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", errors.New("lark http client: empty file")
	}
	if len(data) > maxMessageFileBytes {
		return "", fmt.Errorf("lark http client: file %s is %d bytes, over Lark's 30 MiB limit", filename, len(data))
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FileKey string `json:"file_key"`
		} `json:"data"`
	}
	fields := map[string]string{"file_type": messageFileType(filename), "file_name": filename}
	if err := c.doAuthedMultipart(ctx, creds, "/open-apis/im/v1/files", fields, "file", filename, data, &resp); err != nil {
		return "", fmt.Errorf("lark http client: upload file: %w", err)
	}
	if resp.Code != 0 || resp.Data.FileKey == "" {
		return "", &APIError{Op: "upload file", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.FileKey, nil
}

func (c *httpAPIClient) SendMessage(ctx context.Context, creds InstallationCredentials, target MessageTarget, msgType, content string) (string, error) {
	var path string
	var body map[string]any
	switch {
	case target.Reply.IsSet():
		path, body = outboundMessageRequest("", msgType, content, target.Reply)
	case target.ChatID != "":
		path, body = outboundMessageRequest(ChatID(target.ChatID), msgType, content, ReplyTarget{})
	case target.OpenID != "":
		q := url.Values{}
		q.Set("receive_id_type", "open_id")
		path = "/open-apis/im/v1/messages?" + q.Encode()
		body = map[string]any{"receive_id": target.OpenID, "msg_type": msgType, "content": content}
	default:
		return "", errors.New("lark http client: message target has no chat_id or open_id")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			MessageID string `json:"message_id"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, body, &resp); err != nil {
		return "", fmt.Errorf("lark http client: send %s message: %w", msgType, err)
	}
	if resp.Code != 0 || resp.Data.MessageID == "" {
		return "", &APIError{Op: "send " + msgType + " message", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.MessageID, nil
}

func (c *httpAPIClient) GetDocRawContent(ctx context.Context, creds InstallationCredentials, documentID string) (string, error) {
	if documentID == "" {
		return "", errors.New("lark http client: missing document id")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	path := "/open-apis/docx/v1/documents/" + url.PathEscape(documentID) + "/raw_content"
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return "", fmt.Errorf("lark http client: read document: %w", err)
	}
	if resp.Code != 0 {
		return "", &APIError{Op: "read document", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Content, nil
}

func (c *httpAPIClient) GetWikiNode(ctx context.Context, creds InstallationCredentials, token string) (WikiNode, error) {
	if token == "" {
		return WikiNode{}, errors.New("lark http client: missing wiki token")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Node WikiNode `json:"node"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("token", token)
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, "/open-apis/wiki/v2/spaces/get_node?"+q.Encode(), nil, &resp); err != nil {
		return WikiNode{}, fmt.Errorf("lark http client: resolve wiki node: %w", err)
	}
	if resp.Code != 0 {
		return WikiNode{}, &APIError{Op: "resolve wiki node", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Node, nil
}

func (c *httpAPIClient) CreateChat(ctx context.Context, creds InstallationCredentials, p CreateChatParams) (ChatInfo, error) {
	if strings.TrimSpace(p.Name) == "" {
		return ChatInfo{}, errors.New("lark http client: missing chat name")
	}
	body := map[string]any{
		"name":      p.Name,
		"chat_mode": "group",
		"chat_type": "private",
	}
	if p.Description != "" {
		body["description"] = p.Description
	}
	if len(p.Members) > 0 {
		body["user_id_list"] = p.Members
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ChatID      string `json:"chat_id"`
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("user_id_type", "open_id")
	q.Set("set_bot_manager", "true")
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, "/open-apis/im/v1/chats?"+q.Encode(), body, &resp); err != nil {
		return ChatInfo{}, fmt.Errorf("lark http client: create chat: %w", err)
	}
	if resp.Code != 0 || resp.Data.ChatID == "" {
		return ChatInfo{}, &APIError{Op: "create chat", Code: resp.Code, Msg: resp.Msg}
	}
	return ChatInfo{ChatID: resp.Data.ChatID, Name: resp.Data.Name, Description: resp.Data.Description}, nil
}

func (c *httpAPIClient) AddChatMembers(ctx context.Context, creds InstallationCredentials, chatID string, openIDs []string) ([]string, error) {
	if chatID == "" {
		return nil, errors.New("lark http client: missing chat_id")
	}
	if len(openIDs) == 0 {
		return nil, errors.New("lark http client: no members to add")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			InvalidIDList []string `json:"invalid_id_list"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("member_id_type", "open_id")
	path := "/open-apis/im/v1/chats/" + url.PathEscape(chatID) + "/members?" + q.Encode()
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, map[string]any{"id_list": openIDs}, &resp); err != nil {
		return nil, fmt.Errorf("lark http client: add chat members: %w", err)
	}
	if resp.Code != 0 {
		return nil, &APIError{Op: "add chat members", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.InvalidIDList, nil
}

func (c *httpAPIClient) ListChats(ctx context.Context, creds InstallationCredentials, pageToken string) ([]ChatInfo, string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items     []ChatInfo `json:"items"`
			PageToken string     `json:"page_token"`
			HasMore   bool       `json:"has_more"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("page_size", "100")
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, "/open-apis/im/v1/chats?"+q.Encode(), nil, &resp); err != nil {
		return nil, "", fmt.Errorf("lark http client: list chats: %w", err)
	}
	if resp.Code != 0 {
		return nil, "", &APIError{Op: "list chats", Code: resp.Code, Msg: resp.Msg}
	}
	next := ""
	if resp.Data.HasMore {
		next = resp.Data.PageToken
	}
	return resp.Data.Items, next, nil
}

func (c *httpAPIClient) ListChatMembers(ctx context.Context, creds InstallationCredentials, chatID, pageToken string) ([]ChatMember, string, error) {
	if chatID == "" {
		return nil, "", errors.New("lark http client: missing chat_id")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				MemberID string `json:"member_id"`
				Name     string `json:"name"`
			} `json:"items"`
			PageToken string `json:"page_token"`
			HasMore   bool   `json:"has_more"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("member_id_type", "open_id")
	q.Set("page_size", "100")
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	path := "/open-apis/im/v1/chats/" + url.PathEscape(chatID) + "/members?" + q.Encode()
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return nil, "", fmt.Errorf("lark http client: list chat members: %w", err)
	}
	if resp.Code != 0 {
		return nil, "", &APIError{Op: "list chat members", Code: resp.Code, Msg: resp.Msg}
	}
	members := make([]ChatMember, 0, len(resp.Data.Items))
	for _, it := range resp.Data.Items {
		members = append(members, ChatMember{OpenID: it.MemberID, Name: it.Name})
	}
	next := ""
	if resp.Data.HasMore {
		next = resp.Data.PageToken
	}
	return members, next, nil
}

func (c *httpAPIClient) ListSheets(ctx context.Context, creds InstallationCredentials, spreadsheetToken string) ([]SheetInfo, error) {
	if spreadsheetToken == "" {
		return nil, errors.New("lark http client: missing spreadsheet token")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Sheets []SheetInfo `json:"sheets"`
		} `json:"data"`
	}
	path := "/open-apis/sheets/v3/spreadsheets/" + url.PathEscape(spreadsheetToken) + "/sheets/query"
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("lark http client: list sheets: %w", err)
	}
	if resp.Code != 0 {
		return nil, &APIError{Op: "list sheets", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Sheets, nil
}

func (c *httpAPIClient) GetSheetValues(ctx context.Context, creds InstallationCredentials, spreadsheetToken, sheetID string) ([][]string, error) {
	if spreadsheetToken == "" || sheetID == "" {
		return nil, errors.New("lark http client: missing spreadsheet token or sheet id")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			ValueRange struct {
				Values [][]any `json:"values"`
			} `json:"valueRange"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("valueRenderOption", "ToString")
	path := "/open-apis/sheets/v2/spreadsheets/" + url.PathEscape(spreadsheetToken) + "/values/" + url.PathEscape(sheetID) + "?" + q.Encode()
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("lark http client: read sheet: %w", err)
	}
	if resp.Code != 0 {
		return nil, &APIError{Op: "read sheet", Code: resp.Code, Msg: resp.Msg}
	}
	rows := make([][]string, 0, len(resp.Data.ValueRange.Values))
	for _, row := range resp.Data.ValueRange.Values {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = sheetCellString(v)
		}
		rows = append(rows, cells)
	}
	return rows, nil
}

func (c *httpAPIClient) GetLegacyDocRawContent(ctx context.Context, creds InstallationCredentials, docToken string) (string, error) {
	if docToken == "" {
		return "", errors.New("lark http client: missing doc token")
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Content string `json:"content"`
		} `json:"data"`
	}
	path := "/open-apis/doc/v2/" + url.PathEscape(docToken) + "/raw_content"
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return "", fmt.Errorf("lark http client: read legacy document: %w", err)
	}
	if resp.Code != 0 {
		return "", &APIError{Op: "read legacy document", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Content, nil
}

func (c *httpAPIClient) ListBitableTables(ctx context.Context, creds InstallationCredentials, appToken string) ([]BitableTable, error) {
	if appToken == "" {
		return nil, errors.New("lark http client: missing base token")
	}
	var out []BitableTable
	pageToken := ""
	for {
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items     []BitableTable `json:"items"`
				HasMore   bool           `json:"has_more"`
				PageToken string         `json:"page_token"`
			} `json:"data"`
		}
		q := url.Values{}
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		path := "/open-apis/bitable/v1/apps/" + url.PathEscape(appToken) + "/tables?" + q.Encode()
		if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("lark http client: list base tables: %w", err)
		}
		if resp.Code != 0 {
			return nil, &APIError{Op: "list base tables", Code: resp.Code, Msg: resp.Msg}
		}
		out = append(out, resp.Data.Items...)
		if !resp.Data.HasMore || resp.Data.PageToken == "" || len(out) >= 100 {
			return out, nil
		}
		pageToken = resp.Data.PageToken
	}
}

func (c *httpAPIClient) ListBitableFields(ctx context.Context, creds InstallationCredentials, appToken, tableID string) ([]string, error) {
	var out []string
	pageToken := ""
	for {
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
			Data struct {
				Items []struct {
					FieldName string `json:"field_name"`
				} `json:"items"`
				HasMore   bool   `json:"has_more"`
				PageToken string `json:"page_token"`
			} `json:"data"`
		}
		q := url.Values{}
		q.Set("page_size", "100")
		if pageToken != "" {
			q.Set("page_token", pageToken)
		}
		path := "/open-apis/bitable/v1/apps/" + url.PathEscape(appToken) + "/tables/" + url.PathEscape(tableID) + "/fields?" + q.Encode()
		if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
			return nil, fmt.Errorf("lark http client: list base fields: %w", err)
		}
		if resp.Code != 0 {
			return nil, &APIError{Op: "list base fields", Code: resp.Code, Msg: resp.Msg}
		}
		for _, it := range resp.Data.Items {
			out = append(out, it.FieldName)
		}
		if !resp.Data.HasMore || resp.Data.PageToken == "" || len(out) >= 500 {
			return out, nil
		}
		pageToken = resp.Data.PageToken
	}
}

func (c *httpAPIClient) ListBitableRecords(ctx context.Context, creds InstallationCredentials, appToken, tableID, pageToken string) ([]map[string]any, string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Items []struct {
				Fields map[string]any `json:"fields"`
			} `json:"items"`
			HasMore   bool   `json:"has_more"`
			PageToken string `json:"page_token"`
		} `json:"data"`
	}
	q := url.Values{}
	q.Set("page_size", "500")
	if pageToken != "" {
		q.Set("page_token", pageToken)
	}
	path := "/open-apis/bitable/v1/apps/" + url.PathEscape(appToken) + "/tables/" + url.PathEscape(tableID) + "/records?" + q.Encode()
	if err := c.doAuthedJSON(ctx, creds, http.MethodGet, path, nil, &resp); err != nil {
		return nil, "", fmt.Errorf("lark http client: list base records: %w", err)
	}
	if resp.Code != 0 {
		return nil, "", &APIError{Op: "list base records", Code: resp.Code, Msg: resp.Msg}
	}
	records := make([]map[string]any, 0, len(resp.Data.Items))
	for _, it := range resp.Data.Items {
		records = append(records, it.Fields)
	}
	next := ""
	if resp.Data.HasMore {
		next = resp.Data.PageToken
	}
	return records, next, nil
}

// sheetCellString renders one cell. ToString rendering makes most cells
// strings already; rich cells (links, mentions) arrive as objects or arrays.
func sheetCellString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	case []any:
		// Rich-text segments concatenate; lists of people, options or
		// attachments are separate values.
		sep := ""
		if len(x) > 1 {
			if m, ok := x[0].(map[string]any); !ok || m["type"] != "text" {
				sep = ", "
			}
		}
		parts := make([]string, 0, len(x))
		for _, p := range x {
			parts = append(parts, sheetCellString(p))
		}
		return strings.Join(parts, sep)
	case map[string]any:
		for _, key := range []string{"text", "name", "link", "en_name", "email"} {
			if s, ok := x[key].(string); ok && s != "" {
				return s
			}
		}
		if v, ok := x["value"]; ok {
			return sheetCellString(v)
		}
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

// maxDescendantsPerInsert stays under Lark's 1000-block cap for one
// create-descendant call.
const maxDescendantsPerInsert = 900

// docxImageBlockType is the image block. Converted markdown images need a
// separate upload step, so they are dropped rather than inserted empty.
const docxImageBlockType = 27

func (c *httpAPIClient) CreateDocFromMarkdown(ctx context.Context, creds InstallationCredentials, title, markdown string) (string, error) {
	var created struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Document struct {
				DocumentID string `json:"document_id"`
			} `json:"document"`
		} `json:"data"`
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, "/open-apis/docx/v1/documents", map[string]any{"title": title}, &created); err != nil {
		return "", fmt.Errorf("lark http client: create document: %w", err)
	}
	if created.Code != 0 || created.Data.Document.DocumentID == "" {
		return "", &APIError{Op: "create document", Code: created.Code, Msg: created.Msg}
	}
	docID := created.Data.Document.DocumentID
	if strings.TrimSpace(markdown) == "" {
		return docID, nil
	}

	var converted struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			FirstLevelBlockIDs []string         `json:"first_level_block_ids"`
			Blocks             []map[string]any `json:"blocks"`
		} `json:"data"`
	}
	body := map[string]any{"content_type": "markdown", "content": markdown}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, "/open-apis/docx/v1/documents/blocks/convert", body, &converted); err != nil {
		return docID, fmt.Errorf("lark http client: convert markdown: %w", err)
	}
	if converted.Code != 0 {
		return docID, &APIError{Op: "convert markdown", Code: converted.Code, Msg: converted.Msg}
	}
	for _, batch := range descendantBatches(converted.Data.FirstLevelBlockIDs, converted.Data.Blocks) {
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		path := "/open-apis/docx/v1/documents/" + url.PathEscape(docID) + "/blocks/" + url.PathEscape(docID) + "/descendant?document_revision_id=-1"
		// No index: Lark appends to the end of the document.
		insert := map[string]any{"children_id": batch.children, "descendants": batch.blocks}
		if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, insert, &resp); err != nil {
			return docID, fmt.Errorf("lark http client: insert document blocks: %w", err)
		}
		if resp.Code != 0 {
			return docID, &APIError{Op: "insert document blocks", Code: resp.Code, Msg: resp.Msg}
		}
	}
	return docID, nil
}

type descendantBatch struct {
	children []string
	blocks   []map[string]any
}

// descendantBatches groups converted blocks into create-descendant calls:
// each batch carries whole top-level subtrees, image blocks are dropped
// (along with references to them), and table merge_info — which the insert
// endpoint rejects — is removed.
func descendantBatches(firstLevel []string, blocks []map[string]any) []descendantBatch {
	byID := make(map[string]map[string]any, len(blocks))
	for _, b := range blocks {
		if id, _ := b["block_id"].(string); id != "" {
			byID[id] = b
		}
	}
	isImage := func(id string) bool {
		b := byID[id]
		t, _ := b["block_type"].(float64)
		return b == nil || int(t) == docxImageBlockType
	}
	var subtree func(id string, out *[]map[string]any)
	subtree = func(id string, out *[]map[string]any) {
		b := byID[id]
		if kids, ok := b["children"].([]any); ok {
			kept := make([]any, 0, len(kids))
			for _, k := range kids {
				if kid, _ := k.(string); kid != "" && !isImage(kid) {
					kept = append(kept, kid)
				}
			}
			b["children"] = kept
		}
		if table, ok := b["table"].(map[string]any); ok {
			if prop, ok := table["property"].(map[string]any); ok {
				delete(prop, "merge_info")
			}
		}
		*out = append(*out, b)
		kids, _ := b["children"].([]any)
		for _, k := range kids {
			subtree(k.(string), out)
		}
	}
	var batches []descendantBatch
	var cur descendantBatch
	for _, id := range firstLevel {
		if isImage(id) {
			continue
		}
		if _, ok := byID[id]["children"].([]any); !ok {
			byID[id]["children"] = []any{}
		}
		var tree []map[string]any
		subtree(id, &tree)
		if len(cur.children) > 0 && len(cur.blocks)+len(tree) > maxDescendantsPerInsert {
			batches = append(batches, cur)
			cur = descendantBatch{}
		}
		cur.children = append(cur.children, id)
		cur.blocks = append(cur.blocks, tree...)
	}
	if len(cur.children) > 0 {
		batches = append(batches, cur)
	}
	return batches
}

func (c *httpAPIClient) ShareDoc(ctx context.Context, creds InstallationCredentials, documentID string, openIDs []string, perm, linkShare string) error {
	for _, openID := range openIDs {
		var resp struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		path := "/open-apis/drive/v1/permissions/" + url.PathEscape(documentID) + "/members?type=docx&need_notification=false"
		body := map[string]any{"member_type": "openid", "member_id": openID, "perm": perm, "type": "user"}
		if err := c.doAuthedJSON(ctx, creds, http.MethodPost, path, body, &resp); err != nil {
			return fmt.Errorf("lark http client: share document: %w", err)
		}
		if resp.Code != 0 {
			return &APIError{Op: "share document", Code: resp.Code, Msg: resp.Msg}
		}
	}
	if linkShare == "" {
		return nil
	}
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	path := "/open-apis/drive/v1/permissions/" + url.PathEscape(documentID) + "/public?type=docx"
	if err := c.doAuthedJSON(ctx, creds, http.MethodPatch, path, map[string]any{"link_share_entity": linkShare}, &resp); err != nil {
		return fmt.Errorf("lark http client: set document link sharing: %w", err)
	}
	if resp.Code != 0 {
		return &APIError{Op: "set document link sharing", Code: resp.Code, Msg: resp.Msg}
	}
	return nil
}

func (c *httpAPIClient) DocURL(ctx context.Context, creds InstallationCredentials, documentID string) (string, error) {
	var resp struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Metas []struct {
				URL string `json:"url"`
			} `json:"metas"`
		} `json:"data"`
	}
	body := map[string]any{
		"request_docs": []any{map[string]any{"doc_token": documentID, "doc_type": "docx"}},
		"with_url":     true,
	}
	if err := c.doAuthedJSON(ctx, creds, http.MethodPost, "/open-apis/drive/v1/metas/batch_query", body, &resp); err != nil {
		return "", fmt.Errorf("lark http client: document url: %w", err)
	}
	if resp.Code != 0 || len(resp.Data.Metas) == 0 || resp.Data.Metas[0].URL == "" {
		return "", &APIError{Op: "document url", Code: resp.Code, Msg: resp.Msg}
	}
	return resp.Data.Metas[0].URL, nil
}

// doAuthedMultipart is doAuthedJSON for Lark's multipart upload endpoints,
// with the same refresh-and-replay-once contract on a rejected token.
func (c *httpAPIClient) doAuthedMultipart(ctx context.Context, creds InstallationCredentials, path string, fields map[string]string, fileField, filename string, data []byte, out any) error {
	for attempt := 0; ; attempt++ {
		token, err := c.tenantAccessToken(ctx, creds)
		if err != nil {
			return err
		}
		err = c.doMultipart(ctx, c.resolveBaseURL(creds)+path, token, fields, fileField, filename, data, out)
		if err == nil {
			err = tokenErrorFromBody(out)
		}
		if attempt > 0 || !isTokenError(larkErrorCode(err)) {
			return err
		}
		c.invalidateToken(creds.AppID)
	}
}

// tokenErrorFromBody surfaces a token rejection that Lark reported with
// HTTP 200, so doAuthedMultipart can refresh and replay.
func tokenErrorFromBody(out any) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	code, msg := parseLarkErrorBody(raw)
	if isTokenError(code) {
		return &APIError{Op: "upload", Code: code, Msg: msg}
	}
	return nil
}

func (c *httpAPIClient) doMultipart(ctx context.Context, fullURL, token string, fields map[string]string, fileField, filename string, data []byte, out any) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return fmt.Errorf("write field %s: %w", k, err)
		}
	}
	part, err := w.CreateFormFile(fileField, filename)
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err := part.Write(data); err != nil {
		return fmt.Errorf("write form file: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("close multipart: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fullURL, &buf)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.cfg.ResourceHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		code, msg := parseLarkErrorBody(rawBody)
		return &larkAPIStatusError{StatusCode: resp.StatusCode, Code: code, Msg: msg, Raw: truncate(string(rawBody), 512)}
	}
	if err := json.Unmarshal(rawBody, out); err != nil {
		return fmt.Errorf("decode body: %w (raw=%s)", err, truncate(string(rawBody), 256))
	}
	return nil
}

var _ ToolAPIClient = (*httpAPIClient)(nil)
