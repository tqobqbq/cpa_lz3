#!/usr/bin/env python3
"""
hub.linux.do - API Key 管理

用法：
  python3 api_key_manager.py                          # 查看 key 配置
  python3 api_key_manager.py --add 185                # 添加 channel
  python3 api_key_manager.py --remove 185             # 移除 channel
  python3 api_key_manager.py --set 185 3269 691       # 直接设置 channel 列表
"""

import json
import time
import argparse
from copy import deepcopy
from pathlib import Path

import requests

SCRIPT_DIR = Path(__file__).parent
PROXY = {"http": "http://127.0.0.1:10808", "https": "http://127.0.0.1:10808"}
GRAPHQL_URL = "https://hub.linux.do/admin/graphql"

PROJECT_ID = "gid://axonhub/Project/1"
COOKIE = (SCRIPT_DIR / "cookie.txt").read_text().strip().split("\t")[-1]
AUTH = (SCRIPT_DIR / "authorization.txt").read_text().strip().split("\t")[-1]
if AUTH and not AUTH.lower().startswith("bearer "):
    AUTH = f"Bearer {AUTH}"
HEADERS = {
    "Authorization": AUTH,
    "Content-Type": "application/json",
    "X-Project-ID": PROJECT_ID,
}

UPDATE_PROFILES_MUTATION = """
mutation UpdateAPIKeyProfiles($id: ID!, $input: UpdateAPIKeyProfilesInput!) {
  updateAPIKeyProfiles(id: $id, input: $input) {
    id
    name
    status
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
"""


DEFAULT_PROFILE_NAME = "default"


def _default_profile(name: str = DEFAULT_PROFILE_NAME) -> dict:
    """返回当前网页创建 API key 时使用的默认 profile 结构。"""
    return {
        "name": name,
        "modelMappings": [],
        "channelIDs": [],
        "channelTags": [],
        "channelTagsMatchMode": "any",
        "modelIDs": [],
        "channelBindingMode": "manual",
        "dynamicChannelStrategy": None,
    }


def _profile_update_input(
    profile: dict,
    *,
    channel_ids: list[int] | None = None,
) -> dict:
    """
    将查询结果转换成新版网页提交的 UpdateAPIKeyProfilesInput。

    ``channel_ids`` 不为 None 时表示正在修改这个 profile。显式设置 channel
    列表等价于网页中的 manual 模式，因此同时关闭 dynamic 策略。
    """
    result = _default_profile(profile.get("name") or DEFAULT_PROFILE_NAME)
    result.update({
        "modelMappings": deepcopy(profile.get("modelMappings") or []),
        "channelTagsMatchMode": profile.get("channelTagsMatchMode") or "any",
        "modelIDs": deepcopy(profile.get("modelIDs") or []),
    })

    if channel_ids is not None:
        mode = "manual"
        result["channelIDs"] = list(channel_ids)
        result["channelTags"] = deepcopy(profile.get("channelTags") or [])
        result["dynamicChannelStrategy"] = None
    else:
        mode = "dynamic" if profile.get("channelBindingMode") == "dynamic" else "manual"
        result["channelIDs"] = (
            deepcopy(profile.get("channelIDs") or []) if mode == "manual" else []
        )
        result["channelTags"] = (
            deepcopy(profile.get("channelTags") or []) if mode == "manual" else []
        )
        result["dynamicChannelStrategy"] = (
            deepcopy(profile.get("dynamicChannelStrategy")) if mode == "dynamic" else None
        )
    result["channelBindingMode"] = mode

    # 这些字段在较早版本中不存在。只有查询结果实际包含时才回传，既保留
    # 已有配置，也兼容服务端尚未为旧 key 补齐字段的情况。
    for field in ("loadBalanceStrategy", "routingPolicy", "quota"):
        if field in profile:
            result[field] = deepcopy(profile[field])

    return result


def parse_cookies():
    cookies = {}
    for item in COOKIE.split("; "):
        if "=" in item:
            k, v = item.split("=", 1)
            cookies[k] = v
    return cookies


def gql(query: str, variables: dict = None, operation_name: str = None) -> dict:
    payload = {"query": query}
    if variables:
        payload["variables"] = variables
    if operation_name:
        payload["operationName"] = operation_name
    for attempt in range(3):
        try:
            r = requests.post(
                GRAPHQL_URL, json=payload, headers=HEADERS,
                cookies=parse_cookies(), proxies=PROXY, timeout=60,
            )
            r.raise_for_status()
            result = r.json()
            if "errors" in result:
                msgs = [e["message"] for e in result["errors"]]
                raise RuntimeError(f"GraphQL error: {msgs}")
            return result
        except Exception:
            if attempt == 2:
                raise
            time.sleep(2)


def get_api_key_profiles(key_id: int) -> dict:
    """获取 API key 的 profiles"""
    result = gql("""
    query GetApiKey($id: ID!) {
      node(id: $id) {
        ... on APIKey {
          id
          name
          status
          profiles {
            activeProfile
            profiles {
              name
              modelMappings { from to }
              channelIDs
              channelTags
              channelTagsMatchMode
              channelBindingMode
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
    """, {"id": f"gid://axonhub/APIKey/{key_id}"}, operation_name="GetApiKey")
    return result["data"]["node"]


def _get_current_channels(key_id: int) -> tuple[str, list[dict], list[int]]:
    """获取 active profile 名称、所有 profiles、当前 channelIDs。返回 (active_name, profiles, current_ids)"""
    key = get_api_key_profiles(key_id)
    if not key:
        raise RuntimeError(f"API key #{key_id} 不存在或无权访问")

    # 新版网页允许刚创建的 key 暂时没有 profiles，并在第一次编辑时补一个
    # default profile。这里复刻相同的回退逻辑。
    profiles_data = key.get("profiles") or {}
    profiles = profiles_data.get("profiles") or []
    if not profiles:
        profile = _default_profile()
        return DEFAULT_PROFILE_NAME, [profile], []

    active_name = profiles_data.get("activeProfile")
    if not active_name or not any(p.get("name") == active_name for p in profiles):
        active_name = profiles[0].get("name") or DEFAULT_PROFILE_NAME

    for p in profiles:
        if p["name"] == active_name:
            current_ids = list(p.get("channelIDs") or [])
            return active_name, profiles, current_ids

    raise RuntimeError(f"Active profile '{active_name}' 不存在")


def update_channels(key_id: int, channel_ids: list[int]) -> dict:
    """
    直接设置 API key 的 channel 列表（整体替换）

    Args:
        key_id: API key 数字 ID
        channel_ids: 目标 channel ID 列表
    """
    active_name, profiles, _ = _get_current_channels(key_id)

    new_profiles = [
        _profile_update_input(
            p,
            channel_ids=channel_ids if p.get("name") == active_name else None,
        )
        for p in profiles
    ]

    result = gql(UPDATE_PROFILES_MUTATION, {
        "id": f"gid://axonhub/APIKey/{key_id}",
        "input": {"activeProfile": active_name, "profiles": new_profiles},
    }, operation_name="UpdateAPIKeyProfiles")
    return result["data"]["updateAPIKeyProfiles"]


def add_channel_to_key(key_id: int, channel_id: int) -> dict:
    """添加 channel 到 key"""
    active_name, _, current_ids = _get_current_channels(key_id)
    if channel_id in current_ids:
        print(f"Channel #{channel_id} 已存在，无需添加")
        return get_api_key_profiles(key_id)

    new_ids = current_ids + [channel_id]
    print(f"添加 Channel #{channel_id}")
    print(f"更新后: {new_ids}")
    return update_channels(key_id, new_ids)


def remove_channel_from_key(key_id: int, channel_id: int) -> dict:
    """从 key 移除 channel"""
    active_name, _, current_ids = _get_current_channels(key_id)
    if channel_id not in current_ids:
        print(f"Channel #{channel_id} 不在列表中，无需移除")
        return get_api_key_profiles(key_id)

    new_ids = [c for c in current_ids if c != channel_id]
    print(f"移除 Channel #{channel_id}")
    print(f"更新后: {new_ids}")
    return update_channels(key_id, new_ids)


def show_key_info(key_id: int):
    """显示 API key 信息"""
    try:
        key = get_api_key_profiles(key_id)
        if not key:
            print(f"无法获取 API key #{key_id}")
            return
        print(f"API Key: {key['name']} (#{key_id})")
        print(f"Status: {key['status']}")
        profiles = key.get("profiles") or {}
        profile_items = profiles.get("profiles") or []
        active_profile = profiles.get("activeProfile")
        print(f"Active Profile: {active_profile or '未配置'}")
        if not profile_items:
            print("  Profiles: 未配置（首次设置 channel 时将创建 default profile）")
            return
        for p in profile_items:
            print(f"\n  Profile: {p['name']}")
            print(f"    ChannelIDs: {p.get('channelIDs', [])}")
            print(f"    ChannelTags: {p.get('channelTags', [])}")
            print(f"    BindingMode: {p.get('channelBindingMode', 'N/A')}")
            print(f"    ModelIDs: {p.get('modelIDs', [])}")
    except RuntimeError as e:
        print(f"错误: {e}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="hub.linux.do API Key 管理")
    parser.add_argument("--key", type=int, default=825, help="API key ID (默认 825)")
    parser.add_argument("--add", type=int, metavar="CHANNEL_ID", help="添加 channel")
    parser.add_argument("--remove", type=int, metavar="CHANNEL_ID", help="移除 channel")
    parser.add_argument("--set", type=int, nargs="+", metavar="CHANNEL_ID", help="直接设置 channel 列表")

    args = parser.parse_args()

    if args.add:
        result = add_channel_to_key(args.key, args.add)
        print(f"成功! {result['name']} -> {result['profiles']['profiles'][0]['channelIDs']}")
    elif args.remove:
        result = remove_channel_from_key(args.key, args.remove)
        print(f"成功! {result['name']} -> {result['profiles']['profiles'][0]['channelIDs']}")
    elif args.set:
        result = update_channels(args.key, args.set)
        print(f"成功! {result['name']} -> {result['profiles']['profiles'][0]['channelIDs']}")
    else:
        show_key_info(args.key)
