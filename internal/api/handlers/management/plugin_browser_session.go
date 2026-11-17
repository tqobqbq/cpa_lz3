package management

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

const pluginSessionLifetime = 12 * time.Hour

type pluginBrowserSession struct {
	once sync.Once
	aead cipher.AEAD
}

type pluginSessionPayload struct {
	Key     string `json:"key"`
	Expires int64  `json:"expires"`
}

// V8Middleware also lets plugin pages reuse a browser's authenticated session.
// Other management APIs still require an explicit management key.
func (h *Handler) V8Middleware() gin.HandlerFunc {
	return h.middleware(true)
}

// CreatePluginBrowserSession is authenticated by the v8 middleware, which sets the cookie.
func (h *Handler) CreatePluginBrowserSession(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"authenticated": true})
}

// DeletePluginBrowserSession only clears the requesting browser's cookie.
func (h *Handler) DeletePluginBrowserSession(c *gin.Context) {
	if !pluginSessionSameOrigin(c.Request) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "same-origin request required"})
		return
	}
	c.Header("Cache-Control", "no-store")
	h.clearPluginSession(c)
	c.Status(http.StatusNoContent)
}

func pluginSessionPath(path string) bool {
	if strings.ContainsAny(path, "\\%") {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for _, part := range parts {
		if part == "." || part == ".." {
			return false
		}
	}
	if len(parts) < 5 || parts[0] != "v8" || parts[1] != "management" {
		return false
	}
	if parts[2] == "plugins" {
		return parts[3] != "" && parts[4] == "extension"
	}
	return len(parts) >= 6 && parts[2] == "config" && parts[3] == "plugins" && parts[4] == "configs" && parts[5] != ""
}

func pluginSessionSameOrigin(req *http.Request) bool {
	if site := req.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := req.Header.Get("Origin"); origin != "" {
		parsed, errParse := url.Parse(origin)
		return errParse == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && strings.EqualFold(parsed.Host, req.Host)
	}
	return true
}

func pluginSessionCookieName(req *http.Request) string {
	sum := sha256.Sum256([]byte(strings.ToLower(req.Host)))
	return "cpa_plugin_session_" + hex.EncodeToString(sum[:6])
}

func (h *Handler) pluginSessionCipher() cipher.AEAD {
	h.pluginBrowserSession.once.Do(func() {
		key := make([]byte, 32)
		if _, errRandom := rand.Read(key); errRandom != nil {
			log.WithError(errRandom).Error("management: generate plugin browser session key")
			return
		}
		block, errCipher := aes.NewCipher(key)
		if errCipher != nil {
			log.WithError(errCipher).Error("management: create plugin browser session cipher")
			return
		}
		aead, errAEAD := cipher.NewGCM(block)
		if errAEAD != nil {
			log.WithError(errAEAD).Error("management: create plugin browser session encryption")
			return
		}
		h.pluginBrowserSession.aead = aead
	})
	return h.pluginBrowserSession.aead
}

func (h *Handler) readPluginSession(req *http.Request) (pluginSessionPayload, bool) {
	var payload pluginSessionPayload
	if !pluginSessionSameOrigin(req) {
		return payload, false
	}
	cookie, errCookie := req.Cookie(pluginSessionCookieName(req))
	if errCookie != nil || len(cookie.Value) > 4096 {
		return payload, false
	}
	aead := h.pluginSessionCipher()
	if aead == nil {
		return payload, false
	}
	raw, errDecode := base64.RawURLEncoding.DecodeString(cookie.Value)
	if errDecode != nil || len(raw) < aead.NonceSize()+aead.Overhead() {
		return payload, false
	}
	plain, errOpen := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(cookie.Name))
	if errOpen != nil || json.Unmarshal(plain, &payload) != nil || payload.Key == "" || time.Now().Unix() >= payload.Expires {
		return pluginSessionPayload{}, false
	}
	return payload, true
}

func (h *Handler) pluginSessionKey(req *http.Request) string {
	payload, valid := h.readPluginSession(req)
	if !valid {
		return ""
	}
	return payload.Key
}

func (h *Handler) issuePluginSession(c *gin.Context, key string) {
	if key == "" || len(key) > 2048 || !pluginSessionSameOrigin(c.Request) {
		return
	}
	if payload, valid := h.readPluginSession(c.Request); valid && payload.Key == key && time.Until(time.Unix(payload.Expires, 0)) > time.Hour {
		return
	}
	aead := h.pluginSessionCipher()
	if aead == nil {
		return
	}
	expires := time.Now().Add(pluginSessionLifetime)
	plain, errMarshal := json.Marshal(pluginSessionPayload{Key: key, Expires: expires.Unix()})
	if errMarshal != nil {
		return
	}
	nonce := make([]byte, aead.NonceSize())
	if _, errRandom := rand.Read(nonce); errRandom != nil {
		log.WithError(errRandom).Error("management: generate plugin browser session nonce")
		return
	}
	name := pluginSessionCookieName(c.Request)
	value := base64.RawURLEncoding.EncodeToString(aead.Seal(nonce, nonce, plain, []byte(name)))
	c.Header("Cache-Control", "no-store")
	http.SetCookie(c.Writer, &http.Cookie{
		Name: name, Value: value, Path: "/v8/management", Expires: expires,
		MaxAge: int(pluginSessionLifetime.Seconds()), HttpOnly: true,
		Secure:   c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https"),
		SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) clearPluginSession(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name: pluginSessionCookieName(c.Request), Path: "/v8/management", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: c.Request.TLS != nil || strings.EqualFold(c.GetHeader("X-Forwarded-Proto"), "https"),
	})
}
