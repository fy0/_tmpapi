package service

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenAITurnStateRouteCookieRoundTrip(t *testing.T) {
	account := &Account{ID: 424242, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	t.Cleanup(func() { openAITurnStateRouteCookies.Delete(account.ID) })
	repo := newTurnStateAutoRepo()
	repo.latest = account
	svc := &OpenAIGatewayService{accountRepo: repo}
	c, _ := newTurnStateTestContext(t, 7, "sess-cookie")

	upstream := http.Header{}
	upstream.Add("Set-Cookie", "__cflb=edge-1; Path=/; HttpOnly")
	upstream.Add("Set-Cookie", "other=ignore; Path=/")
	upstream.Add("Set-Cookie", "__oailb=route-1; Path=/")
	svc.noteOpenAITurnStateRouteCookies(c, account, upstream)

	fresh := turnStateFernetBlob(time.Now(), openAIHealthyTurnStateBlocks)
	other := turnStateFernetBlob(time.Now().Add(-time.Second), openAIHealthyTurnStateBlocks)
	for _, blob := range []string{fresh, other} {
		markOpenAITurnStateInjected(c, blob, turnStateSourceAuto)
		h := http.Header{}
		h.Set("Cookie", "session=keep")
		h.Set(openAICodexTurnStateHeader, blob)
		svc.applyOpenAITurnStateRouteCookie(c, account, h)
		require.Equal(t, "session=keep; __cflb=edge-1; __oailb=route-1", h.Get("Cookie"), "打票注入的票和 Cookie 不绑定")
	}

	echo := http.Header{}
	clearOpenAITurnStateInjected(c)
	echo.Set(openAICodexTurnStateHeader, fresh)
	svc.applyOpenAITurnStateRouteCookie(c, account, echo)
	require.Empty(t, echo.Get("Cookie"), "客户端自己回带的票不补打票 Cookie")

	degraded := turnStateFernetBlob(time.Now(), openAIHealthyTurnStateBlocks+1)
	markOpenAITurnStateInjected(c, degraded, turnStateSourceAuto)
	degradedHeader := http.Header{}
	degradedHeader.Set(openAICodexTurnStateHeader, degraded)
	svc.applyOpenAITurnStateRouteCookie(c, account, degradedHeader)
	require.Empty(t, degradedHeader.Get("Cookie"), "312 不补路由 Cookie")

	markOpenAITurnStateInjected(c, fresh, turnStateSourceAuto)
	c.Set(ctxKeyTurnStateProbe, true)
	probe := http.Header{}
	probe.Set(openAICodexTurnStateHeader, fresh)
	svc.applyOpenAITurnStateRouteCookie(c, account, probe)
	require.Empty(t, probe.Get("Cookie"), "探测保持裸发")

	require.NotEmpty(t, repo.extraWrites)
	require.Contains(t, repo.extraWrites[0], openAITurnStateRouteCookieExtraKey)

	// 另一个实例没有进程内缓存，从库里的 extra 读出来继续用。
	openAITurnStateRouteCookies.Delete(account.ID)
	remote := &Account{ID: account.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{
		openAITurnStateRouteCookieExtraKey: account.Extra[openAITurnStateRouteCookieExtraKey],
	}}
	repo.latest = remote
	local := &Account{ID: account.ID, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Extra: map[string]any{}}
	c.Set(ctxKeyTurnStateProbe, false)
	markOpenAITurnStateInjected(c, fresh, turnStateSourceAuto)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, fresh)
	svc.applyOpenAITurnStateRouteCookie(c, local, h)
	require.Equal(t, "__cflb=edge-1; __oailb=route-1", h.Get("Cookie"))
}

func TestOpenAITurnStateRouteCookieExpires(t *testing.T) {
	account := &Account{ID: 424243, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	t.Cleanup(func() { openAITurnStateRouteCookies.Delete(account.ID) })
	openAITurnStateRouteCookies.Store(account.ID, openAITurnStateRouteCookieState{
		cflb: "stale", oailb: "stale", capturedAt: time.Now().Add(-openAITurnStateRouteCookieTTL - time.Second),
	})
	svc := &OpenAIGatewayService{}
	c, _ := newTurnStateTestContext(t, 7, "sess-expired-cookie")
	blob := turnStateFernetBlob(time.Now(), openAIHealthyTurnStateBlocks)
	markOpenAITurnStateInjected(c, blob, turnStateSourceAuto)
	h := http.Header{}
	h.Set(openAICodexTurnStateHeader, blob)
	svc.applyOpenAITurnStateRouteCookie(c, account, h)
	require.Empty(t, h.Get("Cookie"))
}

func TestRememberOpenAITurnStateRouteCookieKeepsTheOtherName(t *testing.T) {
	const accountID = int64(424244)
	t.Cleanup(func() { openAITurnStateRouteCookies.Delete(accountID) })
	first := http.Header{}
	first.Add("Set-Cookie", "__cflb=a; Path=/")
	first.Add("Set-Cookie", "__oailb=b; Path=/")
	_, persist := rememberOpenAITurnStateRouteCookies(accountID, first)
	require.True(t, persist)

	second := http.Header{}
	second.Add("Set-Cookie", "__cflb=a; Path=/")
	state, persist := rememberOpenAITurnStateRouteCookies(accountID, second)
	require.False(t, persist, "值刚写过且间隔未到，不重复落库")
	require.Equal(t, "__cflb=a; __oailb=b", state.header())

	third := http.Header{}
	third.Add("Set-Cookie", "__cflb=a2; Path=/")
	state, persist = rememberOpenAITurnStateRouteCookies(accountID, third)
	require.True(t, persist, "换了值就要落库")
	require.Equal(t, "__cflb=a2; __oailb=b", state.header(), "没一起下发的那只还留着")
}
