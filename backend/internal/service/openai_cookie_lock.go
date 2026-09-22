package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Cookie locking is an opt-in HTTP mode independent of turn-state replacement.
// JWT claims are untrusted routing metadata, never authentication or a URL to dial.
const (
	openAICookieLockExtraKey     = "openai_cookie_lock"
	openAICookiePoolExtraKey     = "openai_cookie_pool"
	openAICookiePoolTarget       = 3
	openAICookiePoolLimit        = 24
	openAICookieObservationLimit = 256 * 1024
	ctxKeyOpenAICookieSent       = "openai_cookie_sent"
)

type openAICookieCandidate struct {
	Pod           string    `json:"pod"`
	Cookie        string    `json:"cookie"`
	Cflb          string    `json:"cflb,omitempty"`
	Iat           int64     `json:"iat"`
	Exp           int64     `json:"exp"`
	Model         string    `json:"model"`
	LastSeenModel string    `json:"last_seen_model"`
	ObservedAt    time.Time `json:"observed_at"`
	Failed        bool      `json:"failed,omitempty"`
	FailStreak    int       `json:"fail_streak,omitempty"`
}

func (a *Account) IsOpenAICookieLockEnabled() bool {
	return a != nil && a.IsOpenAIOAuthLike() && a.getExtraBool(openAICookieLockExtraKey)
}

func (a *Account) openAITicketInjectionEnabled() bool {
	return a.IsOpenAITurnStateAutoEnabled() || a.IsOpenAICookieLockEnabled()
}

func (p openAICookieCandidate) usable(model string, now time.Time) bool {
	return !p.Failed && p.Cookie != "" && now.Unix() < p.Exp &&
		openAITurnStateComparableModel(p.Model) == openAITurnStateComparableModel(model) &&
		openAITurnStateComparableModel(p.LastSeenModel) == openAITurnStateComparableModel(model)
}

func (p openAICookieCandidate) header() string {
	return (openAITurnStateRouteCookieState{cflb: p.Cflb, oailb: p.Cookie}).header()
}

func parseOpenAICookieJWT(value string, now time.Time) (openAICookieCandidate, bool) {
	var p openAICookieCandidate
	if len(value) > 8192 || strings.ContainsAny(value, ";\r\n ") {
		return p, false
	}
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[2] == "" {
		return p, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return p, false
	}
	var claims struct {
		Host string `json:"host"`
		Iat  int64  `json:"iat"`
		Exp  int64  `json:"exp"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return p, false
	}
	host := strings.ToLower(claims.Host)
	number := strings.TrimSuffix(strings.TrimPrefix(host, "chat.gateway.unified-"), ".api.openai.com")
	if number == "" || host != "chat.gateway.unified-"+number+".api.openai.com" {
		return p, false
	}
	for _, ch := range number {
		if ch < '0' || ch > '9' {
			return p, false
		}
	}
	if claims.Iat <= 0 || claims.Iat > now.Add(time.Minute).Unix() || claims.Exp <= claims.Iat || claims.Exp <= now.Unix() {
		return p, false
	}
	p.Pod, p.Cookie, p.Iat, p.Exp = host, value, claims.Iat, claims.Exp
	return p, true
}

func readOpenAICookiePool(a *Account) []openAICookieCandidate {
	if a == nil {
		return nil
	}
	data, err := json.Marshal(a.Extra[openAICookiePoolExtraKey])
	if err != nil {
		return nil
	}
	var pool []openAICookieCandidate
	if json.Unmarshal(data, &pool) != nil {
		return nil
	}
	// Validate stored values too: imports and old snapshots are not trusted.
	valid := make([]openAICookieCandidate, 0, len(pool))
	for _, p := range pool {
		parsed, ok := parseOpenAICookieJWT(p.Cookie, time.Now())
		if !ok || parsed.Pod != p.Pod || parsed.Exp != p.Exp || strings.ContainsAny(p.Cflb, ";\r\n ") {
			continue
		}
		valid = append(valid, p)
		if len(valid) == openAICookiePoolLimit {
			break
		}
	}
	return valid
}

func (s *OpenAIGatewayService) loadOpenAICookiePoolFresh(ctx context.Context, a *Account) []openAICookieCandidate {
	if s != nil && s.accountRepo != nil {
		latest, err := s.accountRepo.GetByID(ctx, a.ID)
		if err != nil || latest == nil {
			return nil
		} // never revive an invalidated scheduler snapshot
		return readOpenAICookiePool(latest)
	}
	return readOpenAICookiePool(a)
}

func pickOpenAICookie(pool []openAICookieCandidate, model string, now time.Time) (openAICookieCandidate, bool) {
	for _, p := range pool {
		if p.usable(model, now) {
			return p, true
		}
	}
	return openAICookieCandidate{}, false
}

func openAICookiePoolFresh(pool []openAICookieCandidate, model string, now time.Time, lead time.Duration) bool {
	pods := map[string]bool{}
	for _, p := range pool {
		if p.usable(model, now.Add(lead)) {
			pods[p.Pod] = true
		}
	}
	return len(pods) >= openAICookiePoolTarget
}

func (s *OpenAIGatewayService) applyOpenAICookieLock(c *gin.Context, a *Account, h http.Header) {
	if c == nil {
		return
	}
	c.Set(ctxKeyOpenAICookieSent, openAICookieCandidate{})
	if !a.IsOpenAICookieLockEnabled() || openAITurnStateAutoSkipped(c) || openAITurnStateProbeContext(c) {
		return
	}
	model := openAITurnStateRequestModel(c)
	pool := s.loadOpenAICookiePoolFresh(turnStateOpCtx(c), a)
	p, ok := pickOpenAICookie(pool, model, time.Now())
	if !ok {
		s.holdOpenAITurnStateIfUnfilled(c, a, model)
		return
	}
	h.Set("Cookie", mergeOpenAITurnStateRouteCookie(h.Get("Cookie"), p.header()))
	c.Set(ctxKeyOpenAICookieSent, p)
}

type openAICookieRequestKey struct{}
type openAICookieRequest struct {
	model   string
	sent    openAICookieCandidate
	started time.Time
}

// Capture per-attempt values, rather than sharing a mutable Gin context with body readers.
func attachOpenAICookieRequest(c *gin.Context, a *Account, req *http.Request) *http.Request {
	if c == nil || !a.IsOpenAICookieLockEnabled() || openAITurnStateAutoSkipped(c) || openAITurnStateProbeContext(c) {
		return req
	}
	value, _ := c.Get(ctxKeyOpenAICookieSent)
	sent, _ := value.(openAICookieCandidate)
	state := openAICookieRequest{model: openAITurnStateRequestModel(c), sent: sent, started: time.Now()}
	return req.WithContext(context.WithValue(req.Context(), openAICookieRequestKey{}, state))
}

func (s *OpenAIGatewayService) observeOpenAICookieResponse(ctx context.Context, a *Account, state openAICookieRequest, headers http.Header, actual string) {
	if !a.IsOpenAICookieLockEnabled() || state.model == "" || actual == "" {
		return
	}
	now := time.Now()
	route := parseOpenAITurnStateRouteCookies(headers)
	incoming, issued := parseOpenAICookieJWT(route.oailb, now)
	current := state.sent
	if issued {
		incoming.Cflb = route.cflb
		if incoming.Pod == current.Pod && incoming.Cflb == "" {
			incoming.Cflb = current.Cflb
		}
		current = incoming
	} else if route.oailb != "" {
		// A rejected/expired replacement must not silently revalidate the sent cookie.
		actual = "unknown"
	} else if route.cflb != "" {
		current.Cflb = route.cflb
	}
	deleted := false
	for _, raw := range headers.Values("Set-Cookie") {
		cookie, err := http.ParseSetCookie(raw)
		if err == nil && cookie.Name == openAITurnStateRouteCookieOailb && (cookie.MaxAge < 0 || cookie.Value == "" || (!cookie.Expires.IsZero() && !now.Before(cookie.Expires))) {
			deleted = true
		}
	}
	if current.Cookie == "" {
		return
	}
	current.Model, current.LastSeenModel, current.ObservedAt = state.model, actual, state.started
	current.Failed, current.FailStreak = false, 0
	mismatch := deleted || openAITurnStateComparableModel(actual) != openAITurnStateComparableModel(state.model)
	mu := openAITurnStatePoolLock(a.ID)
	mu.Lock()
	defer mu.Unlock()
	// Close/cancel of the downstream must not discard a model mismatch already observed.
	opCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	latest := a
	if s.accountRepo != nil {
		var err error
		latest, err = s.accountRepo.GetByID(opCtx, a.ID)
		if err != nil || latest == nil || !latest.IsOpenAICookieLockEnabled() {
			return
		}
	}
	pool := readOpenAICookiePool(latest)
	kept := make([]openAICookieCandidate, 0, len(pool)+1)
	for _, p := range pool {
		if openAITurnStateComparableModel(p.Model) == openAITurnStateComparableModel(state.model) {
			if p.Pod == current.Pod {
				if p.ObservedAt.After(state.started) {
					return
				} // late responses cannot undo newer failures
				if mismatch {
					current.FailStreak = p.FailStreak
				}
				// A request can carry an older JWT while another response renews
				// this pod. Observing it must not shorten the persisted expiry.
				if p.Exp > current.Exp {
					current.Cookie, current.Iat, current.Exp = p.Cookie, p.Iat, p.Exp
					current.Cflb = p.Cflb
				}
				continue
			}
			if issued && state.sent.Pod != "" && state.sent.Pod != current.Pod && p.Pod == state.sent.Pod && !p.ObservedAt.After(state.started) {
				p.Failed, p.ObservedAt = true, state.started
			}
		}
		kept = append(kept, p)
	}
	if mismatch {
		current.FailStreak++
		current.Failed = deleted || current.FailStreak >= a.openAITurnStateFailThreshold()
	}
	pool = append([]openAICookieCandidate{current}, kept...)
	if len(pool) > openAICookiePoolLimit {
		pool = pool[:openAICookiePoolLimit]
	}
	encoded, err := json.Marshal(pool)
	if err != nil {
		return
	}
	var generic []any
	if json.Unmarshal(encoded, &generic) != nil {
		return
	}
	if s.accountRepo != nil {
		if err := s.accountRepo.UpdateExtra(opCtx, a.ID, map[string]any{openAICookiePoolExtraKey: generic}); err != nil {
			logOpenAITurnStateAuto("persist cookie pool failed: account=%d err=%v", a.ID, err)
		}
	} else {
		if a.Extra == nil {
			a.Extra = map[string]any{}
		}
		a.Extra[openAICookiePoolExtraKey] = generic
	}
}

// Observe bytes as the existing forwarding pipeline reads them. No eager read, stream
// rewriting or extra upstream request; the buffer is bounded even for malformed SSE.
type openAICookieBody struct {
	io.ReadCloser
	pending []byte
	sse     bool
	done    bool
	observe func(string)
}

func (b *openAICookieBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if b.done {
		return n, err
	}
	if len(b.pending)+n > openAICookieObservationLimit {
		b.done = true
		b.pending = nil
		return n, err
	}
	b.pending = append(b.pending, p[:n]...)
	if !b.sse {
		if err != nil {
			b.check(string(b.pending))
			b.done = true
			b.pending = nil
		}
		return n, err
	}
	for {
		i := bytes.IndexByte(b.pending, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(b.pending[:i]))
		b.pending = b.pending[i+1:]
		if strings.HasPrefix(line, "data:") && b.check(strings.TrimSpace(strings.TrimPrefix(line, "data:"))) {
			return n, err
		}
	}
	if err != nil {
		b.check(strings.TrimSpace(string(b.pending)))
		b.done = true
		b.pending = nil
	}
	return n, err
}

func (b *openAICookieBody) check(data string) bool {
	kind := gjson.Get(data, "type").String()
	if kind != "response.created" && kind != "response.completed" && kind != "" {
		return false
	}
	model := gjson.Get(data, "response.model").String()
	if model == "" && kind == "" {
		model = gjson.Get(data, "model").String()
	}
	if model == "" {
		return false
	}
	b.done, b.pending = true, nil
	b.observe(model)
	return true
}

func (s *OpenAIGatewayService) wrapOpenAICookieResponse(req *http.Request, a *Account, resp *http.Response) {
	state, ok := req.Context().Value(openAICookieRequestKey{}).(openAICookieRequest)
	if !ok || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		return
	}
	resp.Body = &openAICookieBody{ReadCloser: resp.Body, sse: strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream"), observe: func(model string) {
		s.observeOpenAICookieResponse(req.Context(), a, state, resp.Header, model)
	}}
}

// Cookie probes read only through response.created, with both a byte limit and the
// existing probe deadline. Absence of a model is never a successful sample.
func readOpenAICookieProbeModel(body io.Reader) string {
	scanner := bufio.NewScanner(io.LimitReader(body, openAICookieObservationLimit))
	scanner.Buffer(make([]byte, 4096), openAICookieObservationLimit)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if gjson.Get(data, "type").String() == "response.created" {
			return gjson.Get(data, "response.model").String()
		}
	}
	return ""
}

func nextOpenAICookieRenewal(pool []openAICookieCandidate, model string, now time.Time, lead time.Duration, tried map[string]bool) *openAICookieCandidate {
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].Exp < pool[j].Exp })
	for _, p := range pool {
		if p.usable(model, now) && time.Unix(p.Exp, 0).Sub(now) <= lead && !tried[model+"/"+p.Pod] {
			return &p
		}
	}
	return nil
}

// Allow two ticks to renew before expiry, including when the configured lead is one minute.
func openAICookieRenewLead(cfg openAITurnStateHunterConfig) time.Duration {
	lead := cfg.lead()
	if minimum := 2 * openAITurnStateHunterInterval; lead < minimum {
		return minimum
	}
	return lead
}

// Discovery backoff must leave room for renewal of cookies already in the pool.
func (s *OpenAITurnStateHunterService) cookieRetryAt(ctx context.Context, account *Account, cfg openAITurnStateHunterConfig, now time.Time) time.Time {
	next := now.Add(cfg.retry())
	if !cfg.cookieMode {
		return next
	}
	for _, p := range s.gateway.loadOpenAICookiePoolFresh(ctx, account) {
		if !p.usable(p.Model, now) {
			continue
		}
		renewal := time.Unix(p.Exp, 0).Add(-openAICookieRenewLead(cfg))
		if renewal.Before(now.Add(openAITurnStateHunterInterval)) {
			renewal = now.Add(openAITurnStateHunterInterval)
		}
		if renewal.Before(next) {
			next = renewal
		}
	}
	return next
}
