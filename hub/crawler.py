#!/usr/bin/env python3
"""
hub.linux.do Marketplace Crawler
获取免费模型列表及其价格信息
"""

import json
import requests
from pathlib import Path

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


def graphql_request(query: str, variables: dict = None) -> dict:
    """发送GraphQL请求"""
    payload = {"query": query}
    if variables:
        payload["variables"] = variables

    response = requests.post(
        BASE_URL,
        json=payload,
        headers=HEADERS,
        cookies={"cookie": COOKIE},
        proxies=PROXY,
        timeout=30
    )
    response.raise_for_status()
    return response.json()


def get_all_models() -> list:
    """获取所有市场模型列表"""
    query = """
    {
        marketplaceModels {
            modelID
            providerCount
            displayModel {
                id
                name
                modelID
                group
                developer
                modelCard {
                    cost {
                        input
                        output
                        cacheRead
                        cacheWrite
                    }
                }
            }
        }
    }
    """
    result = graphql_request(query)
    return result["data"]["marketplaceModels"]


def get_model_detail(model_id: str) -> dict:
    """获取单个模型的详细信息，包括提供商价格"""
    query = """
    query($modelID: String!) {
        marketplaceModel(modelID: $modelID) {
            modelID
            displayModel {
                name
                modelCard {
                    cost {
                        input
                        output
                    }
                }
            }
            providers {
                modelID
                channel {
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
    result = graphql_request(query, {"modelID": model_id})
    return result["data"]["marketplaceModel"]


def find_free_models(models: list) -> list:
    """筛选免费模型（input=0 且 output=0）"""
    free_models = []
    for m in models:
        dm = m.get("displayModel")
        if dm and dm.get("modelCard"):
            cost = dm["modelCard"]["cost"]
            if cost["input"] == 0 and cost["output"] == 0:
                free_models.append(m)
        elif dm is None:
            # 没有displayModel信息的模型也可能是免费的
            free_models.append(m)
    return free_models


def extract_provider_pricing(model_detail: dict) -> list:
    """从模型详情中提取提供商价格信息"""
    providers_info = []
    for provider in model_detail.get("providers", []):
        channel = provider["channel"]
        for price_info in channel.get("channelModelPrices", []):
            if price_info["modelID"] == model_detail["modelID"]:
                items = price_info["price"]["items"]
                pricing = {
                    "provider_name": channel["name"],
                    "provider_tags": channel.get("tags", []),
                }
                for item in items:
                    code = item["itemCode"]
                    unit_price = item["pricing"].get("usagePerUnit", 0)
                    multiplier = item.get("multiplier", 1)
                    pricing[code] = {
                        "unit_price": unit_price,
                        "multiplier": multiplier
                    }
                providers_info.append(pricing)
                break
    return providers_info


def main():
    print("=" * 60)
    print("hub.linux.do Marketplace Crawler - 免费模型爬取")
    print("=" * 60)

    # 1. 获取所有模型
    print("\n[1] 正在获取所有模型列表...")
    all_models = get_all_models()
    print(f"    共获取 {len(all_models)} 个模型")

    # 2. 筛选免费模型
    print("\n[2] 筛选免费模型...")
    free_models = find_free_models(all_models)
    print(f"    找到 {len(free_models)} 个免费模型")

    # 3. 输出免费模型列表
    print("\n" + "=" * 60)
    print("免费模型列表 (按提供商数量排序)")
    print("=" * 60)

    # 按提供商数量排序
    free_models.sort(key=lambda x: x["providerCount"], reverse=True)

    results = []
    for m in free_models:
        dm = m.get("displayModel")
        name = dm["name"] if dm else m["modelID"]
        cost = dm["modelCard"]["cost"] if dm and dm.get("modelCard") else {"input": "N/A", "output": "N/A"}
        providers = m["providerCount"]
        group = dm.get("group", "unknown") if dm else "unknown"

        model_info = {
            "model_id": m["modelID"],
            "name": name,
            "group": group,
            "provider_count": providers,
            "cost": cost
        }
        results.append(model_info)

        print(f"\n模型: {name}")
        print(f"  ID: {m['modelID']}")
        print(f"  分组: {group}")
        print(f"  提供商数量: {providers}")
        print(f"  价格: input={cost['input']}, output={cost['output']}")

    # 4. 保存结果
    output_file = SCRIPT_DIR / "free_models.json"
    with open(output_file, "w", encoding="utf-8") as f:
        json.dump(results, f, ensure_ascii=False, indent=2)
    print(f"\n\n结果已保存到: {output_file}")

    # 5. 可选：获取热门免费模型的详细价格
    print("\n" + "=" * 60)
    print("热门免费模型的提供商价格详情 (前5个)")
    print("=" * 60)

    for m in free_models[:5]:
        model_id = m["modelID"]
        print(f"\n正在获取 {model_id} 的提供商价格...")
        try:
            detail = get_model_detail(model_id)
            providers_pricing = extract_provider_pricing(detail)

            print(f"\n模型: {detail['displayModel']['name'] if detail.get('displayModel') else model_id}")
            print(f"提供商数量: {len(providers_pricing)}")

            # 显示前3个提供商的价格
            for p in providers_pricing[:3]:
                print(f"\n  提供商: {p['provider_name']}")
                if p.get("prompt_tokens"):
                    print(f"    输入价格: {p['prompt_tokens']['unit_price']} / 1M tokens")
                if p.get("completion_tokens"):
                    print(f"    输出价格: {p['completion_tokens']['unit_price']} / 1M tokens")
        except Exception as e:
            print(f"  获取失败: {e}")

    return results


if __name__ == "__main__":
    main()
