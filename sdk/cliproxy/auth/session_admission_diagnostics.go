package auth

import (
	"maps"
	"strings"
	"time"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	coresession "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/session"
	"github.com/tidwall/gjson"
)

// Keep the diagnostic input set limited to identity signals understood by the
// native session extractor. Credential headers and message bodies are excluded.
var sessionDiagnosticHeaders = diagnosticFieldSet(`
	X-Claude-Code-Session-Id X-Claude-Code-Agent-Id X-Claude-Code-Parent-Agent-Id
	Session-Id Session_id Thread-Id Thread_id X-Codex-Turn-Metadata
	X-Codex-Parent-Thread-Id X-Openai-Subagent X-Http-Session-Id
	X-Session-Id X-Session-Affinity X-Slot-Session-Id X-Parent-Slot-Session-Id
	X-Parent-Session-Id X-Parent-Id X-Parent-Session-Affinity
	X-Task-Id X-Task_ID X-Parent-Task-Id X-Conversation-Id
	X-Parent-Conversation-Id X-Thread-Id X-Parent-Thread-Id X-Client-Request-Id
	X-Agent-Id
`)

var sessionDiagnosticBodyFields = diagnosticFieldSet(`
	session_id sessionId sessionID child_session_id childSessionId
	thread_id threadId task_id taskId taskID action_id actionId actionID
	conversation_id conversationId chat_id chatId prompt_cache_key promptCacheKey
	cachedContent cached_content user_id agent_id subagent_id parent_agent_id parentAgentId
	parent_session_id parentSessionId parentSessionID parent_thread_id parentThreadId parentThreadID
	parent_conversation_id parentConversationId parentConversationID parent_id parentId parentID
	parent_task_id parentTaskId parentTaskID parent_action_id parentActionId parentActionID
	parent_session parentSession parent_subagent_id parentSubagentId
	forked_from_thread_id forked_from_id previousSessionId previous_session_id
`)

func diagnosticFieldSet(names string) map[string]struct{} {
	fields := make(map[string]struct{})
	for _, name := range strings.Fields(names) {
		fields[strings.ToLower(name)] = struct{}{}
	}
	return fields
}

func collectSessionDiagnosticBody(root gjson.Result, prefix string, fields map[string]any) {
	root.ForEach(func(key, value gjson.Result) bool {
		name, path := key.String(), prefix+key.String()
		if _, known := sessionDiagnosticBodyFields[strings.ToLower(name)]; known {
			fields[path] = value.Value()
			return true
		}
		switch name {
		case "metadata", "extra_body":
			if (prefix == "" || prefix == "request.") && value.IsObject() {
				collectSessionDiagnosticBody(value, path+".", fields)
			}
		case "request":
			if prefix == "" && !root.Get("contents").Exists() && value.IsObject() {
				collectSessionDiagnosticBody(value, path+".", fields)
			}
		case "forkSource", "fork_source":
			for _, field := range []string{"sessionId", "session_id"} {
				if id := value.Get(field); id.Exists() {
					fields[path+"."+field] = id.Value()
				}
			}
		case "conversation":
			if value.Type == gjson.String {
				fields[path] = value.String()
			} else if id := value.Get("id"); id.Exists() {
				fields[path+".id"] = id.Value()
			}
		}
		return true
	})
}

func (s *SessionAffinitySelector) sessionAdmissionDiagnostics(provider, model, sessionID string, opts coreexecutor.Options) map[string]any {
	headers, payload, metadata := make(map[string]any), make(map[string]any), make(map[string]any)
	for name, values := range opts.Headers {
		if _, known := sessionDiagnosticHeaders[strings.ToLower(name)]; known {
			headers[name] = append([]string(nil), values...)
		}
	}
	if len(opts.OriginalRequest) > 0 {
		collectSessionDiagnosticBody(gjson.ParseBytes(opts.OriginalRequest), "", payload)
	}
	for _, key := range []string{
		coreexecutor.ExecutionSessionMetadataKey, coreexecutor.CanonicalSessionIDMetadataKey,
		coreexecutor.ParentSessionIDMetadataKey, coreexecutor.DerivedSessionIDMetadataKey,
		coreexecutor.CallerScopeMetadataKey, coreexecutor.LCPAffinitySessionIDMetadataKey,
		coreexecutor.LCPFingerprintMetadataKey, coreexecutor.LCPMinPrefixLengthMetadataKey,
		coreexecutor.LCPTailFingerprintsMetadataKey, coreexecutor.LCPEnvironmentDigestMetadataKey,
		coreexecutor.LCPAccessGenerationMetadataKey, coreexecutor.IsForkMetadataKey,
		coreexecutor.IsCompactionMetadataKey, coreexecutor.NodeKindMetadataKey,
		coreexecutor.RequestedModelMetadataKey,
		manualChoiceMetadataKey,
	} {
		if value, exists := opts.Metadata[key]; exists {
			metadata[key] = value
		}
	}

	info, explicit := coresession.ExtractSessionInfo(opts.Headers, opts.OriginalRequest, opts.Metadata)
	_, fallbackID := extractSessionIDs(opts.Headers, opts.OriginalRequest, maps.Clone(opts.Metadata))
	mode, reason := "message_hash", "no_native_binding"
	if explicit && info.ClientType != "lcp" {
		mode = "client_identity"
		if strings.HasPrefix(info.SessionID, "execution:") {
			mode = "execution_connection"
		}
	} else if coresession.DerivedID(opts.Metadata) != "" {
		mode = "derived_context"
	}
	cacheKey, parentCacheKey := "", ""
	modelKey := canonicalModelKey(model)
	if strings.HasPrefix(sessionID, "lcp-pending:") {
		mode, reason, fallbackID = "content_lcp", "no_content_prefix_match", ""
	} else if sessionID == "" {
		mode, reason = "unidentified", "no_session_identity"
	} else {
		cacheKey = provider + "::" + sessionID + "::" + modelKey
		if fallbackID != "" && fallbackID != sessionID {
			parentCacheKey = provider + "::" + coresession.BoundSessionIdentity(fallbackID) + "::" + modelKey
		}
	}
	rebindOn := SessionAffinityRebindOnAny
	if s.rebindOnQuotaOnly {
		rebindOn = SessionAffinityRebindOnQuota
	}
	return map[string]any{
		"mode": mode, "pending_reason": reason,
		"provider": provider, "model": model, "model_key": modelKey, "source_format": string(opts.SourceFormat),
		"session_id": sessionID, "parent_session_id": fallbackID,
		"cache_key": cacheKey, "parent_cache_key": parentCacheKey,
		"cache_binding": s.sessionDiagnosticBinding(cacheKey), "parent_cache_binding": s.sessionDiagnosticBinding(parentCacheKey),
		"session_ttl_seconds": s.cache.ttl.Seconds(), "subagent_affinity": s.subagentAffinity, "rebind_on": rebindOn,
		"content_matching_used": mode == "content_lcp",
		"lcp_namespace":         lcpAffinityNamespace(provider, model, opts.Metadata),
		"extracted": map[string]any{
			"session_id": info.SessionID, "parent_session_id": info.ParentSessionID,
			"client_type": info.ClientType, "agent_name": info.AgentName, "caller_scope": info.CallerScope,
			"is_subagent": info.IsSubagent, "is_fork": info.IsFork, "is_compaction": info.IsCompaction, "node_kind": info.NodeKind,
		},
		"headers": headers, "payload_fields": payload, "metadata": metadata,
	}
}

func (s *SessionAffinitySelector) sessionDiagnosticBinding(key string) map[string]any {
	if key == "" {
		return map[string]any{"status": "not_applicable"}
	}
	s.cache.mu.RLock()
	entry, exists := s.cache.entries[key]
	s.cache.mu.RUnlock()
	if !exists {
		return map[string]any{"status": "unbound"}
	}
	status := "bound"
	if !time.Now().Before(entry.expiresAt) {
		status = "expired"
	}
	return map[string]any{"status": status, "auth_id": entry.authID, "expires_at": entry.expiresAt}
}
