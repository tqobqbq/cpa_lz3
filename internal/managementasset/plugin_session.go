package managementasset

import (
	"bytes"
	_ "embed"
	"regexp"
)

//go:embed plugin_session.js
var pluginSessionBridge []byte

//go:embed credential_quota.js
var credentialQuotaBridge []byte

const managementJSIdentifier = `[A-Za-z_$][A-Za-z0-9_$]*`

var (
	managementAuthStorePattern         = regexp.MustCompile(`(` + managementJSIdentifier + `)\s*=\s*` + managementJSIdentifier + `\s*\([^{};]{0,256}\{\s*isAuthenticated\s*:\s*(?:!1|false)\s*,\s*apiBase\s*:`)
	managementQuotaStorePattern        = regexp.MustCompile(`(` + managementJSIdentifier + `)\s*=\s*` + managementJSIdentifier + `\s*\([^{};]{0,256}\{\s*cacheScope\s*:\s*["'\x60]{2}\s*,\s*cacheGeneration\s*:\s*0\s*,`)
	managementQuotaConfigPattern       = regexp.MustCompile(`(` + managementJSIdentifier + `)\s*=\s*\{\s*type\s*:\s*["'\x60](codex|claude)["'\x60]\s*,\s*i18nPrefix\s*:\s*["'\x60](?:codex|claude)_quota["'\x60]`)
	managementCodexQuotaParserPattern  = regexp.MustCompile(`(` + managementJSIdentifier + `)\s*=\s*\([^{};]{0,256}\)\s*=>\s*` + managementJSIdentifier + `\s*\(\s*` + managementJSIdentifier + `\s*,\s*\{\s*planType\s*:\s*` + managementJSIdentifier + `\s*,\s*observedAtMs\s*:\s*` + managementJSIdentifier + `\s*,\s*source\s*:\s*` + managementJSIdentifier + `\s*,\s*rateLimitScope\s*:`)
	managementClaudeQuotaParserPattern = regexp.MustCompile(`(` + managementJSIdentifier + `)\s*=\s*\(\s*` + managementJSIdentifier + `\s*,\s*` + managementJSIdentifier + `\s*\)\s*=>\s*\{\s*let\s+` + managementJSIdentifier + `\s*=\s*\[\]\s*,[^;]{0,256};\s*for\s*\(\s*let\s*\{\s*key\s*:\s*` + managementJSIdentifier + `\s*,\s*id\s*:\s*` + managementJSIdentifier + `\s*,\s*labelKey\s*:`)
)

func managementQuotaBinding(script []byte) []byte {
	store := managementQuotaStorePattern.FindSubmatch(script)
	codexParser := managementCodexQuotaParserPattern.FindSubmatch(script)
	claudeParser := managementClaudeQuotaParserPattern.FindSubmatch(script)
	if store == nil || codexParser == nil || claudeParser == nil {
		return nil
	}
	configs := make(map[string]string, 2)
	for _, match := range managementQuotaConfigPattern.FindAllSubmatch(script, -1) {
		configs[string(match[2])] = string(match[1])
	}
	if configs["codex"] == "" || configs["claude"] == "" {
		return nil
	}
	return []byte(";window.__CPA_PLUGIN_BRIDGE__?.bindQuotaStore(" + string(store[1]) + ",{codex:{config:" + configs["codex"] +
		",parse:(payload,at)=>" + string(codexParser[1]) + "(payload,key=>key,payload.plan_type,at,'response_header')},claude:{config:" + configs["claude"] +
		",parse:payload=>" + string(claudeParser[1]) + "(payload,key=>key)}});\n")
}

// InjectPluginSessionBridge adds login sharing and quota updates without changing the downloaded asset.
func InjectPluginSessionBridge(html []byte) []byte {
	if match := managementAuthStorePattern.FindSubmatchIndex(html); match != nil {
		if closeScript := bytes.Index(html[match[1]:], []byte("</script>")); closeScript >= 0 {
			closeScript += match[1]
			binding := []byte("\n;window.__CPA_PLUGIN_BRIDGE__?.bindAuthStore(" + string(html[match[2]:match[3]]) + ");\n")
			scriptStart := 0
			if previousScript := bytes.LastIndex(html[:match[0]], []byte("</script>")); previousScript >= 0 {
				scriptStart = previousScript + len("</script>")
			}
			binding = append(binding, managementQuotaBinding(html[scriptStart:closeScript])...)
			page := make([]byte, 0, len(html)+len(binding))
			page = append(page, html[:closeScript]...)
			page = append(page, binding...)
			html = append(page, html[closeScript:]...)
		}
	}
	bridge := bytes.Replace(pluginSessionBridge, []byte("/*__CREDENTIAL_QUOTA_SYNC__*/"), credentialQuotaBridge, 1)
	start := bytes.Index(bytes.ToLower(html), []byte("<head"))
	if start < 0 {
		return html
	}
	end := bytes.IndexByte(html[start:], '>')
	if end < 0 {
		return html
	}
	end += start + 1
	page := make([]byte, 0, len(html)+len(bridge)+len("<script></script>"))
	page = append(page, html[:end]...)
	page = append(page, "<script>"...)
	page = append(page, bridge...)
	page = append(page, "</script>"...)
	return append(page, html[end:]...)
}
