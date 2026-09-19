// signin 一次性批量签到工具：遍历 ./auths/trae-*.json 全部账号，
// 自动 RefreshToken（过期时），逐个签到，顺手查积分。
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"traework2api/internal/auth"
	"traework2api/internal/upstream"
)

type row struct {
	file   string
	uid    string
	nick   string
	status string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail string
	remain int64
	hasRem bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	sort.Strings(files)
	up := upstream.New()

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		r := row{file: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// refresh 过期 token
		if a.NeedsRefresh(2 * time.Hour) {
			if err := up.RefreshToken(a); err != nil {
				if ue, ok := err.(*upstream.Error); ok && ue.Kind == upstream.ErrSessionDead {
					r.status = "AUTH_INVALID"
				} else {
					r.status = "FAIL"
				}
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			_ = a.SaveAtomic()
		}

		// 签到闭环：status → claim → status 复核。
		// 已签到不算失败；HTTP 200 但业务失败的响应不再被误判为成功。
		switch err := up.DailyCheckin(a); {
		case err == nil:
			r.status = "OK"
			okN++
		case upstream.IsAlreadyCheckedIn(err):
			r.status = "ALREADY"
			r.detail = short(err.Error())
			alreadyN++
		default:
			r.status = "FAIL"
			r.detail = short(err.Error())
			failN++
		}
		// 查积分
		if remain, qerr := up.UserEntUsage(a); qerr == nil {
			r.remain, r.hasRem = remain, true
		}
		rows = append(rows, r)
	}

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasRem {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			trunc(r.uid, 36), trunc(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}
