#!/usr/bin/env python3
"""
hub.linux.do Playground Chat API

POST https://hub.linux.do/admin/playground/chat
Headers:
  Authorization: Bearer <token>
  Content-Type: application/json
  X-Project-ID: gid://axonhub/Project/1
  X-API-Key-ID: gid://axonhub/APIKey/<id>
Body:
  model, temperature, max_tokens, system(可选),
  messages: [{role, content}]
Response:
  成功: SSE 流 data: {"type":"text-delta"|"reasoning-delta","delta":"..."}
  失败: {"error":{"code":400,"message":"..."}}
"""

import json
import requests
from pathlib import Path

SCRIPT_DIR = Path(__file__).parent
PROXY = {"http": "http://127.0.0.1:10808", "https": "http://127.0.0.1:10808"}
CHAT_URL = "https://hub.linux.do/admin/playground/chat"

COOKIE = (SCRIPT_DIR / "cookie.txt").read_text().strip().split("\t")[-1]
AUTH = (SCRIPT_DIR / "authorization.txt").read_text().strip().split("\t")[-1]
if AUTH and not AUTH.lower().startswith("bearer "):
    AUTH = f"Bearer {AUTH}"
PROJECT_ID = "gid://axonhub/Project/1"

import urllib3
urllib3.disable_warnings(urllib3.exceptions.InsecureRequestWarning)


def parse_cookies():
    cookies = {}
    for item in COOKIE.split("; "):
        if "=" in item:
            k, v = item.split("=", 1)
            cookies[k] = v
    return cookies


def chat(
    message: str,
    model: str = "GLM-5.1",
    api_key_id: int = 1893,
    temperature: float = 0.7,
    max_tokens: int = 1024,
    system: str = None,
    history: list = None,
    stream: bool = True,
) -> dict:
    """
    发送聊天请求

    Returns:
        成功: {"ok": True, "message_id": ..., "reasoning": ..., "content": ...}
        失败: {"ok": False, "code": ..., "message": ...}
    """
    messages = list(history or [])
    messages.append({"role": "user", "content": message})

    body = {
        "model": model,
        "temperature": temperature,
        "max_tokens": max_tokens,
        "messages": messages,
    }
    if system:
        body["system"] = system

    headers = {
        "Authorization": AUTH,
        "Content-Type": "application/json",
        "X-Project-ID": PROJECT_ID,
        "X-API-Key-ID": f"gid://axonhub/APIKey/{api_key_id}",
    }

    session = requests.Session()
    session.verify = False

    resp = session.post(
        CHAT_URL, json=body, headers=headers,
        cookies=parse_cookies(), proxies=PROXY,
        timeout=120, stream=True,
    )

    # 非 2xx：收集完整 body 解析错误
    if resp.status_code >= 300:
        try:
            err = resp.json()
            code = err.get("error", {}).get("code", resp.status_code)
            msg = err.get("error", {}).get("message", resp.text[:200])
        except Exception:
            code = resp.status_code
            msg = resp.text[:200]
        print(f"[ERROR] code={code}, message={msg}")
        return {"ok": False, "code": code, "message": msg}

    # 解析 SSE 流
    reasoning = []
    content = []
    msg_id = None
    error_info = None

    for line in resp.iter_lines(decode_unicode=True):
        if not line or not line.startswith("data: "):
            continue
        raw = line[6:]
        try:
            evt = json.loads(raw)
        except json.JSONDecodeError:
            continue

        t = evt.get("type", "")

        if t == "start":
            msg_id = evt.get("messageId", "")
        elif t == "reasoning-delta":
            delta = evt.get("delta", "")
            reasoning.append(delta)
            if stream:
                print(delta, end="", flush=True)
        elif t == "reasoning-end":
            if stream:
                print("\n\n---\n\n", end="", flush=True)
        elif t == "text-delta":
            delta = evt.get("delta", "")
            content.append(delta)
            if stream:
                print(delta, end="", flush=True)
        elif t == "error":
            error_info = {"code": evt.get("code", -1), "message": evt.get("message", "")}

    if stream:
        print()

    if error_info:
        print(f"[ERROR] code={error_info['code']}, message={error_info['message']}")
        return {"ok": False, **error_info}

    return {
        "ok": True,
        "message_id": msg_id,
        "reasoning": "".join(reasoning),
        "content": "".join(content),
    }


if __name__ == "__main__":
    print("模型: GLM-5.1 | Channel: 3269 | API Key: 1893")
    print("-" * 40)

    result = chat("Hi", model="GLM-5.1", api_key_id=1893)

    print("-" * 40)
    if result["ok"]:
        print(f"成功, content 长度: {len(result['content'])}")
    else:
        print(f"失败, code: {result['code']}, message: {result['message']}")
