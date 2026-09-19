package upstream

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"traework2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{200, `{"code":1005,"message":"plan limit","extra":{"plan":2}}`, ErrPlanLimit},
		{200, `{"code":1005,"msg":"权益不足"}`, ErrPlanLimit},
		{429, ``, ErrSoftRate},
		{401, `{"code":1001,"msg":"login required"}`, ErrSessionDead},
		{401, ``, ErrSessionDead},
		{404, ``, ErrNotFound},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{400, `{"code":11101,"msg":"bad param"}`, ErrClient},
		{200, `{"checked_in":false}`, ErrNone},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:      &http.Client{Transport: fn},
		AgentHost: "https://agent.example",
		UgHost:    "https://ug.example",
		OAuthHost: "https://oauth.example",
		ClientID:  ClientID,
	}
}

func TestRefreshTokenExchange(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpExchange) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			return nil, errors.New("missing content-type")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ClientID":"en1oxy7wnw8j9n"`)) || !bytes.Contains(body, []byte(`"RefreshToken":"oldrt"`)) {
			return nil, errors.New("bad body: " + string(body))
		}
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786805537,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt != 1786805537 {
		t.Errorf("expiresAt=%d", a.ExpiresAt)
	}
}

// TestRefreshTokenExchangeMilliseconds 覆盖上游 TokenExpireAt 返回毫秒的场景：
// 必须归一化为 Unix 秒后再写 auth.ExpiresAt。
func TestRefreshTokenExchangeMilliseconds(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141,"TokenExpireDuration":1209600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1786847930 {
		t.Errorf("expiresAt=%d want 1786847930 (毫秒转秒)", a.ExpiresAt)
	}
}

func TestRefreshTokenIfNeededSkipsFresh(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed {
		t.Error("fresh token should not refresh")
	}
	if calls != 0 {
		t.Errorf("ExchangeToken should not be called, calls=%d", calls)
	}
	if a.AccessToken != "at" {
		t.Error("token should remain unchanged")
	}
}

func TestRefreshTokenIfNeededRefreshesExpired(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"Result":{"Token":"newat","RefreshToken":"newrt","TokenExpireAt":1786847930141}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://oauth.example"}
	refreshed, err := c.RefreshTokenIfNeeded(a, 24*3600*1e9)
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed || calls != 1 {
		t.Errorf("expired token should refresh once, refreshed=%v calls=%d", refreshed, calls)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

func TestRefreshTokenUsesAuthApiHost(t *testing.T) {
	var gotHost string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotHost = r.URL.Scheme + "://" + r.URL.Host
		return jsonResp(200, `{"Result":{"Token":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1, ApiHost: "https://custom.example"}
	if err := c.RefreshToken(a); err != nil {
		t.Fatal(err)
	}
	if gotHost != "https://custom.example" {
		t.Errorf("host=%s want auth.apiHost", gotHost)
	}
}

func TestChatStreamSendsHeadersAndRewritesBody(t *testing.T) {
	var gotAuth, gotUID, gotAppID, gotIdeVer string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-Uid")
		gotAppID = r.Header.Get("X-App-Id")
		gotIdeVer = r.Header.Get("X-Ide-Version")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", MachineID: "m1", DeviceID: "d1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Cloud-IDE-JWT at" || gotUID != "u1" {
		t.Errorf("headers: auth=%q uid=%q", gotAuth, gotUID)
	}
	if gotAppID != AppID || gotIdeVer != IdeVersion {
		t.Errorf("app headers: appid=%q idever=%q", gotAppID, gotIdeVer)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) || !bytes.Contains(gotBody, []byte(`"function":"solo_work_lite"`)) {
		t.Errorf("body not rewritten: %s", gotBody)
	}
}

func TestChatStreamUsesDedicatedStreamClient(t *testing.T) {
	// StreamHTTP 优先于 HTTP 被 ChatStream 使用（无总超时的长 SSE 流客户端）。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("event:done\ndata:{\"finish_reason\":\"stop\"}\n\n")),
		}, nil
	})
	c.StreamHTTP = &http.Client{Transport: c.HTTP.Transport} // 无 Timeout
	rc, status, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{"model":"glm-5.2","messages":[]}`))
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if c.StreamHTTP.Timeout != 0 {
		t.Errorf("stream client should have no total timeout, got %v", c.StreamHTTP.Timeout)
	}
}

func TestChatStreamHTTPError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `rate limited`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`))
	if status != 429 {
		t.Errorf("status=%d", status)
	}
	if err != nil {
		t.Fatalf("429 should come via status, err=%v", err)
	}
	if Classify(status, string(respBody)) != ErrSoftRate {
		t.Errorf("not classified soft rate: %q", respBody)
	}
}

func TestUserEntUsageAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, EpEntUsage) {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT at" {
			return nil, errors.New("missing auth header")
		}
		return jsonResp(200, `{"is_credits_billing":true,"user_entitlement_pack_list":[
			{"entitlement_base_info":{"quota":{"credits_limit":2000}}},
			{"entitlement_base_info":{"quota":{"credits_limit":500}}}
		]}`), nil
	})
	remain, err := c.UserEntUsage(&auth.Auth{AccessToken: "at"})
	if err != nil {
		t.Fatalf("ent usage: %v", err)
	}
	if remain != 2500 {
		t.Errorf("remain=%d want 2500", remain)
	}
}

func TestCheckinStatusSendsOfficialDeviceHeaders(t *testing.T) {
	var path string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		path = r.URL.Path
		if r.Header.Get("X-Device-Id") != "1111222233334444" || r.Header.Get("X-Device-Brand") != "90SB001GCD" || r.Header.Get("X-Device-Type") != "windows" {
			return nil, errors.New("missing official device headers")
		}
		return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
	})
	checkedIn, credits, enable, err := c.CheckinStatus(&auth.Auth{AccessToken: "at", DeviceID: "model-device", CheckinDeviceID: "1111222233334444", CheckinDeviceBrand: "90SB001GCD", CheckinDeviceType: "windows"})
	if err != nil {
		t.Fatal(err)
	}
	if checkedIn || !enable || credits != 200 {
		t.Errorf("status: checked=%v enable=%v credits=%d", checkedIn, enable, credits)
	}
	if path != EpCheckinStatus {
		t.Errorf("path=%s", path)
	}
}

// TestCheckinClaimBusinessError HTTP 200 + 业务码非零必须报错（回归：曾丢弃响应体误判成功）。
func TestCheckinClaimBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":1005,"message":"活动已结束"}`), nil
	})
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("code!=0 with HTTP 200 must be an error, got nil")
	}
	var ce *CheckinError
	if !errors.As(err, &ce) || ce.Code != 1005 {
		t.Errorf("want CheckinError code=1005, got %v", err)
	}
	if !strings.Contains(err.Error(), "活动已结束") {
		t.Errorf("error should carry upstream message, got %v", err)
	}
}

// TestCheckinClaimSuccessFalse HTTP 200 + success=false 也要报错。
func TestCheckinClaimSuccessFalse(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"success":false,"msg":"risk control"}`), nil
	})
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("success=false must be an error")
	}
}

// TestCheckinClaimSuccessFieldAbsent 字段缺失（非显式 false）不算失败。
func TestCheckinClaimSuccessFieldAbsent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"message":"ok"}`), nil
	})
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("absent success field should not fail: %v", err)
	}
}

// TestCheckinClaimRateLimitedRetry code=9074 应等一个重试间隔后重试，第二次成功。
func TestCheckinClaimRateLimitedRetry(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return jsonResp(200, `{"code":9074,"message":"too many requests"}`), nil
		}
		return jsonResp(200, `{"code":0,"message":"success"}`), nil
	})
	c.CheckinRetryDelay = time.Millisecond
	if err := c.CheckinClaim(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if calls != 2 {
		t.Errorf("claim calls=%d want 2", calls)
	}
}

// TestCheckinClaimRateLimitedExhausted 连续 9074 时只重试一次并报错。
func TestCheckinClaimRateLimitedExhausted(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return jsonResp(200, `{"code":9074,"message":"too many requests"}`), nil
	})
	c.CheckinRetryDelay = time.Millisecond
	err := c.CheckinClaim(&auth.Auth{AccessToken: "at"})
	if err == nil {
		t.Fatal("persistent 9074 should fail")
	}
	var ce *CheckinError
	if !errors.As(err, &ce) || ce.Code != CodeCheckinRateLimited {
		t.Errorf("want rate-limit CheckinError, got %v", err)
	}
	if calls != 2 {
		t.Errorf("should retry exactly once, calls=%d", calls)
	}
}

// TestDailyCheckinVerifiesClaim claim 返回 200 但复核仍未签到 → 必须报错。
func TestDailyCheckinVerifiesClaim(t *testing.T) {
	statusCalls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, EpCheckinClaim):
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		default:
			statusCalls++
			return jsonResp(200, `{"checked_in":false,"credits":200,"enable":true}`), nil
		}
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("claim accepted but still unchecked must fail verification")
	}
	if !strings.Contains(err.Error(), "verification failed") {
		t.Errorf("want verification error, got %v", err)
	}
	if statusCalls != 2 {
		t.Errorf("status calls=%d want 2 (pre-check + verify)", statusCalls)
	}
}

// TestDailyCheckinSuccess claim 后复核为已签 → 成功。
func TestDailyCheckinSuccess(t *testing.T) {
	checked := false
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, EpCheckinClaim):
			checked = true
			return jsonResp(200, `{"code":0,"message":"success"}`), nil
		default:
			return jsonResp(200, `{"checked_in":`+strconv.FormatBool(checked)+`,"credits":200,"enable":true}`), nil
		}
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); err != nil {
		t.Fatalf("daily checkin: %v", err)
	}
}

// TestDailyCheckinAlready 已签到返回哨兵错误，且不再调用 claim。
func TestDailyCheckinAlready(t *testing.T) {
	claimCalls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, EpCheckinClaim) {
			claimCalls++
		}
		return jsonResp(200, `{"checked_in":true,"credits":200,"enable":true}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if !errors.Is(err, ErrAlreadyCheckedIn) {
		t.Fatalf("want ErrAlreadyCheckedIn, got %v", err)
	}
	if !IsAlreadyCheckedIn(err) {
		t.Error("IsAlreadyCheckedIn should accept sentinel")
	}
	if claimCalls != 0 {
		t.Errorf("already checked in should skip claim, calls=%d", claimCalls)
	}
}

// TestDailyCheckinDisabled 活动未开启 → ErrCheckinDisabled。
func TestDailyCheckinDisabled(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"checked_in":false,"credits":0,"enable":false}`), nil
	})
	if err := c.DailyCheckin(&auth.Auth{AccessToken: "at"}); !errors.Is(err, ErrCheckinDisabled) {
		t.Fatalf("want ErrCheckinDisabled, got %v", err)
	}
}

// TestCheckinStatusBusinessError status 接口的业务失败同样要报错。
func TestCheckinStatusBusinessError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":20101,"msg":"login required"}`), nil
	})
	if _, _, _, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"}); err == nil {
		t.Fatal("status code!=0 must be an error")
	}
}

// TestCheckinStatusRejectsMissingStateFields 状态响应缺 checked_in/enable 字段属于
// 协议异常，必须报错而不是当成「未签到」去 claim（PR #9）。
func TestCheckinStatusRejectsMissingStateFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"message":"success"}`), nil
	})
	_, _, _, err := c.CheckinStatus(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "missing checked_in or enable") {
		t.Fatalf("error=%v want protocol failure", err)
	}
}
