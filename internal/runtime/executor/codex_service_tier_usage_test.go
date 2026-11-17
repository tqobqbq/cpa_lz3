package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type serviceTierUsageExecutor interface {
	Execute(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error)
	ExecuteStream(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error)
}

func TestCodexUsageRecordsFinalOutboundServiceTier(t *testing.T) {
	models := []config.PayloadModelRule{{Name: "gpt-*", Protocol: "codex"}}
	for _, tc := range []struct {
		name, clientTier, want string
		payload                config.PayloadConfig
	}{
		{"filtered", "priority", "auto", config.PayloadConfig{Filter: []config.PayloadFilterRule{{Models: models, Params: []string{"service_tier", "speed"}}}}},
		{"standard override", "priority", "default", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"service_tier": "default"}}}}},
		{"priority override", "default", "priority", config.PayloadConfig{Override: []config.PayloadRule{{Models: models, Params: map[string]any{"service_tier": "priority"}}}}},
	} {
		for _, useWS := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				for _, from := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatClaude} {
					t.Run(fmt.Sprintf("%s/ws=%t/stream=%t/from=%s", tc.name, useWS, stream, from), func(t *testing.T) {
						const completed = `{"type":"response.completed","response":{"id":"resp_tier","object":"response","status":"completed","model":"gpt-5.5","service_tier":"default","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
						bodies := make(chan []byte, 1)
						server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							if useWS {
								upgrader := websocket.Upgrader{}
								conn, errUpgrade := upgrader.Upgrade(w, r, nil)
								if errUpgrade != nil {
									t.Error(errUpgrade)
									return
								}
								defer func() {
									if errClose := conn.Close(); errClose != nil {
										t.Error(errClose)
									}
								}()
								_, body, errRead := conn.ReadMessage()
								if errRead != nil {
									t.Error(errRead)
									return
								}
								bodies <- body
								if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(completed)); errWrite != nil {
									t.Error(errWrite)
								}
								return
							}
							body, errRead := io.ReadAll(r.Body)
							if errRead != nil {
								t.Error(errRead)
								return
							}
							bodies <- body
							w.Header().Set("Content-Type", "text/event-stream")
							if _, errWrite := io.WriteString(w, "data: "+completed+"\n\n"); errWrite != nil {
								t.Error(errWrite)
							}
						}))
						t.Cleanup(server.Close)
						cfg := &config.Config{Payload: tc.payload, SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}}
						var executor serviceTierUsageExecutor = NewCodexExecutor(cfg)
						if useWS {
							executor = NewCodexWebsocketsExecutor(cfg)
						}
						clientTier := tc.clientTier
						payload := fmt.Sprintf(`{"model":"gpt-5.5","service_tier":%q,"input":"hello"}`, clientTier)
						if from == sdktranslator.FormatClaude {
							speed := ""
							if clientTier == "priority" {
								speed = `,"speed":"fast"`
							}
							payload = `{"model":"gpt-5.5","max_tokens":8,"messages":[{"role":"user","content":"hello"}]` + speed + `}`
							clientTier = "auto"
						}
						capture := &codexResponseModelUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
						coreusage.RegisterNamedPlugin(t.Name(), capture)
						t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })
						ctx := coreusage.WithServiceTier(coreusage.WithRequestedModelAlias(context.Background(), t.Name()), clientTier)
						req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(payload)}
						opts := cliproxyexecutor.Options{SourceFormat: from, Stream: stream}
						if stream {
							result, errExecute := executor.ExecuteStream(ctx, codexOAuthTestAuth(server.URL), req, opts)
							if errExecute != nil {
								t.Fatal(errExecute)
							}
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									t.Fatal(chunk.Err)
								}
							}
						} else if _, errExecute := executor.Execute(ctx, codexOAuthTestAuth(server.URL), req, opts); errExecute != nil {
							t.Fatal(errExecute)
						}
						body := <-bodies
						tier := gjson.GetBytes(body, "service_tier").String()
						if tier == "" {
							tier = "auto"
						}
						record := capture.await(t)
						if tier != tc.want || record.OutboundServiceTier != tier || record.ServiceTier != clientTier || record.ResponseServiceTier != "default" {
							t.Fatalf("sent=%q, recorded=%q, client=%q, response=%q; want %q, %q, %q, default", tier, record.OutboundServiceTier, record.ServiceTier, record.ResponseServiceTier, tc.want, tc.want, clientTier)
						}
					})
				}
			}
		}
	}
}
