#!/usr/bin/env python3
"""
全链路脚本：找到能成功对话的免费 gpt-5.5-pro channel 并绑定到 key

1. 爬取所有 free_channels_with_models
2. 找出包含 gpt-5.5-pro 的 channels，更新到 key #825
3. 逐个将 API key 绑定到单个 channel，并用 Codex/Claude 格式请求测试
4. 将成功的 channels 更新到 key #825
"""

import os
import time
import json
from pathlib import Path

import requests

from crawl_free_channels import fetch_all_channels, is_free, summarize,main as crawl_main
from api_key_manager import update_channels, get_api_key_profiles, gql

SCRIPT_DIR = Path(__file__).parent
TEST_BASE_URL = "https://hub.oaifree.com"
REQUEST_PROMPT = "hi"
REQUEST_TIMEOUT_SECONDS = 120
SECRET_FIELD_CANDIDATES = [
    "key",
    "token",
    "value",
    "secret",
    "apiKey",
    "apiToken",
    "accessToken",
    "plainKey",
    "rawKey",
]

FRONTEND_GET_API_KEY_QUERY = """
query GetApiKey($id: ID!) {
  node(id: $id) {
    ... on APIKey {
      id
      createdAt
      updatedAt
      key
      name
      type
      status
      scopes
      profiles {
        activeProfile
        profiles {
          name
          modelMappings { from to }
          channelIDs
          channelTags
          channelTagsMatchMode
          modelIDs
          loadBalanceStrategy
          routingPolicy {
            channelWeights { channelID weight }
            providerWeights { provider weight }
            includeProviders
            excludeProviders
            allowChannelAPIKeys { channelID keyHashes }
            excludeChannelAPIKeys { channelID keyHashes }
            fallbackChannelIDs
            excludeChannelIDs
          }
          channelBindingMode
          dynamicChannelStrategy {
            mode
            selectionPolicy
            maxChannels
            minChannels
            maxPriceMultiplier
            maxLatencyMs
            minSuccessRate
            onlyOfficial
            includeTags
            excludeTags
            excludeChannelIDs
            fallbackChannelIDs
          }
          quota {
            requests
            totalTokens
            cost
            period {
              type
              pastDuration { value unit }
              calendarDuration { unit }
            }
          }
        }
      }
    }
  }
}
"""

FRONTEND_GET_API_KEYS_QUERY = """
query GetApiKeys($first: Int, $after: Cursor, $orderBy: APIKeyOrder, $where: APIKeyWhereInput) {
  apiKeys(first: $first, after: $after, orderBy: $orderBy, where: $where) {
    edges {
      node {
        id
        createdAt
        updatedAt
        user {
          id
          firstName
          lastName
          email
          avatar
          linuxdoUserID
          linuxdoUsername
          linuxdoProfile {
            id
            username
            name
            avatarTemplate
            avatarUrl
            active
            trustLevel
            silenced
            externalIds
            updatedAt
          }
        }
        key
        name
        type
        status
        scopes
      }
      cursor
    }
    pageInfo {
      hasNextPage
      hasPreviousPage
      startCursor
      endCursor
    }
    totalCount
  }
}
"""

ME_QUERY = """
query {
  me {
    id
  }
}
"""


def _is_masked_secret(value: str) -> bool:
    lowered = value.lower()
    return (
        "*" in value
        or "..." in value
        or "redacted" in lowered
        or "masked" in lowered
        or lowered in {"null", "none"}
    )


def extract_api_key_secret(key: dict) -> str | None:
    """从 APIKey 节点里提取可实际使用的明文 key。"""
    for field in SECRET_FIELD_CANDIDATES:
        value = key.get(field)
        if isinstance(value, str) and value.strip() and not _is_masked_secret(value):
            return value.strip()
    return None


def get_current_user_id() -> str:
    value = os.environ.get("HUB_USER_ID")
    if value:
        return value
    result = gql(ME_QUERY)
    user_id = result.get("data", {}).get("me", {}).get("id")
    if not user_id:
        raise RuntimeError("无法获取当前用户 ID，请设置 HUB_USER_ID")
    return user_id


def get_api_key_secret(key_id: int) -> str:
    """
    获取 api_key_id 对应的真实密钥。

    优先复刻 /project/api-keys 前端使用的 GetApiKey GraphQL query。
    同时支持环境变量，方便在前端接口不返回明文 key 时手动注入：
    HUB_API_KEY_<id> 或 HUB_API_KEY。
    """
    for env_name in (f"HUB_API_KEY_{key_id}", "HUB_API_KEY"):
        value = os.environ.get(env_name)
        if value:
            return value

    gid = f"gid://axonhub/APIKey/{key_id}"
    user_id = get_current_user_id()
    list_result = gql(
        FRONTEND_GET_API_KEYS_QUERY,
        {
            "first": 20,
            "where": {
                "statusIn": ["enabled", "disabled"],
                "userID": user_id,
                "typeNotIn": ["noauth"],
            },
            "orderBy": {"field": "CREATED_AT", "direction": "DESC"},
        },
        operation_name="GetApiKeys",
    )
    edges = list_result.get("data", {}).get("apiKeys", {}).get("edges") or []
    for edge in edges:
        node = edge.get("node") or {}
        if node.get("id") == gid:
            secret = extract_api_key_secret(node)
            if secret:
                return secret
            break

    result = gql(FRONTEND_GET_API_KEY_QUERY, {"id": gid}, operation_name="GetApiKey")

    key = result.get("data", {}).get("node")
    if not key:
        raise RuntimeError(f"API key #{key_id} 不存在或无权访问")

    secret = extract_api_key_secret(key)
    if not secret:
        raise RuntimeError(
            f"/project/api-keys 前端接口未返回 API key #{key_id} 的明文密钥，"
            f"请设置 HUB_API_KEY_{key_id} 或 HUB_API_KEY"
        )
    return secret


def normalize_openai_base_url(base_url: str) -> str:
    base = base_url.rstrip("/")
    return base if base.endswith("/v1") else f"{base}/v1"


def build_codex_tool() -> dict:
    return {
        "type": "function",
        "name": "exec_command",
        "description": "Runs a command in a PTY, returning output or a session ID for ongoing interaction.",
        "strict": False,
        "parameters": {
            "type": "object",
            "properties": {
                "cmd": {
                    "type": "string",
                    "description": "Shell command to execute.",
                },
                "yield_time_ms": {
                    "type": "number",
                    "description": "How long to wait (in milliseconds) for output before yielding.",
                },
                "max_output_tokens": {
                    "type": "number",
                    "description": "Maximum number of tokens to return. Excess output will be truncated.",
                }
            },
            "required": ["cmd"],
            "additionalProperties": False,
        },
    }


def build_claude_tool() -> dict:
    return {
        "name": "Bash",
        "description": "execute shell commands",
        "input_schema": {
            "$schema": "https://json-schema.org/draft/2020-12/schema",
            "type": "object",
            "properties": {
                "command": {
                    "type": "string",
                    "description": "The command to execute",
                },
                "timeout": {
                    "type": "number",
                    "description": "Optional timeout in milliseconds (max 600000)",
                },
                "description": {
                    "type": "string",
                    "description": "Clear, concise description of what this command does in active voice.",
                },
                "run_in_background": {
                    "type": "boolean",
                    "description": "Set to true to run this command in the background.",
                },
                "dangerouslyDisableSandbox": {
                    "type": "boolean",
                    "description": "Set this to true to dangerously override sandbox mode and run commands without sandboxing.",
                }
            },
            "required": ["command"],
            "additionalProperties": False,
        },
    }


def build_codex_request(model: str, base_url: str = TEST_BASE_URL, api_key: str = "") -> dict:
    return {
        "url": f"{normalize_openai_base_url(base_url)}/responses",
        "headers": {
            "Authorization": f"Bearer {api_key}",
            "Accept": "text/event-stream",
            "Content-Type": "application/json",
            "originator": "codex_exec",
            "User-Agent": "codex_exec/0.130.0 (Ubuntu 24.4.0; x86_64) WindowsTerminal (codex_exec; 0.130.0)",
            "x-codex-beta-features": "terminal_resize_reflow",
        },
        "json": {
            "model": model,
            "input": [
                {
                    "type": "message",
                    "role": "user",
                    "content": [{"type": "input_text", "text": REQUEST_PROMPT}],
                }
            ],
            "tool_choice": "auto",
            "tools": [build_codex_tool()],
            "parallel_tool_calls": True,
            "stream": True,
            "store": False,
        },
    }


def build_claude_request(model: str, base_url: str = TEST_BASE_URL, api_key: str = "") -> dict:
    return {
        "url": f"{normalize_openai_base_url(base_url)}/messages?beta=true",
        "headers": {
            "Accept": "application/json",
            "Authorization": f"Bearer {api_key}",
            "Content-Type": "application/json",
            "User-Agent": "claude-cli/2.1.141 (external, sdk-cli)",
            "anthropic-beta": "claude-code-20250219,interleaved-thinking-2025-05-14,context-management-2025-06-27,prompt-caching-scope-2026-01-05,effort-2025-11-24",
            "anthropic-dangerous-direct-browser-access": "true",
            "anthropic-version": "2023-06-01",
            "x-api-key": api_key,
            "x-app": "cli",
        },
        "json": {
            "model": model,
            "messages": [
                {
                    "role": "user",
                    "content": [{"type": "text", "text": REQUEST_PROMPT}],
                }
            ],
            "system": [
                {"type": "text", "text": "You are a Claude agent, built on Anthropic's Claude Agent SDK."},
            ],
            "tools": [build_claude_tool()],
            "max_tokens": 1024,
            "thinking": {"type": "adaptive"},
            "stream": True,
        },
    }


def build_format_request(cli: str, api_key: str, model: str, base_url: str = TEST_BASE_URL) -> dict:
    if cli == "codex":
        return build_codex_request(model, base_url=base_url, api_key=api_key)
    if cli == "claude":
        return build_claude_request(model, base_url=base_url, api_key=api_key)
    raise ValueError(f"不支持的 CLI: {cli}")


def send_format_request(
    cli: str,
    api_key: str,
    model: str,
    base_url: str = TEST_BASE_URL,
    timeout: int = REQUEST_TIMEOUT_SECONDS,
) -> dict:
    request = build_format_request(cli, api_key, model, base_url)
    try:
        response = requests.post(
            request["url"],
            headers=request["headers"],
            json=request["json"],
            timeout=timeout,
        )
    except requests.Timeout:
        return {
            "ok": False,
            "status_code": None,
            "request": {"url": request["url"], "json": request["json"]},
            "response": None,
            "error": f"timeout after {timeout}s",
        }
    except requests.RequestException as e:
        return {
            "ok": False,
            "status_code": None,
            "request": {"url": request["url"], "json": request["json"]},
            "response": None,
            "error": str(e),
        }

    try:
        body = response.json()
    except ValueError:
        body = response.text[:1000]

    return {
        "ok": 200 <= response.status_code < 300,
        "status_code": response.status_code,
        "request": {"url": request["url"], "json": request["json"]},
        "response": body,
    }


def infer_cli(model: str) -> str:
    return "claude" if "claude" in model.lower() else "codex"

# skip_channels = {691}  # 这两个 channel 虽然标免费但实际不可用，先跳过 691 5.5有问题
def find_channels_with_model(channels: list, model_keyword: str) -> list[int]:
    """返回包含指定模型的 channel ID 列表"""
    matched = []
    for c in channels:
        ch_id = int(c["id"].split("/")[-1])
        # if ch_id in skip_channels:
        #     continue
        for m in c.get("models", []):
            if model_keyword.lower() in m.lower():
                matched.append(ch_id)
                break
        # if model_keyword in c.get("models", []):
        #     matched.append(ch_id)
    # raise
    return matched


def test_channel(
    api_key_id: int,
    channel_id: int,
    model: str,
    cli: str | None = None,
    settle_seconds: float = 0.5,
) -> dict:
    """用指定 channel 通过 Codex/Claude 协议格式请求测试是否可用。"""
    # 临时将 key 绑定为只有这一个 channel
    update_channels(api_key_id, [channel_id])
    time.sleep(settle_seconds)

    selected_cli = cli or infer_cli(model)
    api_key = get_api_key_secret(api_key_id)
    result = send_format_request(selected_cli, api_key, model)
    result["cli"] = selected_cli
    return result


def main():
    print("=" * 60)
    print(f"全链路: 寻找可用的免费 {TARGET_MODEL} channels")
    print("=" * 60)

    # --- Step 1: 爬取免费 channels ---
    print("\n[1] 爬取免费 channels...")
    with open(SCRIPT_DIR / "free_channels_with_models.json", encoding="utf-8") as f:
        free_channels = json.load(f)
    print(f"    免费 channel: {len(free_channels)} 个")

    # --- Step 2: 找包含 gpt-5.5-pro 的 channels ---
    with open(SCRIPT_DIR / "update.log", 'a+',encoding="utf-8") as f:
        f.write(
            f"[{time.strftime('%Y-%m-%d %H:%M:%S')}] "
            f"寻找包含 {TARGET_MODEL} 的 channels, 用 {TEST_MODEL} 通过 {TEST_CLI} 测试\n"
        )
        target_ids = find_channels_with_model(free_channels, TARGET_MODEL)
        f.write(f"    找到 {len(target_ids)} 个 channels 包含 {TARGET_MODEL}: {target_ids}\n")

        # if not target_ids:
        #     f.write("    未找到匹配的 channel，退出\n")
        #     return

        f.write(f"    将逐个更新 key #{KEY_ID} 为单 channel 后测试\n")

        # --- Step 3: 逐个用 Codex/Claude 协议格式请求测试 ---
        if SHOULD_TEST_MODEL:
            ok_channels = []
            for i, cid in enumerate(target_ids, 1):
                ch = next(c for c in free_channels if int(c["id"].split("/")[-1]) == cid)
                print(f"    [{i}/{len(target_ids)}] channel {cid} ({ch['name']})...", end=" ", flush=True)
                try:
                    result = test_channel(KEY_ID, cid, TEST_MODEL, cli=TEST_CLI)
                    if result["ok"]:
                        ok_channels.append(cid)
                        f.write(f"    channel {cid} ({ch['name']}) {result['cli']} 测试成功\n")
                    else:
                        # print(result)
                        try:
                            a=result['response']['error']
                        except:
                            a=result
                        f.write(f"    channel {cid} ({ch['name']}) {result.get('cli')} 测试失败: {a}\n")
                        print(a)
                except Exception as e:
                    raise
                    print(f"ERROR: {e}")
                time.sleep(1)
        else:
            ok_channels = target_ids
        f.write(f"\n    成功: {ok_channels}")

        # if ok_channels:
        #     update_channels(KEY_ID, ok_channels)
        #     f.write(f"    完成! key #{KEY_ID} -> {ok_channels}")
        # else:
        #     f.write("    没有可用的 channel，保留当前配置")
        update_channels(KEY_ID, ok_channels)
        f.write(f"    完成! key #{KEY_ID} -> {ok_channels}")

        key = get_api_key_profiles(KEY_ID)
        f.write(f"\n最终 key 配置:")
        f.write(f"  名称: {key['name']}")
        f.write(f"  Channels: {key['profiles']['profiles'][0]['channelIDs']}\n\n")



if __name__ == "__main__":
    # crawl_main()
    SHOULD_TEST_MODEL=True
    for KEY_ID, TARGET_MODEL, TEST_MODEL, TEST_CLI in [
        # (825, "terra", None, "codex"),
        # # (1893, "glm-5.2", "glm-5.2", "claude"),
        # # (1294, "GLM-5.2", "GLM-5.2", "claude"),
        # (4535, "opus-5", None, "claude"),
        # (58909, "fable", None, "claude"),
        # (58910, "k3", None, "claude"),
        (58944, "grok-4.5", "grok-4.5", "claude"),
    ]:
        main()
