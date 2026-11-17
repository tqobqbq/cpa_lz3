package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/examples/plugin/session-quota-control/evaluation"
)

// probe uses the Hub API key directly. Only transport is specific to Hub; the
// evaluation runner owns challenges, retries, validation and classification.
func (c *hubClient) probe(secret string) evaluation.Probe {
	return func(ctx context.Context, prompt evaluation.ProbeRequest) (evaluation.ProbeResponse, error) {
		body, headers, path := hubProbePayload(c.settings.Protocol, prompt)
		payload, errMarshal := json.Marshal(body)
		if errMarshal != nil {
			return evaluation.ProbeResponse{}, fmt.Errorf("编码模型请求失败")
		}
		base := strings.TrimRight(c.settings.APIBaseURL, "/")
		if !strings.HasSuffix(base, "/v1") {
			base += "/v1"
		}
		req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(payload))
		if errRequest != nil {
			return evaluation.ProbeResponse{}, fmt.Errorf("创建模型请求失败")
		}
		req.Header = headers
		req.Header.Set("Authorization", "Bearer "+secret)
		if path == "/messages?beta=true" {
			req.Header.Set("X-Api-Key", secret)
		}
		resp, errDo := c.http.Do(req)
		if errDo != nil {
			if ctx.Err() != nil {
				return evaluation.ProbeResponse{}, ctx.Err()
			}
			return evaluation.ProbeResponse{}, evaluation.RetryProbe(fmt.Errorf("Hub 模型连接失败，请检查接口和代理"))
		}
		defer closeHubBody(resp.Body)
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errStatus := fmt.Errorf("Hub 模型请求返回 HTTP %d", resp.StatusCode)
			if resp.StatusCode == 408 || resp.StatusCode >= 500 {
				return evaluation.ProbeResponse{}, evaluation.RetryProbe(errStatus)
			}
			return evaluation.ProbeResponse{}, errStatus
		}
		out, errRead := readHubProbe(resp.Body, strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"), path == "/messages?beta=true")
		out.Text = strings.ReplaceAll(out.Text, secret, "[redacted]")
		if errRead != nil {
			return out, evaluation.RetryProbe(fmt.Errorf("%s", c.redact(errRead.Error(), secret)))
		}
		return out, nil
	}
}

func hubProbePayload(protocol string, p evaluation.ProbeRequest) (map[string]any, http.Header, string) {
	if protocol == "auto" {
		protocol = "codex"
		if strings.Contains(strings.ToLower(p.Model), "claude") {
			protocol = "claude"
		}
	}
	headers := http.Header{"Content-Type": {"application/json"}, "Accept": {"text/event-stream"}}
	if protocol == "claude" {
		headers.Set("User-Agent", "claude-cli/2.1.141 (external, sdk-cli)")
		headers.Set("Anthropic-Version", "2023-06-01")
		headers.Set("Anthropic-Beta", "claude-code-20250219,interleaved-thinking-2025-05-14,effort-2025-11-24")
		headers.Set("X-App", "cli")
		body := map[string]any{"model": p.Model, "stream": true, "max_tokens": 16384,
			"messages": []any{map[string]any{"role": "user", "content": p.Prompt}},
			"system":   "You are a Claude agent, built on Anthropic's Claude Agent SDK.\n" + p.Instructions,
			"tools":    []any{map[string]any{"name": "Bash", "description": "execute shell commands", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}, "required": []string{"command"}}}},
		}
		if p.Effort != "" && p.Effort != "none" {
			effort := p.Effort
			if effort == "minimal" {
				effort = "low"
			}
			if effort == "xhigh" {
				effort = "max"
			}
			body["thinking"], body["output_config"] = map[string]any{"type": "adaptive"}, map[string]any{"effort": effort}
		}
		return body, headers, "/messages?beta=true"
	}
	headers.Set("User-Agent", "codex_exec/0.130.0 (Ubuntu 24.4.0; x86_64)")
	headers.Set("Originator", "codex_exec")
	input := []any{}
	if p.Instructions != "" {
		input = append(input, map[string]any{"role": "developer", "content": p.Instructions})
	}
	input = append(input, map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": p.Prompt}}})
	body := map[string]any{"model": p.Model, "input": input, "stream": true, "store": false, "tool_choice": "auto", "parallel_tool_calls": true,
		"tools": []any{map[string]any{"type": "function", "name": "exec_command", "description": "Runs a command in a PTY.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []string{"cmd"}}}},
	}
	if p.Effort != "" && p.Effort != "none" {
		body["reasoning"] = map[string]string{"effort": p.Effort}
	}
	return body, headers, "/responses"
}

func readHubProbe(reader io.Reader, streaming, claude bool) (evaluation.ProbeResponse, error) {
	var out evaluation.ProbeResponse
	if !streaming {
		raw, errRead := io.ReadAll(io.LimitReader(reader, 4<<20))
		if errRead != nil {
			return out, fmt.Errorf("读取模型回复失败")
		}
		return decodeHubAnswer(raw, claude)
	}
	scanner := bufio.NewScanner(io.LimitReader(reader, 4<<20))
	scanner.Buffer(make([]byte, 4096), 2<<20)
	var data []string
	complete := false
	consume := func() error {
		if len(data) == 0 {
			return nil
		}
		raw := strings.Join(data, "\n")
		data = nil
		if raw == "[DONE]" {
			return nil
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
			Message  json.RawMessage `json:"message"`
			Delta    struct {
				Type       string `json:"type"`
				Text       string `json:"text"`
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
			} `json:"usage"`
		}
		if errDecode := json.Unmarshal([]byte(raw), &event); errDecode != nil {
			return fmt.Errorf("无法解析模型事件流")
		}
		switch event.Type {
		case "error", "response.failed", "response.incomplete":
			return fmt.Errorf("模型返回失败或未完成事件")
		case "response.completed":
			var errDecode error
			out, errDecode = decodeHubAnswer(event.Response, false)
			complete = errDecode == nil
			return errDecode
		case "message_start":
			var message struct {
				Usage struct {
					Input  int64 `json:"input_tokens"`
					Output int64 `json:"output_tokens"`
				} `json:"usage"`
			}
			if errDecode := json.Unmarshal(event.Message, &message); errDecode != nil {
				return fmt.Errorf("无法解析模型消息")
			}
			out.InputTokens, out.OutputTokens = message.Usage.Input, message.Usage.Output
		case "content_block_delta":
			if event.Delta.Type == "text_delta" {
				out.Text += event.Delta.Text
			}
		case "message_delta":
			out.OutputTokens = event.Usage.Output
			if event.Delta.StopReason != "" && event.Delta.StopReason != "end_turn" && event.Delta.StopReason != "stop_sequence" {
				return fmt.Errorf("模型未直接完成回答（%s）", event.Delta.StopReason)
			}
		case "message_stop":
			complete = claude
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if errConsume := consume(); errConsume != nil {
				return out, errConsume
			}
			if complete {
				break
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return out, fmt.Errorf("模型事件流中断")
	}
	if errConsume := consume(); errConsume != nil {
		return out, errConsume
	}
	if !complete || strings.TrimSpace(out.Text) == "" {
		return out, fmt.Errorf("模型未返回完整文本回答")
	}
	return out, nil
}

func decodeHubAnswer(raw []byte, claude bool) (evaluation.ProbeResponse, error) {
	var value struct {
		Status     string          `json:"status"`
		StopReason string          `json:"stop_reason"`
		Error      json.RawMessage `json:"error"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Details struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	var out evaluation.ProbeResponse
	if errDecode := json.Unmarshal(raw, &value); errDecode != nil {
		return out, fmt.Errorf("无法解析模型回复")
	}
	out.InputTokens, out.OutputTokens, out.ReasoningTokens = value.Usage.Input, value.Usage.Output, value.Usage.Details.Reasoning
	if (len(value.Error) != 0 && string(value.Error) != "null") || (value.Status != "" && value.Status != "completed") || (claude && value.StopReason != "end_turn" && value.StopReason != "stop_sequence") {
		return out, fmt.Errorf("模型未正常完成回答")
	}
	for _, item := range value.Content {
		if item.Type == "text" {
			out.Text += item.Text
		}
	}
	if !claude {
		for _, item := range value.Output {
			if item.Type != "message" {
				continue
			}
			for _, part := range item.Content {
				if part.Type == "output_text" {
					out.Text += part.Text
				}
			}
		}
	}
	if strings.TrimSpace(out.Text) == "" {
		return out, fmt.Errorf("模型未直接返回文本回答")
	}
	return out, nil
}
