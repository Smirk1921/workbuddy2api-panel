// export.go 账号导出：把池内账号导出成 cockpit 导入格式（可再导回任意实例）。
//
//	GET /panel/api/export/accounts?uids=uid1,uid2&credit=credits
//
// 输出**始终是 .json 文件**（不打包 zip），`mode` 控制多账号的交付方式：
//   - mode=combined（默认）：
//     · 只命中 1 个账号 → 文件名 `<账号名>-<积分>.json`，内容是与「账号」目录里的
//     既有文件（如 60800953-4000.json）**逐字段一致**的单元素数组；
//     · 命中多个账号 → 文件名 `账号导出-<时间戳>.json`，内容是这些账号的数组。
//     多账号时每条记录额外带 `group` 与 `credits`（导入端忽略未知字段）。
//   - mode=split：
//     返回一个文件清单（JSON），每个账号一项：`name` / `group` / `credits` /
//     `content`（规范 10 字段）。面板前端按清单逐账号下载 `<账号名>-<积分>.json`。
//     服务端不生成 zip；需要按组目录/压缩时仍由本地脚本处理。
//
// 口径对齐本地脚本「导出实例账号.ps1」：
//   - 字段：{id,uid,email,nickname,access_token,refresh_token,token_type,
//     expires_at(毫秒),domain,status}
//   - 账号名：实例落盘的昵称是**打码形态**（703****0），用 access_token 的 JWT
//     claim `preferred_username` 还原，**并校验与打码值吻合**（前 3 位 + 尾部长度），
//     对不上就保留打码值并告警，避免张冠李戴。
//
// 导出内容含凭证：响应带 `Cache-Control: no-store`，端点受面板 api_key 保护。
package panel

import (
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

// exportAccountMeta 多账号导出时的一条记录：在前面的规范字段之外附加分组与积分。
// 这两个字段是为了让「打包脚本」能还原 `<账号名>-<积分>.json` 的文件名与分组目录
// ——凭证格式本身不携带积分，只在文件名里体现。导入端会忽略未知字段。
type exportAccountMeta struct {
	exportAccount
	Group   string `json:"group,omitempty"`
	Credits int64  `json:"credits"`
}

// exportAccounts 导出账号（单个 .json 文件）。
func (p *Panel) exportAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	creditField := q.Get("credit")
	if creditField != "credits_total" {
		creditField = "credits" // 缺省 = 当前可用积分（与脚本默认一致）
	}

	mode := strings.TrimSpace(q.Get("mode"))
	if mode == "" {
		mode = "combined"
	}
	if mode != "combined" && mode != "split" {
		writeErr(w, http.StatusBadRequest, `mode 只支持 "combined" 或 "split"`)
		return
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

	recs, metas, skipped, warns := p.buildExportRecords(targets, creditField)
	if len(recs) == 0 {
		writeErr(w, http.StatusBadRequest, "没有可导出的账号："+strings.Join(skipped, "；"))
		return
	}

	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Export-Total", strconv.Itoa(len(targets)))
	w.Header().Set("X-Export-Count", strconv.Itoa(len(recs)))
	w.Header().Set("X-Export-Skipped", strconv.Itoa(len(skipped)))
	w.Header().Set("X-Export-Warnings", strconv.Itoa(len(warns)))
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	if mode == "split" {
		p.writeExportSplit(w, recs, metas, len(targets), len(skipped), len(warns))
		return
	}

	if len(recs) == 1 {
		// 单账号：文件名 <账号名>-<积分>.json，内容与「账号」目录里的样例完全一致
		// （不附加 group/credits 元数据）。
		name := exportFileName(recs[0], metas[0].Credits)
		w.Header().Set("Content-Disposition", contentDisposition(name))
		data, err := json.MarshalIndent(recs, "", "  ")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "序列化失败: "+err.Error())
			return
		}
		_, _ = w.Write(append(data, '\n'))
		return
	}

	// 多账号：一个 JSON 数组（每条带 group/credits 元数据，供打包脚本还原文件名）。
	w.Header().Set("Content-Disposition",
		contentDisposition("账号导出-"+time.Now().Format("20060102-150405")+".json"))
	data, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化失败: "+err.Error())
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

// exportFileName 单账号文件名：<账号名>-<积分>.json（非法字符已替换）。
func exportFileName(rec exportAccount, credits int64) string {
	return safeExportName(rec.Nickname+"-"+strconv.FormatInt(credits, 10), shortUID(rec.UID)) + ".json"
}

// exportSplitFile mode=split 清单中的一项。content 只含规范 10 字段；
// group/credits 放在清单项上，供前端/本地脚本还原文件名与分组目录。
type exportSplitFile struct {
	Name    string        `json:"name"`
	Group   string        `json:"group,omitempty"`
	Credits int64         `json:"credits"`
	Content exportAccount `json:"content"`
}

// exportSplitManifest mode=split 的响应体。前端按 files[] 逐个下载。
type exportSplitManifest struct {
	OK       bool              `json:"ok"`
	Mode     string            `json:"mode"`
	Total    int               `json:"total"`
	Count    int               `json:"count"`
	Skipped  int               `json:"skipped"`
	Warnings int               `json:"warnings"`
	Files    []exportSplitFile `json:"files"`
}

// writeExportSplit 输出逐账号文件清单。不设置 Content-Disposition：清单本身不是
// 下载文件，避免浏览器把清单存成文件；X-Export-* 头已由 exportAccounts 设置。
func (p *Panel) writeExportSplit(w http.ResponseWriter, recs []exportAccount, metas []exportAccountMeta, total, skipped, warnings int) {
	files := make([]exportSplitFile, 0, len(recs))
	for i, rec := range recs {
		files = append(files, exportSplitFile{
			Name:    exportFileName(rec, metas[i].Credits),
			Group:   metas[i].Group,
			Credits: metas[i].Credits,
			Content: rec,
		})
	}
	data, err := json.MarshalIndent(exportSplitManifest{
		OK:       true,
		Mode:     "split",
		Total:    total,
		Count:    len(files),
		Skipped:  skipped,
		Warnings: warnings,
		Files:    files,
	}, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "序列化失败: "+err.Error())
		return
	}
	_, _ = w.Write(append(data, '\n'))
}

// buildExportRecords 按目标账号生成导出记录。recs 与 metas 一一对应
// （metas[i] 内嵌 recs[i] 的全部字段）。skipped/warns 用于回报原因。
func (p *Panel) buildExportRecords(targets []pool.Status, creditField string) (recs []exportAccount, metas []exportAccountMeta, skipped, warns []string) {
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
		recs = append(recs, rec)
		metas = append(metas, exportAccountMeta{
			exportAccount: rec,
			Group:         strings.TrimSpace(st.Group),
			Credits:       credits,
		})
	}
	return recs, metas, skipped, warns
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
	return `attachment; filename="export.json"; filename*=UTF-8''` + url.PathEscape(name)
}
