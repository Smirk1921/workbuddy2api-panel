// export.go 账号导出：把池内账号导出成 cockpit 导入格式（可再导回任意实例）。
//
//	GET /panel/api/export/accounts?uids=uid1,uid2&credit=credits
//
// 口径对齐本地脚本「导出实例账号.ps1」与「账号」目录里的既有文件
// （如 `60800953-4000.json`）：
//   - 每个账号一个文件，单元素数组：{id,uid,email,nickname,access_token,
//     refresh_token,token_type,expires_at(毫秒),domain,status}
//   - 文件名 `<账号名>-<积分>.json`（同名追加 uid 前 8 位去重）
//   - 账号名：实例落盘的昵称是**打码形态**（703****0），用 access_token 的 JWT
//     claim `preferred_username` 还原，**并校验与打码值吻合**（前 3 位 + 尾部长度），
//     对不上就保留打码值并告警，避免张冠李戴。
//
// 输出形态：只命中 1 个账号 → 直接返回 .json；多个 → 打包成 zip（一个账号一个条目）。
// 全部在内存完成（账号数有界），不落临时文件。导出内容含凭证，响应带
// `Cache-Control: no-store`，且端点本身受面板 api_key 保护。
package panel

import (
	"archive/zip"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// exportAccount 导出的单账号记录。字段顺序与「账号」目录里的既有文件一致
// （json 结构体天然保序；不要改成 map，否则键序会随机）。
type exportAccount struct {
	ID           string `json:"id"`
	UID          string `json:"uid"`
	Email        string `json:"email"`
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresAt    int64  `json:"expires_at"`
	Domain       string `json:"domain"`
	Status       string `json:"status"`
}

// exportFile 一个待输出的文件（名 + 内容）。
type exportFile struct {
	Name string
	Data []byte
}

// exportAccounts 导出账号（JSON 或 ZIP）。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creditField := q.Get("credit")
	if creditField != "credits_total" {
		creditField = "credits" // 缺省 = 当前可用积分（与脚本默认一致）
	}

	// 目标账号：显式 uids（前端传勾选或当前可见集）优先；缺省 = 池内全部。
	var targets []pool.Status
	if raw := strings.TrimSpace(q.Get("uids")); raw != "" {
		var uids []string
		for _, s := range strings.Split(raw, ",") {
			if s = strings.TrimSpace(s); s != "" {
				uids = append(uids, s)
			}
		}
		if len(uids) == 0 {
			writeErr(w, http.StatusBadRequest, "uids 参数为空")
			return
		}
		if len(uids) > maxBatchUIDs {
			writeErr(w, http.StatusBadRequest,
				"一次最多导出 "+strconv.Itoa(maxBatchUIDs)+" 个账号（收到 "+strconv.Itoa(len(uids))+" 个）")
			return
		}
		targets, _ = p.targetsByUIDs(uids)
	} else {
		targets = p.cfg.Pool.List()
	}
	if len(targets) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可导出的账号")
		return
	}

	files, skipped, warns := p.buildExportFiles(targets, creditField)
	if len(files) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可导出的账号："+strings.Join(skipped, "；"))
		return
	}

	// 只有在「按 uid 导出」时，uid 不存在/未加载才值得单独回报（全量导出时池内即全部）。
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Export-Total", strconv.Itoa(len(targets)))
	w.Header().Set("X-Export-Files", strconv.Itoa(len(files)))
	w.Header().Set("X-Export-Skipped", strconv.Itoa(len(skipped)))
	w.Header().Set("X-Export-Warnings", strconv.Itoa(len(warns)))

	if len(files) == 1 {
		// 单账号直接给 .json：去掉分组目录前缀（响应头里的文件名不能带 /）。
		name := files[0].Name
		if i := strings.LastIndex(name, "/"); i >= 0 {
			name = name[i+1:]
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", contentDisposition(name))
		_, _ = w.Write(files[0].Data)
		return
	}

	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		contentDisposition("账号导出-"+time.Now().Format("20060102-150405")+".zip"))
	zw := zip.NewWriter(w)
	for _, f := range files {
		fw, err := zw.Create(f.Name)
		if err != nil {
			return
		}
		if _, err := fw.Write(f.Data); err != nil {
			return
		}
	}
	_ = zw.Close()
}

// buildExportFiles 按目标账号生成导出文件。skipped/warns 用于回报原因。
func (p *Panel) buildExportFiles(targets []pool.Status, creditField string) (files []exportFile, skipped, warns []string) {
	used := make(map[string]bool, len(targets))
	for _, st := range targets {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			skipped = append(skipped, st.UID+": 凭证未加载")
			continue
		}
		at := strings.TrimSpace(a.AccessTokenValue())
		if at == "" {
			skipped = append(skipped, st.UID+": 缺 access_token")
			continue
		}
		rt := a.RefreshTokenValue()
		claims := jwtClaims(at)

		// 账号名：打码值用 JWT 的 preferred_username 还原，并交叉校验。
		nick := strings.TrimSpace(st.Nickname)
		if strings.Contains(nick, "*") {
			switch {
			case claims.PreferredUsername == "":
				warns = append(warns, st.UID+": 昵称是打码值且 token 里无 preferred_username，保留原值")
			case maskMatches(nick, claims.PreferredUsername):
				nick = claims.PreferredUsername
			default:
				warns = append(warns, st.UID+": 打码值 "+nick+" 与 token 用户名 "+
					claims.PreferredUsername+" 不吻合，保留打码值")
			}
		}
		if nick == "" {
			nick = shortUID(st.UID)
		}

		credits := st.Credits
		if creditField == "credits_total" {
			credits = st.CreditsTotal
		}

		a.Lock()
		expSec := a.ExpiresAt
		a.Unlock()
		if expSec < 0 {
			expSec = 0
		}

		rec := exportAccount{
			ID:           st.UID,
			UID:          st.UID,
			Email:        claims.Email,
			Nickname:     nick,
			AccessToken:  at,
			RefreshToken: rt,
			TokenType:    "Bearer",
			ExpiresAt:    expSec * 1000, // 目标格式是毫秒
			Domain:       a.DomainValue(),
			Status:       "active",
		}
		data, err := json.MarshalIndent([]exportAccount{rec}, "", "  ")
		if err != nil {
			skipped = append(skipped, st.UID+": 序列化失败: "+err.Error())
			continue
		}
		data = append(data, '\n') // 与脚本 Set-Content 的结尾换行一致

		// 按分组目录组织：解压后直接是「主力/xxx.json」「备机/xxx.json」「未分组/xxx.json」。
		// 组名同样做文件名安全化（组名是用户自由输入，可能带 / \ 之类）。
		dir := safeExportName(strings.TrimSpace(st.Group), "")
		if dir == "" {
			dir = "未分组"
		}
		base := safeExportName(nick+"-"+strconv.FormatInt(credits, 10), shortUID(st.UID))
		name := dir + "/" + base + ".json"
		if used[name] {
			// 同组内同名（账号名 + 积分都相同）→ 追加 uid 前 8 位区分，绝不覆盖。
			name = dir + "/" + base + "-" + shortUID(st.UID) + ".json"
		}
		used[name] = true
		files = append(files, exportFile{Name: name, Data: data})
	}
	return files, skipped, warns
}

// shortUID 取 uid 前 8 位作为兜底名（与脚本同口径）。
func shortUID(uid string) string {
	if len(uid) > 8 {
		return uid[:8]
	}
	if uid == "" {
		return "account"
	}
	return uid
}

// safeExportName 文件名安全化：替换 Windows 非法字符与控制字符、去尾点、限 60 字符。
func safeExportName(name, fallback string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20, r == '\\', r == '/', r == ':', r == '*', r == '?', r == '"', r == '<', r == '>', r == '|':
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	s := strings.TrimSpace(b.String())
	s = strings.TrimRight(s, ".")
	if r := []rune(s); len(r) > 60 {
		s = string(r[:60])
	}
	if s == "" {
		return fallback
	}
	return s
}

// maskMatches 校验打码昵称与全量用户名是否吻合：
// mask 应为 full 的「前 3 位 + 中间若干 * + 尾部若干位」（手机号形态尾部是末 4 位）。
// 不吻合就说明 token 与账号对不上，宁可保留打码值也不猜。
func maskMatches(mask, full string) bool {
	if mask == "" || full == "" || !strings.Contains(mask, "*") {
		return false
	}
	mr, fr := []rune(mask), []rune(full)
	if len(mr) < 5 || len(fr) < 5 {
		return false
	}
	const head = 3
	if string(mr[:head]) != string(fr[:head]) {
		return false
	}
	star := -1
	for i, r := range mr {
		if r == '*' {
			star = i
		}
	}
	if star < 0 {
		return false
	}
	tail := len(mr) - star - 1
	if tail <= 0 || len(fr) < tail {
		return false
	}
	return string(mr[len(mr)-tail:]) == string(fr[len(fr)-tail:])
}

// contentDisposition 生成带 UTF-8 文件名的 Content-Disposition（RFC 5987）。
// 纯 ASCII 走普通 filename；含中文时同时给 ASCII 兜底名 + filename*。
func contentDisposition(name string) string {
	ascii := true
	for _, r := range name {
		if r > 127 {
			ascii = false
			break
		}
	}
	if ascii {
		return `attachment; filename="` + name + `"`
	}
	fallback := "export.json"
	if strings.HasSuffix(strings.ToLower(name), ".zip") {
		fallback = "export.zip"
	}
	return `attachment; filename="` + fallback + `"; filename*=UTF-8''` + url.PathEscape(name)
}
