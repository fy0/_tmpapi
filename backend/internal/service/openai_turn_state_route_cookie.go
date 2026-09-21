package service

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// 2026-09-22 凌晨上游拉闸之后，292 不再能单靠票续一小时。
// 实测：票管资格，__cflb / __oailb 管路由，两者不绑在同一张票上，也不绑对话；
// 任何一个还活着的账号级 Cookie 都能配任何一张 292。不带 Cookie 的 292 续不上，
// 带上之后整段大约 240 秒。Cookie 自己也会先死，所以单独计时，不写进候选池。
const (
	openAITurnStateRouteCookieExtraKey = "openai_turn_state_route_cookies"
	openAITurnStateRouteCookieTTL      = 240 * time.Second
	// 值没变也定期落库，让别的实例在 240 秒窗口内读得到；不必每条响应都写。
	openAITurnStateRouteCookiePersistGap = time.Minute

	openAITurnStateRouteCookieCflb  = "__cflb"
	openAITurnStateRouteCookieOailb = "__oailb"
)

// openAITurnStateRouteCookies 是进程内的账号级路由 Cookie。键是账号 ID。
// 猎手和真实流量可能打在不同实例上，所以变更还会写进 extra（调度中性键）。
var openAITurnStateRouteCookies sync.Map // int64 -> openAITurnStateRouteCookieState

type openAITurnStateRouteCookieState struct {
	cflb       string
	oailb      string
	capturedAt time.Time
}

type openAITurnStateRouteCookieRecord struct {
	Cflb       string    `json:"cflb,omitempty"`
	Oailb      string    `json:"oailb,omitempty"`
	CapturedAt time.Time `json:"captured_at"`
}

func (s openAITurnStateRouteCookieState) header() string {
	parts := make([]string, 0, 2)
	if s.cflb != "" {
		parts = append(parts, openAITurnStateRouteCookieCflb+"="+s.cflb)
	}
	if s.oailb != "" {
		parts = append(parts, openAITurnStateRouteCookieOailb+"="+s.oailb)
	}
	return strings.Join(parts, "; ")
}

func (s openAITurnStateRouteCookieState) alive(now time.Time) bool {
	return s.header() != "" && !s.capturedAt.IsZero() && now.Before(s.capturedAt.Add(openAITurnStateRouteCookieTTL))
}

func (s openAITurnStateRouteCookieState) sameValues(other openAITurnStateRouteCookieState) bool {
	return s.cflb == other.cflb && s.oailb == other.oailb
}

func parseOpenAITurnStateRouteCookies(upstream http.Header) openAITurnStateRouteCookieState {
	var state openAITurnStateRouteCookieState
	if upstream == nil {
		return state
	}
	for _, raw := range upstream.Values("Set-Cookie") {
		cookie, err := http.ParseSetCookie(raw)
		if err != nil || cookie == nil {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(cookie.Name))
		value := strings.TrimSpace(cookie.Value)
		if value == "" || strings.ContainsAny(value, ";\r\n") {
			continue
		}
		switch name {
		case openAITurnStateRouteCookieCflb:
			state.cflb = value
		case openAITurnStateRouteCookieOailb:
			state.oailb = value
		}
	}
	return state
}

func loadOpenAITurnStateRouteCookies(accountID int64) (openAITurnStateRouteCookieState, bool) {
	raw, ok := openAITurnStateRouteCookies.Load(accountID)
	if !ok {
		return openAITurnStateRouteCookieState{}, false
	}
	state, ok := raw.(openAITurnStateRouteCookieState)
	return state, ok
}

// rememberOpenAITurnStateRouteCookies 把响应里的路由 Cookie 收进进程内池。
// 只更新这次响应里出现的名字，另一只还活着的留着——上游不一定每次两只一起下发。
// 第二个返回值表示要不要落库：值变了，或者距上次落库已经过了持久化间隔。
func rememberOpenAITurnStateRouteCookies(accountID int64, upstream http.Header) (openAITurnStateRouteCookieState, bool) {
	if accountID <= 0 {
		return openAITurnStateRouteCookieState{}, false
	}
	incoming := parseOpenAITurnStateRouteCookies(upstream)
	if incoming.cflb == "" && incoming.oailb == "" {
		return openAITurnStateRouteCookieState{}, false
	}
	now := time.Now()
	var current openAITurnStateRouteCookieState
	if state, ok := loadOpenAITurnStateRouteCookies(accountID); ok && state.alive(now) {
		current = state
	}
	next := current
	if incoming.cflb != "" {
		next.cflb = incoming.cflb
	}
	if incoming.oailb != "" {
		next.oailb = incoming.oailb
	}
	next.capturedAt = now
	openAITurnStateRouteCookies.Store(accountID, next)
	persist := !next.sameValues(current) || current.capturedAt.IsZero() || now.Sub(current.capturedAt) >= openAITurnStateRouteCookiePersistGap
	return next, persist
}

func absorbOpenAITurnStateRouteCookies(account *Account) {
	if account == nil || account.ID <= 0 {
		return
	}
	state, ok := readOpenAITurnStateRouteCookies(account)
	if !ok || !state.alive(time.Now()) {
		return
	}
	if current, ok := loadOpenAITurnStateRouteCookies(account.ID); ok && current.alive(time.Now()) && !current.capturedAt.Before(state.capturedAt) {
		return
	}
	openAITurnStateRouteCookies.Store(account.ID, state)
}

func readOpenAITurnStateRouteCookies(account *Account) (openAITurnStateRouteCookieState, bool) {
	if account == nil || account.Extra == nil {
		return openAITurnStateRouteCookieState{}, false
	}
	raw, ok := account.Extra[openAITurnStateRouteCookieExtraKey]
	if !ok || raw == nil {
		return openAITurnStateRouteCookieState{}, false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return openAITurnStateRouteCookieState{}, false
	}
	var record openAITurnStateRouteCookieRecord
	if err := json.Unmarshal(encoded, &record); err != nil {
		return openAITurnStateRouteCookieState{}, false
	}
	state := openAITurnStateRouteCookieState{cflb: record.Cflb, oailb: record.Oailb, capturedAt: record.CapturedAt}
	if !state.alive(time.Now()) {
		return openAITurnStateRouteCookieState{}, false
	}
	return state, true
}

// noteOpenAITurnStateRouteCookies 在看到上游响应头时收 Cookie。
// 调用点要盖住猎手探测和真实流量（含首输出暂存、还没 relay 的那条）。
func (s *OpenAIGatewayService) noteOpenAITurnStateRouteCookies(c *gin.Context, account *Account, upstream http.Header) {
	if account == nil || !account.UsesOpenAICodexProtocol() {
		return
	}
	state, persist := rememberOpenAITurnStateRouteCookies(account.ID, upstream)
	if !persist {
		return
	}
	s.persistOpenAITurnStateRouteCookies(c, account, state)
}

func (s *OpenAIGatewayService) persistOpenAITurnStateRouteCookies(c *gin.Context, account *Account, state openAITurnStateRouteCookieState) {
	if account == nil || !state.alive(time.Now()) {
		return
	}
	record := openAITurnStateRouteCookieRecord{Cflb: state.cflb, Oailb: state.oailb, CapturedAt: state.capturedAt.UTC()}
	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}
	var generic map[string]any
	if err := json.Unmarshal(encoded, &generic); err != nil {
		return
	}
	if account.Extra == nil {
		account.Extra = map[string]any{}
	}
	account.Extra[openAITurnStateRouteCookieExtraKey] = generic
	if s == nil || s.accountRepo == nil {
		return
	}
	if err := s.accountRepo.UpdateExtra(turnStateOpCtx(c), account.ID, map[string]any{
		openAITurnStateRouteCookieExtraKey: generic,
	}); err != nil {
		logOpenAITurnStateAuto("persist route cookie failed: account=%d err=%v", account.ID, err)
	}
}

// liveOpenAITurnStateRouteCookie 取这个账号还能用的路由 Cookie。
// 先看进程内，再看账号快照，最后才读库——292 注入路径通常已经把新鲜 extra 吸进内存了。
func (s *OpenAIGatewayService) liveOpenAITurnStateRouteCookie(c *gin.Context, account *Account) string {
	if account == nil || account.ID <= 0 {
		return ""
	}
	now := time.Now()
	if state, ok := loadOpenAITurnStateRouteCookies(account.ID); ok && state.alive(now) {
		return state.header()
	}
	if state, ok := readOpenAITurnStateRouteCookies(account); ok {
		openAITurnStateRouteCookies.Store(account.ID, state)
		return state.header()
	}
	if s == nil || s.accountRepo == nil {
		return ""
	}
	latest, err := s.accountRepo.GetByID(turnStateOpCtx(c), account.ID)
	if err != nil || latest == nil {
		return ""
	}
	state, ok := readOpenAITurnStateRouteCookies(latest)
	if !ok {
		return ""
	}
	if current, ok := loadOpenAITurnStateRouteCookies(account.ID); ok && current.alive(now) && !current.capturedAt.Before(state.capturedAt) {
		return current.header()
	}
	openAITurnStateRouteCookies.Store(account.ID, state)
	return state.header()
}

// applyOpenAITurnStateRouteCookie 给出站的健康票补上账号级路由 Cookie。
// 票和 Cookie 不配对：池里任意一张活着的都可。探测保持裸发，免得把摇骰子钉死在旧路由上。
// 312 不带——那条路由本来就不是要续的 292。
func (s *OpenAIGatewayService) applyOpenAITurnStateRouteCookie(c *gin.Context, account *Account, h http.Header) {
	if s == nil || h == nil || account == nil || !account.UsesOpenAICodexProtocol() || openAITurnStateProbeContext(c) {
		return
	}
	if !openAITurnStateHealthy(h.Get(openAICodexTurnStateHeader)) {
		return
	}
	route := s.liveOpenAITurnStateRouteCookie(c, account)
	if route == "" {
		return
	}
	if merged := mergeOpenAITurnStateRouteCookie(h.Get("Cookie"), route); merged != "" {
		h.Set("Cookie", merged)
	}
}

func mergeOpenAITurnStateRouteCookie(existing, route string) string {
	kept := make([]string, 0, 4)
	for _, part := range strings.Split(existing, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, _ := strings.Cut(part, "=")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case openAITurnStateRouteCookieCflb, openAITurnStateRouteCookieOailb:
			continue
		}
		kept = append(kept, part)
	}
	for _, part := range strings.Split(route, ";") {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "; ")
}
