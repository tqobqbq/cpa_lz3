#!/usr/bin/env python3
"""
hub.linux.do Channel Crawler
爬取所有模型都是免费的 channel（提供商）
"""

import json
import requests
from pathlib import Path
from typing import Optional

# 配置
PROXY = {"http": "http://127.0.0.1:10808", "https": "http://127.0.0.1:10808"}
BASE_URL = "https://hub.linux.do/admin/graphql"

# 读取认证信息
SCRIPT_DIR = Path(__file__).parent
COOKIE = (SCRIPT_DIR / "cookie.txt").read_text().strip().split("\t")[-1]
AUTH = (SCRIPT_DIR / "authorization.txt").read_text().strip().split("\t")[-1]
if AUTH and not AUTH.lower().startswith("bearer "):
    AUTH = f"Bearer {AUTH}"

HEADERS = {
    "Authorization": AUTH,
    "Content-Type": "application/json",
}


def graphql_request(query: str, variables: dict = None, retry: int = 3) -> dict:
    """发送GraphQL请求"""
    payload = {"query": query}
    if variables:
        payload["variables"] = variables

    last_error = None
    for attempt in range(retry):
        try:
            response = requests.post(
                BASE_URL,
                json=payload,
                headers=HEADERS,
                cookies={"cookie": COOKIE},
                proxies=PROXY,
                timeout=60
            )
            response.raise_for_status()
            return response.json()
        except Exception as e:
            last_error = e
            import time
            time.sleep(2)  # 等待2秒后重试

    raise last_error


def get_all_channels(batch_size: int = 100) -> list:
    """分页获取所有公开且启用的 channels"""
    all_channels = []
    cursor = None
    total = None

    query = """
    query($first: Int, $after: Cursor) {
        channels(first: $first, after: $after, where: {visibility: public, status: enabled}) {
            totalCount
            pageInfo {
                hasNextPage
                endCursor
            }
            edges {
                node {
                    id
                    name
                    tags
                    channelModelPrices {
                        modelID
                        price {
                            items {
                                itemCode
                                pricing {
                                    mode
                                    flatFee
                                    usagePerUnit
                                }
                                multiplier
                            }
                        }
                    }
                }
            }
        }
    }
    """

    while True:
        variables = {"first": batch_size}
        if cursor:
            variables["after"] = cursor

        result = graphql_request(query, variables)
        data = result["data"]["channels"]

        if total is None:
            total = data["totalCount"]
            print(f"总共 {total} 个公开且启用的 channel，正在获取...")

        edges = data["edges"]
        all_channels.extend([e["node"] for e in edges])
        print(f"  已获取 {len(all_channels)}/{total}")

        if not data["pageInfo"]["hasNextPage"]:
            break

        cursor = data["pageInfo"]["endCursor"]

    return all_channels


def is_free_channel(channel: dict) -> bool:
    """判断一个 channel 是否所有模型都免费"""
    prices = channel.get("channelModelPrices", [])

    if not prices:
        # 没有价格信息的 channel 可能是免费的（需要人工确认）
        return True

    for price_info in prices:
        items = price_info.get("price", {}).get("items", [])

        for item in items:
            pricing = item.get("pricing", {})
            mode = pricing.get("mode", "")

            # 检查是否有非零价格
            flat_fee = pricing.get("flatFee")
            usage_per_unit = pricing.get("usagePerUnit")

            # 如果有非零的 flatFee 或 usagePerUnit，则不是免费的
            if flat_fee is not None and float(flat_fee) > 0:
                return False
            if usage_per_unit is not None and float(usage_per_unit) > 0:
                return False

    return True


def get_free_channels(channels: list) -> list:
    """筛选出所有模型都免费的 channels"""
    free_channels = []

    for channel in channels:
        if is_free_channel(channel):
            free_channels.append(channel)

    return free_channels


def analyze_channel(channel: dict) -> dict:
    """分析 channel 的价格信息"""
    prices = channel.get("channelModelPrices", [])
    models_info = []

    for price_info in prices:
        model_id = price_info.get("modelID", "unknown")
        items = price_info.get("price", {}).get("items", [])

        model_pricing = {"model_id": model_id, "items": {}}

        for item in items:
            item_code = item.get("itemCode", "unknown")
            pricing = item.get("pricing", {})
            flat_fee = pricing.get("flatFee")
            usage_per_unit = pricing.get("usagePerUnit")
            multiplier = item.get("multiplier")

            model_pricing["items"][item_code] = {
                "flat_fee": flat_fee,
                "usage_per_unit": usage_per_unit,
                "multiplier": multiplier
            }

        models_info.append(model_pricing)

    return {
        "id": channel["id"],
        "name": channel["name"],
        "tags": channel.get("tags", []),
        "model_count": len(prices),
        "models": models_info
    }


def main():
    print("=" * 70)
    print("hub.linux.do Channel Crawler - 爬取所有模型免费的 Channel")
    print("=" * 70)

    # 1. 获取所有公开且启用的 channels
    print("\n[1] 正在获取所有公开且启用的 channels...")
    all_channels = get_all_channels(batch_size=100)
    print(f"    共获取 {len(all_channels)} 个 channels")

    # 2. 筛选免费的 channels
    print("\n[2] 正在筛选所有模型都免费的 channels...")
    free_channels = get_free_channels(all_channels)
    print(f"    找到 {len(free_channels)} 个免费 channels")

    # 3. 分析并输出结果
    print("\n" + "=" * 70)
    print("免费 Channel 列表")
    print("=" * 70)

    results = []
    for channel in free_channels:
        info = analyze_channel(channel)
        results.append(info)

        print(f"\nChannel: {info['name']}")
        print(f"  ID: {info['id']}")
        print(f"  标签: {info['tags']}")
        print(f"  模型数量: {info['model_count']}")
        if info['model_count'] > 0 and info['model_count'] <= 10:
            print(f"  模型列表:")
            for m in info['models'][:10]:
                print(f"    - {m['model_id']}")

    # 4. 保存结果
    output_file = SCRIPT_DIR / "free_channels.json"
    with open(output_file, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    print(f"\n\n结果已保存到: {output_file}")

    # 5. 统计摘要
    print("\n" + "=" * 70)
    print("统计摘要")
    print("=" * 70)
    print(f"总 channel 数: {len(all_channels)}")
    print(f"免费 channel 数: {len(free_channels)}")
    print(f"免费比例: {len(free_channels)/len(all_channels)*100:.1f}%")

    # 按模型数量排序，显示前10
    print("\n模型数量最多的免费 channels (前10):")
    sorted_results = sorted(results, key=lambda x: x['model_count'], reverse=True)
    for i, r in enumerate(sorted_results[:10], 1):
        print(f"  {i}. {r['name']} - {r['model_count']} 个模型")

    return results


if __name__ == "__main__":
    main()
