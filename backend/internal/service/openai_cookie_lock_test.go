//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func cookieTestJWT(t *testing.T, pod string, minted, expires time.Time) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"host": pod, "iat": minted.Unix(), "exp": expires.Unix()})
	require.NoError(t, err)
	return "eyJhbGciOiJFUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".signed-upstream"
}
func cookieTestAccount() *Account {
	a := hunterTestAccount(hunterConfig(nil))
	a.Extra[openAITurnStateAutoExtraKey] = false
	a.Extra[openAICookieLockExtraKey] = true
	return a
}

const cookiePodA = "chat.gateway.unified-185.api.openai.com"
const cookiePodB = "chat.gateway.unified-87.api.openai.com"

func cookieTestHeaders(jwt string) http.Header {
	h := http.Header{"Content-Type": []string{"text/event-stream"}}
	if jwt != "" {
		h.Add("Set-Cookie", "__oailb="+jwt+"; Path=/; Secure; HttpOnly")
	}
	return h
}
func cookieTestObserve(t *testing.T, gw *OpenAIGatewayService, a *Account, pod, model string, started, expiry time.Time) openAICookieCandidate {
	t.Helper()
	jwt := cookieTestJWT(t, pod, time.Now().Add(-time.Minute), expiry)
	gw.observeOpenAICookieResponse(context.Background(), a, openAICookieRequest{model: hunterTestModel, started: started}, cookieTestHeaders(jwt), model)
	for _, p := range readOpenAICookiePool(a) {
		if p.Pod == pod {
			return p
		}
	}
	t.Fatal("missing candidate")
	return openAICookieCandidate{}
}
func TestOpenAICookieLockIndependentAndAccountScoped(t *testing.T) {
	gw, a := &OpenAIGatewayService{}, cookieTestAccount()
	now := time.Now()
	p := cookieTestObserve(t, gw, a, cookiePodA, hunterTestModel, now, now.Add(time.Hour))
	c := turnStateAutoCtxModel("cookie-only", hunterTestModel)
	h := http.Header{"Cookie": []string{"other=keep"}, "X-Codex-Turn-State": []string{"client-state"}}
	gw.applyOpenAICodexTurnStateOverrideHeader(c, a, h)
	require.Contains(t, h.Get("Cookie"), "__oailb="+p.Cookie)
	require.Contains(t, h.Get("Cookie"), "other=keep")
	require.Equal(t, "client-state", h.Get(openAICodexTurnStateHeader))
	require.Empty(t, OpenAITurnStateUsageSource(c))
	require.Empty(t, readOpenAITurnStatePool(a))
	other := cookieTestAccount()
	other.ID++
	next := http.Header{}
	gw.applyOpenAICodexTurnStateOverrideHeader(c, other, next)
	require.Empty(t, next.Get("Cookie"))
	value, _ := c.Get(ctxKeyOpenAICookieSent)
	require.Empty(t, value.(openAICookieCandidate).Cookie)
}
func TestOpenAICookieInvalidationAndLateResponse(t *testing.T) {
	gw, a := &OpenAIGatewayService{}, cookieTestAccount()
	now := time.Now()
	sent := cookieTestObserve(t, gw, a, cookiePodA, hunterTestModel, now, now.Add(time.Hour))
	state := openAICookieRequest{model: hunterTestModel, sent: sent, started: now.Add(time.Second)}
	gw.observeOpenAICookieResponse(context.Background(), a, state, http.Header{}, "gpt-5.6-luna")
	pool := readOpenAICookiePool(a)
	require.True(t, pool[0].Failed)
	_, ok := pickOpenAICookie(pool, hunterTestModel, now)
	require.False(t, ok)
	state.started = now
	gw.observeOpenAICookieResponse(context.Background(), a, state, http.Header{}, hunterTestModel)
	require.True(t, readOpenAICookiePool(a)[0].Failed)
	state.started = now.Add(2 * time.Second)
	gw.observeOpenAICookieResponse(context.Background(), a, state, http.Header{}, hunterTestModel)
	require.False(t, readOpenAICookiePool(a)[0].Failed)
	state.started = now.Add(3 * time.Second)
	replacement := cookieTestJWT(t, cookiePodB, now, now.Add(time.Hour))
	gw.observeOpenAICookieResponse(context.Background(), a, state, cookieTestHeaders(replacement), hunterTestModel)
	pool = readOpenAICookiePool(a)
	require.Len(t, pool, 2)
	require.Equal(t, cookiePodB, pool[0].Pod)
	require.True(t, pool[1].Failed)
}
func TestOpenAICookieDeletionExpiryAndMalformedJWT(t *testing.T) {
	now := time.Now()
	for _, jwt := range []string{"garbage", "a.e30.sig", cookieTestJWT(t, cookiePodA, now.Add(-2*time.Hour), now.Add(-time.Second)), cookieTestJWT(t, "attacker.example", now, now.Add(time.Hour))} {
		_, ok := parseOpenAICookieJWT(jwt, now)
		require.False(t, ok)
	}
	gw, a := &OpenAIGatewayService{}, cookieTestAccount()
	sent := cookieTestObserve(t, gw, a, cookiePodA, hunterTestModel, now, now.Add(time.Hour))
	require.False(t, sent.usable(hunterTestModel, time.Unix(sent.Exp, 0)))
	require.False(t, sent.usable("gpt-5.6-sol", now))
	headers := http.Header{"Set-Cookie": []string{"__oailb=; Max-Age=0"}}
	gw.observeOpenAICookieResponse(context.Background(), a, openAICookieRequest{model: hunterTestModel, sent: sent, started: now.Add(time.Second)}, headers, hunterTestModel)
	require.True(t, readOpenAICookiePool(a)[0].Failed)
}

func cookieTestSSE(model string) string {
	return "data: {\"type\":\"response.created\",\"response\":{\"model\":\"" + model + "\"}}\n\n"
}
func TestOpenAICookieResponseObserverPreservesWire(t *testing.T) {
	for _, wire := range []string{cookieTestSSE(hunterTestModel) + "data: [DONE]\n", "{\n  \"model\": \"gpt-6-astra\"\n}"} {
		var models []string
		body := &openAICookieBody{ReadCloser: io.NopCloser(strings.NewReader(wire)), sse: strings.HasPrefix(wire, "data:"), observe: func(m string) { models = append(models, m) }}
		var output strings.Builder
		buf := make([]byte, 7)
		for {
			n, err := body.Read(buf)
			output.Write(buf[:n])
			if err == io.EOF {
				break
			}
			require.NoError(t, err)
		}
		require.Equal(t, wire, output.String())
		require.Equal(t, []string{hunterTestModel}, models)
	}
}
func TestOpenAICookieHunterUsesActualModelWithoutState(t *testing.T) {
	a := cookieTestAccount()
	h := newHunterHarness(a, hunterWebshareProxy)
	now := time.Now()
	for _, sample := range []struct{ pod, model string }{
		{cookiePodA, "gpt-5.6-luna"}, {cookiePodA, hunterTestModel}, {cookiePodB, hunterTestModel}, {"chat.gateway.unified-42.api.openai.com", hunterTestModel},
	} {
		jwt := cookieTestJWT(t, sample.pod, now, now.Add(time.Hour))
		h.up.queue = append(h.up.queue, &http.Response{StatusCode: 200, Header: cookieTestHeaders(jwt), Body: io.NopCloser(strings.NewReader(cookieTestSSE(sample.model)))})
	}
	h.run(t)
	require.Len(t, h.up.requests, 4)
	for _, req := range h.up.requests {
		require.Empty(t, req.Header.Get("Cookie"))
		require.Empty(t, req.Header.Get(openAICodexTurnStateHeader))
	}
	require.Len(t, readOpenAICookiePool(a), 3)
	require.Empty(t, readOpenAITurnStatePool(a))
	require.True(t, h.state().Last[0].Healthy)
	require.Equal(t, hunterTestModel, h.state().Last[0].ServedModel)
	require.False(t, h.state().Last[3].Healthy)
}
func TestOpenAICookieRenewalAndFreshSnapshot(t *testing.T) {
	a := cookieTestAccount()
	h := newHunterHarness(a, hunterCoxProxy)
	now := time.Now()
	old := cookieTestObserve(t, h.gw, a, cookiePodA, hunterTestModel, now.Add(-time.Minute), now.Add(time.Minute))
	cfg, _ := readOpenAITurnStateHunterConfig(a)
	cfg.renewCookie = &old
	renewed := cookieTestJWT(t, cookiePodA, now, now.Add(time.Hour))
	h.up.queue = []*http.Response{{StatusCode: 200, Header: cookieTestHeaders(renewed), Body: io.NopCloser(strings.NewReader(cookieTestSSE(hunterTestModel)))}}
	attempt := h.svc.probe(context.Background(), a, hunterTestModel, cfg, hunterCoxProxy)
	require.True(t, attempt.Healthy)
	require.True(t, attempt.Renewal)
	require.Contains(t, h.up.requests[0].Header.Get("Cookie"), old.Cookie)
	require.Equal(t, renewed, readOpenAICookiePool(a)[0].Cookie)
	require.Equal(t, now.Add(time.Hour).Unix(), readOpenAICookiePool(a)[0].Exp)
	stale := hunterCloneAccount(a)
	a.Extra[openAICookiePoolExtraKey] = nil
	require.Empty(t, h.gw.loadOpenAICookiePoolFresh(context.Background(), &stale))
	st := openAITurnStateHuntState{Exits: []openAITurnStateHuntExit{{IP: "1.2.3.4", ProxyID: hunterCoxProxy.ID, At: now, Healthy: false}}}
	_, cooling := h.svc.resolveHuntExit(context.Background(), cfg, &st, hunterCoxProxy)
	require.False(t, cooling)
}
func TestOpenAICookieMissingModelNeverHits(t *testing.T) {
	a := cookieTestAccount()
	h := newHunterHarness(a, hunterCoxProxy)
	jwt := cookieTestJWT(t, cookiePodA, time.Now(), time.Now().Add(time.Hour))
	resp, _ := hunterResp(200, strings.Repeat("a", 292), "data: {\"type\":\"response.created\",\"response\":{}}\n\n")
	resp.Header = cookieTestHeaders(jwt)
	h.up.queue = []*http.Response{resp}
	cfg, _ := readOpenAITurnStateHunterConfig(a)
	attempt := h.svc.probe(context.Background(), a, hunterTestModel, cfg, hunterCoxProxy)
	require.False(t, attempt.Healthy)
	require.NotEmpty(t, attempt.Error)
	require.Empty(t, readOpenAICookiePool(a))
}

func TestOpenAICookieHoldAndReleaseWithoutState(t *testing.T) {
	a := cookieTestAccount()
	a.Extra[openAITurnStateHunterExtraKey] = hunterConfig(map[string]any{"hold_when_degraded": true})
	h := newHunterHarness(a, hunterWebshareProxy)
	c := turnStateAutoCtxModel("held-cookie", hunterTestModel)
	h.gw.applyOpenAICodexTurnStateOverrideHeader(c, a, http.Header{})
	require.Error(t, openAITurnStateHoldError(c))
	require.True(t, openAITurnStateModelHeld(a, hunterTestModel, time.Now()))
	h.svc.syncHold(context.Background(), a, time.Now())
	require.True(t, openAITurnStateModelHeld(a, hunterTestModel, time.Now()), "state takeover being off must not release cookie holds")
	cookieTestObserve(t, h.gw, a, cookiePodA, hunterTestModel, time.Now(), time.Now().Add(time.Hour))
	h.svc.syncHold(context.Background(), a, time.Now())
	require.False(t, openAITurnStateModelHeld(a, hunterTestModel, time.Now()))
}
func TestOpenAICookieDoesNotExtendExpiryWithoutNewJWT(t *testing.T) {
	a := cookieTestAccount()
	h := newHunterHarness(a, hunterCoxProxy)
	now := time.Now()
	old := cookieTestObserve(t, h.gw, a, cookiePodA, hunterTestModel, now, now.Add(time.Minute))
	h.gw.observeOpenAICookieResponse(context.Background(), a, openAICookieRequest{model: hunterTestModel, sent: old, started: now.Add(time.Second)}, http.Header{}, hunterTestModel)
	require.Equal(t, old.Exp, readOpenAICookiePool(a)[0].Exp)
	renewed := cookieTestObserve(t, h.gw, a, cookiePodA, hunterTestModel, now.Add(2*time.Second), now.Add(time.Hour))
	h.gw.observeOpenAICookieResponse(context.Background(), a, openAICookieRequest{model: hunterTestModel, sent: old, started: now.Add(3 * time.Second)}, http.Header{}, hunterTestModel)
	require.Equal(t, renewed.Exp, readOpenAICookiePool(a)[0].Exp)
	cfg, _ := readOpenAITurnStateHunterConfig(a)
	pool := []openAICookieCandidate{old}
	require.NotNil(t, nextOpenAICookieRenewal(pool, hunterTestModel, now, openAICookieRenewLead(cfg), nil))
	require.Nil(t, nextOpenAICookieRenewal(pool, hunterTestModel, now, openAICookieRenewLead(cfg), map[string]bool{hunterTestModel + "/" + old.Pod: true}))
}
