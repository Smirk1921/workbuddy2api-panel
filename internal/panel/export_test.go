// 账号导出测试：格式与文件名对齐「账号」目录里的既有文件（60800953-4000.json），
// 覆盖打码昵称还原/校验、按分组目录打包、同名去重、缺凭证跳过、单账号直出 JSON。
package panel

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// newExportPanel 组装一个带凭证的面板。
func newExportPanel(t *testing.T, accts ...*auth.Auth) (*pool.Pool, *Panel, func(query string) *httptest.ResponseRecorder) {
	t.Helper()
	pl := pool.New("")
	for _, a := range accts {
		pl.Add(a)
	}
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl})
	get := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/panel/api/export/accounts"+query, nil)
		req.Header.Set("Authorization", "Bearer test-key")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		return rec
	}
	return pl, p, get
}

func exportJWT(t *testing.T, sub, nick, email string, exp int64) string {
	t.Helper()
	return testJWT(t, map[string]any{
		"exp": exp, "sub": sub, "preferred_username": nick, "email": email,
	})
}

// 单账号：直出 .json，文件名 <账号名>-<积分>.json，内容与目标格式逐字段一致。
func TestExportSingleAccountJSON(t *testing.T) {
	at := exportJWT(t, "u1", "60800953", "a@b.c", 1794117146)
	pl, _, get := newExportPanel(t, &auth.Auth{
		UID: "u1", Nickname: "608****3", AccessToken: at, RefreshToken: "rt",
		Domain: "www.workbuddy.cn", ExpiresAt: 1794117146,
	})
	pl.SetCredits("u1", 4000, 5000)
	pl.SetGroup("u1", "主力")

	rec := get("?uids=u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type=%q want json", ct)
	}
	cd := rec.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "60800953-4000.json") {
		t.Errorf("Content-Disposition=%q（应含 60800953-4000.json）", cd)
	}
	// 打码昵称应被 JWT 里的全量用户名还原
	var list []exportAccount
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("len=%d want 1", len(list))
	}
	got := list[0]
	want := exportAccount{
		ID: "u1", UID: "u1", Email: "a@b.c", Nickname: "60800953",
		AccessToken: at, RefreshToken: "rt", TokenType: "Bearer",
		ExpiresAt: 1794117146000, Domain: "www.workbuddy.cn", Status: "active",
	}
	if got != want {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
	// 键名与顺序：结构体序列化天然保序，这里只校验键集合齐全。
	for _, k := range []string{`"id"`, `"uid"`, `"email"`, `"nickname"`, `"access_token"`,
		`"refresh_token"`, `"token_type"`, `"expires_at"`, `"domain"`, `"status"`} {
		if !strings.Contains(rec.Body.String(), k) {
			t.Errorf("导出 JSON 缺键 %s", k)
		}
	}
}

// 多账号：zip，条目按分组目录组织；未分组进「未分组/」。
func TestExportMultiAccountZip(t *testing.T) {
	pl, _, get := newExportPanel(t,
		&auth.Auth{UID: "u1", Nickname: "n1", AccessToken: exportJWT(t, "u1", "n1", "", 100), RefreshToken: "r1", Domain: "www.workbuddy.cn", ExpiresAt: 100},
		&auth.Auth{UID: "u2", Nickname: "n2", AccessToken: exportJWT(t, "u2", "n2", "", 200), RefreshToken: "r2", Domain: "www.workbuddy.cn", ExpiresAt: 200},
		&auth.Auth{UID: "u3", Nickname: "n3", AccessToken: exportJWT(t, "u3", "n3", "", 300), RefreshToken: "r3", Domain: "www.workbuddy.ai", ExpiresAt: 300},
	)
	pl.SetCredits("u1", 4000, 0)
	pl.SetCredits("u2", 3500, 0)
	pl.SetCredits("u3", 1200, 0)
	pl.SetGroup("u1", "主力")
	pl.SetGroup("u2", "主力")
	// u3 未分组

	rec := get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type=%q want application/zip", ct)
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	joined := strings.Join(names, "|")
	for _, want := range []string{"主力/n1-4000.json", "主力/n2-3500.json", "未分组/n3-1200.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("zip 条目缺 %q（实际 %v）", want, names)
		}
	}
	// 抽一个条目验证内容仍是单元素数组
	for _, f := range zr.File {
		if f.Name != "主力/n1-4000.json" {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(rc)
		rc.Close()
		var list []exportAccount
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || list[0].Nickname != "n1" || list[0].ExpiresAt != 100000 {
			t.Errorf("条目内容异常: %+v", list)
		}
	}
}

// 打码值对不上 token 用户名：保留打码值并计入 warnings。
func TestExportMaskMismatchKeepsMasked(t *testing.T) {
	_, _, get := newExportPanel(t, &auth.Auth{
		UID: "u1", Nickname: "608****3", AccessToken: exportJWT(t, "u1", "99999999", "", 100),
		RefreshToken: "r1", Domain: "www.workbuddy.cn", ExpiresAt: 100,
	})
	rec := get("?uids=u1")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	if rec.Header().Get("X-Export-Warnings") != "1" {
		t.Errorf("应计 1 条 warning，头=%q", rec.Header().Get("X-Export-Warnings"))
	}
	var list []exportAccount
	_ = json.Unmarshal(rec.Body.Bytes(), &list)
	if list[0].Nickname != "608****3" {
		t.Errorf("不吻合时应保留打码值，实得 %q", list[0].Nickname)
	}
}

// 缺 access_token 的账号跳过并计入 skipped；同组同名去重。
func TestExportSkipsAndDedup(t *testing.T) {
	at := exportJWT(t, "u1", "dup", "", 100)
	pl, _, get := newExportPanel(t,
		&auth.Auth{UID: "u1", Nickname: "dup", AccessToken: at, RefreshToken: "r", Domain: "www.workbuddy.cn", ExpiresAt: 100},
		&auth.Auth{UID: "u2", Nickname: "dup", AccessToken: exportJWT(t, "u2", "dup", "", 100), RefreshToken: "r", Domain: "www.workbuddy.cn", ExpiresAt: 100},
		&auth.Auth{UID: "u3", Nickname: "noToken", RefreshToken: "r", Domain: "www.workbuddy.cn"},
	)
	pl.SetCredits("u1", 100, 0)
	pl.SetCredits("u2", 100, 0)
	pl.SetGroup("u1", "组A")
	pl.SetGroup("u2", "组A")

	rec := get("")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Export-Skipped") != "1" {
		t.Errorf("skipped=%q want 1", rec.Header().Get("X-Export-Skipped"))
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("条目数=%d want 2（两个 dup，缺 token 的跳过）", len(zr.File))
	}
	// 同组同名：第二个追加 uid 前 8 位
	if zr.File[0].Name == zr.File[1].Name {
		t.Fatalf("同组同名未去重: %v", []string{zr.File[0].Name, zr.File[1].Name})
	}
}

// credit=credits_total 时文件名用总额。
func TestExportCreditField(t *testing.T) {
	pl, _, get := newExportPanel(t, &auth.Auth{
		UID: "u1", Nickname: "n1", AccessToken: exportJWT(t, "u1", "n1", "", 100),
		RefreshToken: "r1", Domain: "www.workbuddy.cn", ExpiresAt: 100,
	})
	pl.SetCredits("u1", 4000, 5000)
	rec := get("?uids=u1&credit=credits_total")
	if !strings.Contains(rec.Header().Get("Content-Disposition"), "n1-5000.json") {
		t.Errorf("Content-Disposition=%q（应含 n1-5000.json）", rec.Header().Get("Content-Disposition"))
	}
}

// 过滤非法组名/昵称字符，避免 zip 条目里出现路径穿越。
func TestExportSafeNames(t *testing.T) {
	if got := safeExportName("a/b\\c:d*e?f\"g<h>i|j", "x"); strings.ContainsAny(got, `/\\:*?"<>|`) {
		t.Errorf("非法字符未替换: %q", got)
	}
	if got := safeExportName("   ", "fallback"); got != "fallback" {
		t.Errorf("空白应回落 fallback，实得 %q", got)
	}
	if got := safeExportName(strings.Repeat("组", 80), "x"); len([]rune(got)) > 60 {
		t.Errorf("应截到 60 字符，实得 %d", len([]rune(got)))
	}
	// 打码校验
	if !maskMatches("608****3", "60800953") {
		t.Error("608****3 与 60800953 应吻合")
	}
	if maskMatches("608****3", "99999999") {
		t.Error("前缀不同不该吻合")
	}
	if maskMatches("60800953", "60800953") {
		t.Error("没有 * 不算打码值")
	}
}
