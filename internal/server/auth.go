package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Zslocal/go_docker_log/internal/web"
)

const (
	// sessionCookie 存的是「口令的派生值」而不是口令本身，
	// 这样即便浏览器里翻到 Cookie 也拿不到原始密钥。
	sessionCookie = "dlog_session"
	sessionMaxAge = 7 * 24 * 3600
)

// sessionValue 由访问口令派生，口令不变则派生值稳定，重启不掉线。
// 反过来说，改口令等于让所有已登录会话立即失效，行为符合预期。
func (s *Server) sessionValue() string {
	sum := sha256.Sum256([]byte("dlog-session|" + s.cfg.Token))
	return hex.EncodeToString(sum[:])
}

// tokenEqual 用常数时间比较，避免按字节提前返回泄露口令长度/前缀。
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *Server) hasValidSession(r *http.Request) bool {
	ck, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	return tokenEqual(ck.Value, s.sessionValue())
}

// authorized 依次检查 Cookie、query token、Bearer 头。
//
// 之所以三种都支持：
//   - Cookie：浏览器主路径，EventSource 无法自定义请求头，只能靠 Cookie；
//   - query：curl / 脚本 / 临时分享一条链接；
//   - Bearer：给自动化工具用，不会把口令留在 URL 和访问日志里。
func (s *Server) authorized(r *http.Request) bool {
	if s.hasValidSession(r) {
		return true
	}
	if t := r.URL.Query().Get("token"); t != "" && tokenEqual(t, s.cfg.Token) {
		return true
	}
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if tokenEqual(strings.TrimPrefix(h, "Bearer "), s.cfg.Token) {
			return true
		}
	}
	return false
}

// setSessionCookie 种登录态。
func (s *Server) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    s.sessionValue(),
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   sessionMaxAge,
		// 穿透 nginx 时 r.TLS 为空，得看反代给的 X-Forwarded-Proto，
		// 否则 https 站点上这个 Cookie 永远不会被标记为 Secure。
		Secure: r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"),
	})
}

// auth 是访问控制中间件；未配置口令时直接放行。
func (s *Server) auth(next http.Handler) http.Handler {
	if s.cfg.Token == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 登录接口本身必须匿名可访问，否则登录页永远提交不上。
		if r.URL.Path == "/api/login" {
			next.ServeHTTP(w, r)
			return
		}

		// 用 ?token= 打开链接时顺手种下 Cookie，之后地址栏里就不用带着密钥了。
		if t := r.URL.Query().Get("token"); t != "" && tokenEqual(t, s.cfg.Token) {
			s.setSessionCookie(w, r)
			next.ServeHTTP(w, r)
			return
		}

		if s.authorized(r) {
			next.ServeHTTP(w, r)
			return
		}

		if wantsHTML(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write(web.LoginHTML())
			return
		}
		writeError(w, http.StatusUnauthorized, errors.New("未授权：请先在浏览器打开首页输入访问口令"))
	})
}

// wantsHTML 判断这次请求是不是"用户点开一个页面"，决定未授权时返回登录页还是 JSON。
func wantsHTML(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		return true
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// handleLogin 校验口令并下发会话 Cookie。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, errors.New("只支持 POST"))
		return
	}
	if s.cfg.Token == "" {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
		return
	}

	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}

	if !tokenEqual(strings.TrimSpace(body.Token), s.cfg.Token) {
		// 固定延迟，挡掉最廉价的暴力尝试。
		time.Sleep(300 * time.Millisecond)
		writeError(w, http.StatusUnauthorized, errors.New("口令不正确"))
		return
	}

	s.setSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleLogout 清掉会话。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		MaxAge:   -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
