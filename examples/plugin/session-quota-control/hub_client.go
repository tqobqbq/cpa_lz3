package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type hubSettings struct {
	Authorization string  `json:"authorization,omitempty"`
	Cookie        string  `json:"cookie,omitempty"`
	KeyID         int64   `json:"key_id"`
	Model         string  `json:"model"`
	Filter        string  `json:"filter"`
	Effort        string  `json:"effort"`
	Protocol      string  `json:"protocol"`
	GraphQLURL    string  `json:"graphql_url"`
	APIBaseURL    string  `json:"api_base_url"`
	ProjectID     int64   `json:"project_id"`
	ProxyURL      string  `json:"proxy_url"`
	ChannelIDs    []int64 `json:"channel_ids"`
}

func defaultHubSettings() hubSettings {
	return hubSettings{Model: "gpt-6-astra", Effort: "low", Protocol: "auto", ProjectID: 1,
		GraphQLURL: "https://hub.linux.do/admin/graphql", APIBaseURL: "https://hub.oaifree.com"}
}

func (s hubSettings) public() hubSettings {
	s.Authorization, s.Cookie = "", ""
	return s
}

func (s hubSettings) validate() error {
	for _, address := range []string{s.GraphQLURL, s.APIBaseURL} {
		u, errParse := url.Parse(address)
		if errParse != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("请输入有效的 HTTP(S) 接口地址，不含账号、查询参数或片段")
		}
	}
	if s.KeyID < 0 || s.KeyID > 9007199254740991 || s.ProjectID < 1 || s.ProjectID > 9007199254740991 {
		return fmt.Errorf("KEY_ID 和 Project ID 必须为有效整数")
	}
	if !slices.Contains([]string{"auto", "codex", "claude"}, s.Protocol) || !slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, s.Effort) {
		return fmt.Errorf("请选择有效的请求协议和 Effort")
	}
	if strings.ContainsAny(s.Authorization+s.Cookie, "\r\n") || len(s.Authorization)+len(s.Cookie) > 32768 || len(s.Model) > 200 || len(s.Filter) > 200 {
		return fmt.Errorf("配置内容格式错误或过长")
	}
	if _, errParse := proxyutil.Parse(s.ProxyURL); errParse != nil {
		return fmt.Errorf("代理地址无效")
	}
	return nil
}

type hubChannel struct {
	ID     int64    `json:"id"`
	Name   string   `json:"name"`
	Models []string `json:"models,omitempty"`
	Tags   []string `json:"tags,omitempty"`
}

type hubProfiles struct {
	Active   string           `json:"activeProfile"`
	Profiles []map[string]any `json:"profiles"`
}

type hubKey struct {
	ID       string      `json:"id"`
	Name     string      `json:"name"`
	Key      string      `json:"key"`
	Profiles hubProfiles `json:"profiles"`
}

type hubClient struct {
	settings hubSettings
	http     *http.Client
}

func newHubClient(s hubSettings) (*hubClient, error) {
	transport, _, errBuild := proxyutil.BuildHTTPTransport(s.ProxyURL)
	if errBuild != nil {
		return nil, fmt.Errorf("无法创建 Hub 代理连接")
	}
	if transport == nil {
		if standard, ok := http.DefaultTransport.(*http.Transport); ok {
			transport = standard.Clone()
		} else {
			transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
		}
	}
	transport.TLSHandshakeTimeout, transport.ResponseHeaderTimeout, transport.IdleConnTimeout = 0, 0, 0
	// The shared SOCKS helper predates context-aware dialing. A cancelled Hub job
	// must also release a connection still being acquired through SOCKS.
	setting, _ := proxyutil.Parse(s.ProxyURL)
	if setting.URL != nil && strings.HasPrefix(setting.URL.Scheme, "socks5") {
		dialer, errDialer := proxy.FromURL(setting.URL, &net.Dialer{})
		if errDialer != nil {
			return nil, fmt.Errorf("无法创建 Hub SOCKS 连接")
		}
		contextDialer, ok := dialer.(proxy.ContextDialer)
		if !ok {
			return nil, fmt.Errorf("Hub SOCKS 连接不支持取消")
		}
		transport.DialContext = contextDialer.DialContext
	}
	return &hubClient{settings: s, http: &http.Client{Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func closeHubBody(body io.Closer) {
	if errClose := body.Close(); errClose != nil {
		log.Debug("session-quota-control: could not close Hub response")
	}
}

func (c *hubClient) redact(message string, extra ...string) string {
	for _, secret := range append(extra, c.settings.Authorization, strings.TrimPrefix(c.settings.Authorization, "Bearer "), c.settings.Cookie) {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	return string([]rune(message)[:min(1000, len([]rune(message)))])
}

func (c *hubClient) gql(ctx context.Context, operation, query string, variables any, target any) error {
	body, errMarshal := json.Marshal(map[string]any{"operationName": operation, "query": query, "variables": variables})
	if errMarshal != nil {
		return fmt.Errorf("编码 Hub 请求失败")
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, c.settings.GraphQLURL, bytes.NewReader(body))
	if errRequest != nil {
		return fmt.Errorf("创建 Hub 请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", c.settings.Authorization)
	req.Header.Set("X-Project-ID", fmt.Sprintf("gid://axonhub/Project/%d", c.settings.ProjectID))
	if c.settings.Cookie != "" {
		req.Header.Set("Cookie", c.settings.Cookie)
	}
	resp, errDo := c.http.Do(req)
	if errDo != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Hub 网络请求失败，请检查地址和代理")
	}
	defer closeHubBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Hub 返回 HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&envelope); errDecode != nil {
		return fmt.Errorf("Hub 返回了无效的 GraphQL 响应")
	}
	if len(envelope.Errors) > 0 {
		return fmt.Errorf("Hub: %s", c.redact(envelope.Errors[0].Message))
	}
	if len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return fmt.Errorf("Hub 未返回数据")
	}
	if errDecode := json.Unmarshal(envelope.Data, target); errDecode != nil {
		return fmt.Errorf("无法解析 Hub 数据")
	}
	return nil
}

const hubProfileFields = `activeProfile profiles {
 name modelMappings { from to } channelIDs channelTags channelTagsMatchMode modelIDs loadBalanceStrategy
 routingPolicy { channelWeights { channelID weight } providerWeights { provider weight } includeProviders excludeProviders
 allowChannelAPIKeys { channelID keyHashes } excludeChannelAPIKeys { channelID keyHashes } fallbackChannelIDs excludeChannelIDs }
 channelBindingMode dynamicChannelStrategy { mode selectionPolicy maxChannels minChannels maxPriceMultiplier maxLatencyMs minSuccessRate
 onlyOfficial includeTags excludeTags excludeChannelIDs fallbackChannelIDs }
 quota { requests totalTokens cost period { type pastDuration { value unit } calendarDuration { unit } } }
}`

func (c *hubClient) key(ctx context.Context) (*hubKey, error) {
	var result struct {
		Node *hubKey `json:"node"`
	}
	errQuery := c.gql(ctx, "GetApiKey", `query GetApiKey($id: ID!) { node(id: $id) { ... on APIKey { id name key profiles { `+hubProfileFields+` } } } }`,
		map[string]any{"id": fmt.Sprintf("gid://axonhub/APIKey/%d", c.settings.KeyID)}, &result)
	if errQuery != nil {
		return nil, errQuery
	}
	if result.Node == nil {
		return nil, fmt.Errorf("KEY_ID 不存在或无权访问")
	}
	return result.Node, nil
}

func usableHubSecret(value string) bool {
	value = strings.TrimSpace(value)
	lower := strings.ToLower(value)
	return value != "" && !strings.ContainsAny(value, "*…") && !strings.Contains(value, "...") && !strings.Contains(lower, "redacted") && !strings.Contains(lower, "masked") && lower != "null" && lower != "none"
}

func (c *hubClient) secret(ctx context.Context, key *hubKey) (string, error) {
	if usableHubSecret(key.Key) {
		return strings.TrimSpace(key.Key), nil
	}
	var me struct {
		Me struct {
			ID string `json:"id"`
		} `json:"me"`
	}
	if errMe := c.gql(ctx, "CurrentUser", `query CurrentUser { me { id } }`, nil, &me); errMe != nil {
		return "", errMe
	}
	var cursor string
	for page := 0; page < 100; page++ {
		var result struct {
			APIKeys struct {
				Edges []struct {
					Node hubKey `json:"node"`
				} `json:"edges"`
				PageInfo struct {
					HasNext bool   `json:"hasNextPage"`
					End     string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"apiKeys"`
		}
		variables := map[string]any{"first": 100, "after": nil, "where": map[string]any{"userID": me.Me.ID, "typeNotIn": []string{"noauth"}}}
		if cursor != "" {
			variables["after"] = cursor
		}
		if errQuery := c.gql(ctx, "GetApiKeys", `query GetApiKeys($first: Int, $after: Cursor, $where: APIKeyWhereInput) {
 apiKeys(first: $first, after: $after, where: $where) { edges { node { id key } } pageInfo { hasNextPage endCursor } } }`, variables, &result); errQuery != nil {
			return "", errQuery
		}
		for _, edge := range result.APIKeys.Edges {
			if edge.Node.ID == key.ID && usableHubSecret(edge.Node.Key) {
				return strings.TrimSpace(edge.Node.Key), nil
			}
		}
		info := result.APIKeys.PageInfo
		if !info.HasNext {
			break
		}
		if info.End == "" || info.End == cursor {
			return "", fmt.Errorf("Hub Key 分页游标无效")
		}
		cursor = info.End
	}
	return "", fmt.Errorf("Hub 未返回该 KEY_ID 的调用密钥，请检查 Authorization 的访问权限")
}

func (c *hubClient) setProfiles(ctx context.Context, profiles hubProfiles) error {
	var result struct {
		Key *hubKey `json:"updateAPIKeyProfiles"`
	}
	errUpdate := c.gql(ctx, "UpdateAPIKeyProfiles", `mutation UpdateAPIKeyProfiles($id: ID!, $input: UpdateAPIKeyProfilesInput!) {
 updateAPIKeyProfiles(id: $id, input: $input) { id profiles { `+hubProfileFields+` } } }`,
		map[string]any{"id": fmt.Sprintf("gid://axonhub/APIKey/%d", c.settings.KeyID), "input": profiles}, &result)
	if errUpdate != nil {
		return errUpdate
	}
	if result.Key == nil || !equalHubProfiles(result.Key.Profiles, profiles) {
		return fmt.Errorf("Hub 未确认渠道绑定配置，请恢复后重试")
	}
	return nil
}

func equalHubProfiles(a, b hubProfiles) bool {
	// Normalize integer values and map types through the wire representation.
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	var x, y any
	_ = json.Unmarshal(left, &x)
	_ = json.Unmarshal(right, &y)
	return reflect.DeepEqual(x, y)
}

func (p hubProfiles) bound(ids []int64) hubProfiles {
	raw, _ := json.Marshal(p)
	var out hubProfiles
	_ = json.Unmarshal(raw, &out)
	if len(out.Profiles) == 0 {
		out.Profiles = []map[string]any{{"name": "default", "modelMappings": []any{}, "modelIDs": []any{}, "channelTagsMatchMode": "any", "loadBalanceStrategy": nil, "quota": nil}}
		out.Active = "default"
	}
	index := slices.IndexFunc(out.Profiles, func(profile map[string]any) bool { return profile["name"] == out.Active })
	if index < 0 {
		index = 0
		out.Active, _ = out.Profiles[0]["name"].(string)
	}
	profile := out.Profiles[index]
	profile["channelBindingMode"], profile["channelIDs"] = "manual", ids
	// Remove alternate routes while retaining quota, model mappings and provider
	// restrictions. An explicit binding must not fall back to untested channels.
	profile["channelTags"], profile["dynamicChannelStrategy"] = []any{}, nil
	if policy, ok := profile["routingPolicy"].(map[string]any); ok {
		policy["fallbackChannelIDs"] = []any{}
	}
	return out
}

func (p hubProfiles) channelIDs() []int64 {
	for i, profile := range p.Profiles {
		if profile["name"] != p.Active && !(p.Active == "" && i == 0) {
			continue
		}
		raw, _ := json.Marshal(profile["channelIDs"])
		var ids []int64
		_ = json.Unmarshal(raw, &ids)
		return ids
	}
	return nil
}

func (c *hubClient) discover(ctx context.Context) ([]hubChannel, error) {
	channels := []hubChannel{}
	cursor := ""
	for page := 0; page < 1000; page++ {
		var result struct {
			Channels struct {
				Edges []struct {
					Node struct {
						ID     string   `json:"id"`
						Name   string   `json:"name"`
						Tags   []string `json:"tags"`
						Models []string `json:"supportedModels"`
						Prices []struct {
							Model string `json:"modelID"`
							Price struct {
								Items []struct {
									Pricing struct {
										Mode  string   `json:"mode"`
										Flat  *float64 `json:"flatFee"`
										Usage *float64 `json:"usagePerUnit"`
									} `json:"pricing"`
								} `json:"items"`
							} `json:"price"`
						} `json:"channelModelPrices"`
					} `json:"node"`
				} `json:"edges"`
				Info struct {
					Next bool   `json:"hasNextPage"`
					End  string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"channels"`
		}
		variables := map[string]any{"first": 100, "after": nil}
		if cursor != "" {
			variables["after"] = cursor
		}
		if errQuery := c.gql(ctx, "FreeChannels", `query FreeChannels($first: Int!, $after: Cursor) {
 channels(first: $first, after: $after, where: {visibility: public, status: enabled}) {
 pageInfo { hasNextPage endCursor } edges { node { id name tags supportedModels
 channelModelPrices { modelID price { items { pricing { mode flatFee usagePerUnit } } } } } } } }`, variables, &result); errQuery != nil {
			return nil, errQuery
		}
		for _, edge := range result.Channels.Edges {
			node := edge.Node
			free, priced := true, false
			models := append([]string(nil), node.Models...)
			for _, price := range node.Prices {
				if price.Model != "" {
					models = append(models, price.Model)
				}
				if len(price.Price.Items) == 0 {
					free = false
				}
				for _, item := range price.Price.Items {
					if item.Pricing.Flat == nil && item.Pricing.Usage == nil {
						free = false
					}
					for _, amount := range []*float64{item.Pricing.Flat, item.Pricing.Usage} {
						if amount != nil {
							priced = true
							if *amount != 0 || math.IsNaN(*amount) {
								free = false
							}
						}
					}
				}
			}
			id, errID := strconv.ParseInt(node.ID[strings.LastIndex(node.ID, "/")+1:], 10, 64)
			if !free || !priced || len(models) == 0 || errID != nil || id <= 0 {
				continue
			}
			slices.Sort(models)
			channels = append(channels, hubChannel{ID: id, Name: node.Name, Models: slices.Compact(models), Tags: node.Tags})
		}
		if !result.Channels.Info.Next {
			slices.SortFunc(channels, func(a, b hubChannel) int {
				if a.ID < b.ID {
					return -1
				}
				if a.ID > b.ID {
					return 1
				}
				return 0
			})
			return slices.CompactFunc(channels, func(a, b hubChannel) bool { return a.ID == b.ID }), nil
		}
		if result.Channels.Info.End == "" || result.Channels.Info.End == cursor {
			return nil, fmt.Errorf("Hub 渠道分页游标无效")
		}
		cursor = result.Channels.Info.End
	}
	return nil, fmt.Errorf("Hub 渠道分页超出上限")
}
