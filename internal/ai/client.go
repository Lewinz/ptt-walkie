// Package ai adds speech-to-text, keyword alerts and "what did I miss"
// summaries on top of the archive, using Qiniu AI inference (or any
// OpenAI-compatible endpoint).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Client talks to an OpenAI-compatible inference endpoint.
type Client struct {
	BaseURL  string
	Key      string
	Model    string // chat model for summaries
	ASRMode  string // "qiniu" or "openai"
	ASRModel string
	HTTP     *http.Client
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 60 * time.Second}
}

func (c *Client) do(ctx context.Context, req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+c.Key)
	resp, err := c.http().Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %d %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

func (c *Client) url(path string) string {
	return strings.TrimRight(c.BaseURL, "/") + path
}

// Chat runs a single-turn chat completion.
func (c *Client) Chat(ctx context.Context, system, user string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model": c.Model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature": 0.3,
		"max_tokens":  300,
	})
	req, _ := http.NewRequest(http.MethodPost, c.url("/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := c.do(ctx, req, &out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", errors.New("chat: empty response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// Transcribe converts an Ogg Opus recording to text. Qiniu mode fetches the
// audio from audioURL (must be publicly reachable); OpenAI mode uploads it.
func (c *Client) Transcribe(ctx context.Context, audioURL string, ogg []byte) (string, error) {
	if c.ASRMode == "openai" {
		return c.transcribeUpload(ctx, ogg)
	}
	body, _ := json.Marshal(map[string]any{
		"model": c.ASRModel,
		"audio": map[string]string{"format": "ogg", "url": audioURL},
	})
	req, _ := http.NewRequest(http.MethodPost, c.url("/voice/asr"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Data struct {
			Result struct {
				Text string `json:"text"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := c.do(ctx, req, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Data.Result.Text), nil
}

func (c *Client) transcribeUpload(ctx context.Context, ogg []byte) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("model", c.ASRModel)
	mw.WriteField("language", "zh")
	fw, _ := mw.CreateFormFile("file", "audio.ogg")
	fw.Write(ogg)
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, c.url("/audio/transcriptions"), &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out struct {
		Text string `json:"text"`
	}
	if err := c.do(ctx, req, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Text), nil
}

// ToolCall is a function call requested by the model.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Message is an OpenAI chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ChatTools runs one chat completion round with function tools.
func (c *Client) ChatTools(ctx context.Context, msgs []Message, tools []map[string]any) (Message, error) {
	body, _ := json.Marshal(map[string]any{
		"model":       c.Model,
		"messages":    msgs,
		"tools":       tools,
		"tool_choice": "auto",
		"temperature": 0.2,
		"max_tokens":  300,
	})
	req, _ := http.NewRequest(http.MethodPost, c.url("/chat/completions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Choices []struct {
			Message Message `json:"message"`
		} `json:"choices"`
	}
	if err := c.do(ctx, req, &out); err != nil {
		return Message{}, err
	}
	if len(out.Choices) == 0 {
		return Message{}, errors.New("chat: empty response")
	}
	return out.Choices[0].Message, nil
}
