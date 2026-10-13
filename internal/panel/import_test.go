// 账号导入端点契约测试：文件形态兼容（数组/单对象/信封/BOM）、
// 「只有 access token」的凭证可导入（refresh_token 可空）、过期时间回退链、
// 以及跳过原因/警告是否如实回报给面板。
package panel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// testJWT 造一个只有 payload 的假 JWT。导入端只读 claims、不验签，故无需真实签名。
func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
}

func TestNormalizeExpiry(t *testing.T) {
	cases := []struct {
		in, want int64
		why      string
	}{
		{0, 0, "缺省"},
		{-1, 0, "负值"},
		{1794223192000, 1794223192, "毫秒 → 秒"},
		{1794223192, 1794223192, "已是秒（旧实现会除成 1970）"},
		{12345, 0, "量级荒谬 → 无效"},
	}
	for _, c := range cases {
		if got := normalizeExpiry(c.in); got != c.want {
			t.Errorf("normalizeExpiry(%d)=%d want %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

func TestDecodeCockpitAccounts(t *testing.T) {
	one := `{"uid":"u1","access_token":"at"}`
	cases := []struct {
		name string
		in   string
		want int
		bad  bool
	}{
		{"顶层数组", `[` + one + `,` + one + `]`, 2, false},
		{"顶层单对象", one, 1, false},
		{"信封 accounts", `{"accounts":[` + one + `]}`, 1, false},
		{"信封 data", `{"data":[` + one + `]}`, 1, false},
		{"带 UTF-8 BOM", "\xef\xbb\xbf" + `[` + one + `]`, 1, false},
		{"空文件", "   ", 0, true},
		{"裸字符串", `"nope"`, 0, true},
		{"坏 JSON", `[{"uid":`, 0, true},
	}
	for _, c := range cases {
		got, err := decodeCockpitAccounts([]byte(c.in))
		if c.bad {
			if err == nil {
				t.Errorf("%s: 期望报错，实际 ok (%d 条)", c.name, len(got))
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 意外报错 %v", c.name, err)
			continue
		}
		if len(got) != c.want {
			t.Errorf("%s: 解出 %d 条 want %d", c.name, len(got), c.want)
		}
	}
}

// 核心回归：cockpit tools 对「只拿到 access token」的账号导出 refresh_token="" 且
// expires_at=0。旧实现把这类记录与「字段全缺」合并跳过，导入必然 0 成功。
func TestPlanImportAccountWithoutRefreshToken(t *testing.T) {
	exp := time.Now().Add(27 * 24 * time.Hour).Unix()
	at := testJWT(t, map[string]any{"exp": exp, "sub": "08cfc50d-9ed4-4dde-97d4-9b3e70e28d24"})

	plan, err := planImportAccount(cockpitAccount{
		ID:          "08cfc50d-9ed4-4dde-97d4-9b3e70e28d24",
		UID:         "08cfc50d-9ed4-4dde-97d4-9b3e70e28d24",
		Nickname:    "63358934",
		AccessToken: at,
		Domain:      "www.workbuddy.cn",
	}, t.TempDir())
	if err != nil {
		t.Fatalf("无 refresh_token 的账号必须可导入，实际报错: %v", err)
	}
	if plan.auth.RefreshToken != "" {
		t.Errorf("refresh_token 应为空，得到 %q", plan.auth.RefreshToken)
	}
	// expires_at=0 → 回退 access_token 的 JWT exp（而非旧实现的 now+365d 假值）。
	if plan.auth.ExpiresAt != exp {
		t.Errorf("ExpiresAt=%d want %d（应取自 JWT exp）", plan.auth.ExpiresAt, exp)
	}
	if plan.realm != "cn" {
		t.Errorf("realm=%q want cn", plan.realm)
	}
	if len(plan.warnings) != 1 || !bytes.Contains([]byte(plan.warnings[0]), []byte("无 refresh_token")) {
		t.Errorf("应给出一条「无 refresh_token」警告，得到 %v", plan.warnings)
	}
	if filepath.Base(plan.auth.FilePath) != "workbuddy-08cfc50d-9ed4-4dde-97d4-9b3e70e28d24.json" {
		t.Errorf("落盘文件名异常: %s", plan.auth.FilePath)
	}
}

// uid 缺失时回退 JWT 的 sub；过期时间毫秒优先于 JWT exp。
func TestPlanImportAccountFallbacks(t *testing.T) {
	sub := "088bfcdf-9b9b-42ff-90b9-fd98b173298b"
	jwtExp := time.Now().Add(48 * time.Hour).Unix()
	at := testJWT(t, map[string]any{"exp": jwtExp, "sub": sub})
	fileExp := time.Now().Add(72 * time.Hour).Unix()

	plan, err := planImportAccount(cockpitAccount{
		AccessToken:  at,
		ExpiresAt:    fileExp * 1000, // 毫秒
		RefreshToken: "rt",
		Domain:       "www.workbuddy.cn",
	}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if plan.auth.UID != sub {
		t.Errorf("uid 应回退 JWT sub=%s，得到 %q", sub, plan.auth.UID)
	}
	if plan.auth.ExpiresAt != fileExp {
		t.Errorf("ExpiresAt=%d want %d（expires_at 毫秒应优先且正确换算）", plan.auth.ExpiresAt, fileExp)
	}
	if len(plan.warnings) != 0 {
		t.Errorf("完整凭证不该有警告，得到 %v", plan.warnings)
	}
}

func TestPlanImportAccountRejects(t *testing.T) {
	cases := []struct {
		name string
		acc  cockpitAccount
		want string
	}{
		{"缺 access_token", cockpitAccount{UID: "u1"}, "missing access_token"},
		{"缺 uid 且 JWT 无 sub", cockpitAccount{AccessToken: "not-a-jwt"}, "missing uid"},
		{"uid 含路径字符", cockpitAccount{UID: "a/../b", AccessToken: "x"}, "invalid uid"},
	}
	for _, c := range cases {
		_, err := planImportAccount(c.acc, t.TempDir())
		if err == nil || !bytes.Contains([]byte(err.Error()), []byte(c.want)) {
			t.Errorf("%s: err=%v want 含 %q", c.name, err, c.want)
		}
	}
}

// 端点级回归：导入一份「只有 access token」的真实形态文件，必须 1 成功 0 跳过，
// 并把警告回给面板（此前是 0 成功 1 跳过 + 原因只进 console.warn）。
func TestImportCockpitEndpointAcceptsTokenOnlyAccount(t *testing.T) {
	dir := t.TempDir()
	pl := pool.New("")
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, AuthDir: dir})

	exp := time.Now().Add(27 * 24 * time.Hour).Unix()
	file := `[{"id":"08cfc50d-9ed4-4dde-97d4-9b3e70e28d24","uid":"08cfc50d-9ed4-4dde-97d4-9b3e70e28d24",` +
		`"email":"","nickname":"63358934","access_token":"` +
		testJWT(t, map[string]any{"exp": exp, "sub": "08cfc50d-9ed4-4dde-97d4-9b3e70e28d24"}) +
		`","refresh_token":"","token_type":"Bearer","expires_at":0,"domain":"www.workbuddy.cn","status":"active"}]`

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "63358934-4300.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write([]byte(file)); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		OK       bool     `json:"ok"`
		Total    int      `json:"total"`
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.OK || got.Total != 1 || got.Imported != 1 || got.Skipped != 0 {
		t.Fatalf("resp=%+v（期望 1 成功 0 跳过）", got)
	}
	if len(got.Warnings) != 1 {
		t.Fatalf("warnings=%v（应回报无 refresh_token 警告）", got.Warnings)
	}
	// 凭证确实落盘 + 进池。
	if _, err := os.Stat(filepath.Join(dir, "workbuddy-08cfc50d-9ed4-4dde-97d4-9b3e70e28d24.json")); err != nil {
		t.Fatalf("auth 文件未落盘: %v", err)
	}
	if _, ok := pl.Status("08cfc50d-9ed4-4dde-97d4-9b3e70e28d24"); !ok {
		t.Fatal("账号未进池")
	}
}

// 缺 access_token 的记录必须仍然被跳过并回报原因（不得静默）。
func TestImportCockpitEndpointReportsSkipReasons(t *testing.T) {
	pl := pool.New("")
	p := New(Config{Version: "test", APIKey: "test-key", Pool: pl, AuthDir: t.TempDir()})

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "bad.json")
	fw.Write([]byte(`[{"id":"x","uid":"u1"}]`))
	mw.Close()

	req := httptest.NewRequest("POST", "/panel/api/import/cockpit", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-key")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)

	var got struct {
		Imported int      `json:"imported"`
		Skipped  int      `json:"skipped"`
		Errors   []string `json:"errors"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Imported != 0 || got.Skipped != 1 || len(got.Errors) != 1 {
		t.Fatalf("resp=%+v（应 0 成功 1 跳过且带原因）", got)
	}
}
