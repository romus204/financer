// Package tg is a minimal Telegram Bot API client built on the standard library.
package tg

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
	"strconv"
	"strings"
	"time"
)

const apiBase = "https://api.telegram.org/bot"

type Client struct {
	base string
	http *http.Client
}

func New(token string) *Client { return NewWithURL(token, apiBase) }

// NewWithURL lets you set a custom Bot API address (a local server or a test stub).
func NewWithURL(token, apiURL string) *Client {
	return &Client{
		base: apiURL + token + "/",
		// The timeout is longer than the getUpdates long polling.
		http: &http.Client{Timeout: 75 * time.Second},
	}
}

// APIError is an error returned by Telegram.
type APIError struct {
	Method      string
	Code        int
	Description string
	RetryAfter  int
}

func (e *APIError) Error() string {
	return fmt.Sprintf("telegram %s: %d %s", e.Method, e.Code, e.Description)
}

type response struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+method, bytes.NewReader(body))
	if err != nil {
		// Don't wrap the original error: it may contain the URL with the token.
		return fmt.Errorf("telegram %s: не удалось создать запрос", method)
	}
	req.Header.Set("Content-Type", "application/json")
	return c.do(method, req, out)
}

func (c *Client) do(method string, req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error contains the full URL including the bot token — strip it from the error.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("telegram %s: %w", method, err)
	}
	defer resp.Body.Close()

	var r response
	if err := json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: HTTP %d: %w", method, resp.StatusCode, err)
	}
	if !r.OK {
		e := &APIError{Method: method, Code: r.ErrorCode, Description: r.Description}
		if r.Parameters != nil {
			e.RetryAfter = r.Parameters.RetryAfter
		}
		return e
	}
	if out != nil && len(r.Result) > 0 {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeout int) ([]Update, error) {
	var updates []Update
	err := c.call(ctx, "getUpdates", map[string]any{
		"offset":          offset,
		"timeout":         timeout,
		"allowed_updates": []string{"message", "callback_query"},
	}, &updates)
	return updates, err
}

// SendMessage sends an HTML message. markup is *InlineKeyboard, *ReplyKeyboard or nil.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, markup any) (*Message, error) {
	p := textParams(text)
	p["chat_id"] = chatID
	if markup != nil {
		p["reply_markup"] = markup
	}
	var m Message
	if err := c.call(ctx, "sendMessage", p, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// EditMessageText changes the text and inline keyboard. kb == nil removes the keyboard.
func (c *Client) EditMessageText(ctx context.Context, chatID int64, msgID int, text string, kb *InlineKeyboard) error {
	p := textParams(text)
	p["chat_id"] = chatID
	p["message_id"] = msgID
	if kb != nil {
		p["reply_markup"] = kb
	}
	err := c.call(ctx, "editMessageText", p, nil)
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Description, "message is not modified") {
		return nil
	}
	return err
}

func (c *Client) AnswerCallbackQuery(ctx context.Context, id, text string) error {
	p := map[string]any{"callback_query_id": id}
	if text != "" {
		p["text"] = text
	}
	return c.call(ctx, "answerCallbackQuery", p, nil)
}

func (c *Client) DeleteMessage(ctx context.Context, chatID int64, msgID int) error {
	return c.call(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": msgID}, nil)
}

func (c *Client) SetMyCommands(ctx context.Context, cmds []BotCommand) error {
	return c.call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil)
}

func (c *Client) SendDocument(ctx context.Context, chatID int64, filename string, data []byte, caption string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	fw, err := w.CreateFormFile("document", filename)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"sendDocument", &buf)
	if err != nil {
		return errors.New("telegram sendDocument: не удалось создать запрос")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	return c.do("sendDocument", req, nil)
}

func textParams(text string) map[string]any {
	return map[string]any{
		"text":                 text,
		"parse_mode":           "HTML",
		"link_preview_options": map[string]bool{"is_disabled": true},
	}
}
