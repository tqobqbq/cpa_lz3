#!/usr/bin/env python3
"""
hub.linux.do - 爬取所有模型都免费的 Channel

每个 Channel 有两处展示模型：
  1. supportedModels  — 频道支持的所有模型名预览列表
  2. channelModelPrices — 模型名对应的详细价格表

判断逻辑：
  - channelModelPrices 中所有已定价模型的价格必须为 0（免费）
  - supportedModels 不为空（有模型可提供）

结果保存为 free_channels_with_models.json
"""

import json
import time
from pathlib import Path

import requests

SCRIPT_DIR = Path(__file__).parent
PROXY = {"http": "http://127.0.0.1:10808", "https": "http://127.0.0.1:10808"}
GRAPHQL_URL = "https://hub.linux.do/admin/graphql"

COOKIE = (SCRIPT_DIR / "cookie.txt").read_text().strip().split("\t")[-1]
AUTH = (SCRIPT_DIR / "authorization.txt").read_text().strip().split("\t")[-1]
if AUTH and not AUTH.lower().startswith("bearer "):
    AUTH = f"Bearer {AUTH}"
HEADERS = {"Authorization": AUTH, "Content-Type": "application/json"}


skip_channels = {}  # 这两个 channel 虽然标免费但实际不可用，先跳过
def gql(query: str, variables: dict = None) -> dict:
    payload = {"query": query}
    if variables:
        payload["variables"] = variables
    for attempt in range(3):
        try:
            r = requests.post(
                GRAPHQL_URL, json=payload, headers=HEADERS,
                cookies={"cookie": COOKIE}, proxies=PROXY, timeout=60,
            )
            r.raise_for_status()
            return r.json()
        except Exception:
            if attempt == 2:
                raise
            time.sleep(2)


FETCH_CHANNELS_QUERY = """
query($first: Int!, $after: Cursor) {
  channels(
    first: $first, after: $after,
    where: {visibility: public, status: enabled}
  ) {
    totalCount
    pageInfo { hasNextPage endCursor }
    edges {
      node {
        id
        name
        tags
        supportedModels
        channelModelPrices {
          modelID
          price {
            items {
              itemCode
              pricing { mode flatFee usagePerUnit }
              multiplier
            }
          }
        }
      }
    }
  }
}
"""


def fetch_all_channels(batch=100):
    channels, cursor = [], None
    while True:
        variables = {"first": batch, "after": cursor}
        data = gql(FETCH_CHANNELS_QUERY, variables)["data"]["channels"]
        if cursor is None:
            print(f"总 channel 数: {data['totalCount']}，正在分页获取...")
        channels.extend(e["node"] for e in data["edges"])
        print(f"  已获取 {len(channels)}/{data['totalCount']}")
        if not data["pageInfo"]["hasNextPage"]:
            break
        cursor = data["pageInfo"]["endCursor"]
    return channels

def fetch_all_channels_and_summarize(batch=100):
    channels, cursor = [], None
    while True:
        variables = {"first": batch, "after": cursor}
        data = gql(FETCH_CHANNELS_QUERY, variables)["data"]["channels"]
        if cursor is None:
            print(f"总 channel 数: {data['totalCount']}，正在分页获取...")
        channels.extend(e["node"] for e in data["edges"])
        print(f"  已获取 {len(channels)}/{data['totalCount']}")
        if not data["pageInfo"]["hasNextPage"]:
            break
        cursor = data["pageInfo"]["endCursor"]
    return channels

def is_free(channel):
    """所有已定价模型的价格都为 0 则视为免费"""
    for p in channel.get("channelModelPrices", []):
        for item in p.get("price", {}).get("items", []):
            pricing = item.get("pricing", {})
            for field in ("flatFee", "usagePerUnit"):
                val = pricing.get(field)
                if val is not None and float(val) > 0:
                    return False
    return True


def summarize(channel):
    models = set(channel.get("supportedModels", []))
    for p in channel.get("channelModelPrices", []):
        mid = p.get("modelID", "unknown")
        for item in p.get("price", {}).get("items", []):
            for i in ("flatFee", "usagePerUnit"):
                val = item.get("pricing", {}).get(i)
                if val is not None and float(val) > 0:
                    return None
        models.add(mid)


    return {
        "id": channel["id"],
        "name": channel["name"],
        "tags": channel.get("tags", []),
        "models": list(models),
    }


def main():
    print("=" * 60)
    print("hub.linux.do - 爬取所有模型免费的 Channel")
    print("=" * 60)

    print("\n[1] 获取所有公开且启用的 channels...")
    all_channels = fetch_all_channels()

    print("\n[2] 筛选免费且包含模型的 channels...")
    free = []
    for c in all_channels:
        ch_id = int(c["id"].split("/")[-1])
        if ch_id in skip_channels:
            continue
        a= summarize(c)
        if a:
            free.append(a)


    print(f"    免费且有模型的 channel: {len(free)} 个")

    print("\n" + "=" * 60)
    print("结果")
    print("=" * 60)

    out = SCRIPT_DIR / "free_channels_with_models.json"
    out.write_text(json.dumps(free, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n已保存到: {out}")


if __name__ == "__main__":
    main()
