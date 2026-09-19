package main

import (
	"errors"
	"testing"

	"traework2api/internal/upstream"
)

// TestIsAlreadyCheckedIn 覆盖已签判定（逻辑在 upstream.IsAlreadyCheckedIn）。
func TestIsAlreadyCheckedIn(t *testing.T) {
	// 明确"已签到"标记 → true
	for _, msg := range []string{"今日已签到", "you have already checked in"} {
		if !upstream.IsAlreadyCheckedIn(errors.New(msg)) {
			t.Errorf("%q should be already", msg)
		}
	}
	// CheckinError 携带已签文案 → true
	if !upstream.IsAlreadyCheckedIn(&upstream.CheckinError{Code: 9095, Msg: "今日已签到"}) {
		t.Error("CheckinError with 已签到 should be already")
	}
	// 哨兵错误 → true
	if !upstream.IsAlreadyCheckedIn(upstream.ErrAlreadyCheckedIn) {
		t.Error("sentinel ErrAlreadyCheckedIn should be already")
	}
	// 歧义/错误路径 → false（不误判为已签）
	for _, msg := range []string{
		"checkin service error",
		"upstream 429: checkin rate limited",
		"code=400 bad request",
		"",
	} {
		if upstream.IsAlreadyCheckedIn(errors.New(msg)) {
			t.Errorf("%q should NOT be already", msg)
		}
	}
	// CheckinError 非已签文案 → false
	if upstream.IsAlreadyCheckedIn(&upstream.CheckinError{Code: 1001, Msg: "internal error"}) {
		t.Error("CheckinError with unrelated msg should NOT be already")
	}
}
