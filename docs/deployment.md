# 部署與 VRChat 實測

本文件說明如何把 yt-vrc 暴露到 `v.gravity.tw` 並在 VRChat 中完成 M1 驗收。

---

## 1. 為什麼需要對外暴露

AVPro（VRChat 的播放器核心）**對 TLS 憑證驗證嚴格，自簽憑證必定失敗**
（spec §9.2）。因此無法以 `localhost` 或自簽憑證完成實測，必須有一個
受信任憑證的公開網址。

---

## 2. 兩種路徑

| 方案 | Cloudflare Tunnel | DNS-only + Caddy |
|---|---|---|
| 連接埠轉發 | **不需要** | 需要（80/443） |
| TLS 憑證 | Cloudflare 自動提供 | Let's Encrypt，Caddy 自動處理 |
| 來源 IP | 隱藏 | 暴露 |
| 媒體流量路徑 | 經 Cloudflare 網路 | 直連 |
| 適合 | 實驗室網路、快速測試 | 長期自主運行 |

### 2.1 需要知道的取捨

**Cloudflare 服務條款 §2.8** 限制在非 Enterprise 方案上以 CDN 大量提供影片
等非 HTML 內容（Cloudflare Stream 除外）。本專案的性質正是影片傳遞。以本專案
的規模（5 人以內、間歇使用）實際被稽核的可能性低，但這是條款的字面規定，
**採用與否是你的決定**。若要完全避開，用 §4 的 DNS-only 方案，Cloudflare 僅
負責 DNS，媒體流量不經過其網路。

**Cloudflare 免費方案有 100 秒的來源逾時（錯誤 524）**。實測冷啟動：75 分鐘
影片 8.1 秒，3 小時影片推估約 20 秒，都在範圍內。但超長影片仍可能觸及上限，
建議設定 `MAX_DURATION` 明確拒絕。

---

## 3. Cloudflare Tunnel（目前採用）

**本專案重用既有的 `Dorm Windows` 通道**（`7a045f06-6432-4e7b-82d8-1772c9203b73`），
不另建通道。該通道以 Windows 服務形式常駐於本機（GravityPC）：

```
"C:\Program Files (x86)\cloudflared\cloudflared.exe" tunnel run --token <token>
```

### 3.1 這是「儀表板管理」的通道

以 `--token` 執行的通道，其 **ingress 規則儲存在 Cloudflare 儀表板，而非本機
`config.yml`**。在本機寫任何 `config.yml` 都不會生效。因此新增主機名稱必須於
儀表板操作，無法以 CLI 完成。

### 3.2 已完成的部分（CLI）

`v.gravity.tw` 的 CNAME 已指向 Dorm Windows 通道：

```powershell
cloudflared tunnel route dns --overwrite-dns 7a045f06-6432-4e7b-82d8-1772c9203b73 v.gravity.tw
```

### 3.3 待完成的部分（儀表板，需手動）

Cloudflare Dashboard → **Zero Trust** → Networks → Tunnels →
**Dorm Windows** → Public Hostname → **Add a public hostname**：

| 欄位 | 值 |
|---|---|
| Subdomain | `v` |
| Domain | `gravity.tw` |
| Type | `HTTP` |
| URL | `localhost:8080` |

儲存後即時生效，無需重啟服務。

### 3.4 啟動服務本體

通道由 Windows 服務常駐，因此只需啟動 yt-vrc：

```powershell
cd C:\Users\gravity\Documents\Repositories\gravity\yt-vrc
$env:DATA_DIR = ".\data"
$env:LISTEN_ADDR = ":8080"
go run .\cmd\yt-vrc
```

### 3.5 確認

```powershell
curl.exe -sI https://v.gravity.tw/h
curl.exe -sL -o NUL -w "%{http_code}`n" https://v.gravity.tw/h
```

預期分別為 302 與 200。若回傳 Cloudflare 錯誤 1033 或 404，表示 §3.3 的主機
名稱尚未加入。

---

## 4. 替代方案：DNS-only + Caddy

若不想讓媒體流量經過 Cloudflare：

1. Cloudflare DNS 中將 `v.gravity.tw` 的 A 記錄指向你的固定 IP，
   **代理狀態設為 DNS only（灰雲）**
2. 路由器將 80/443 轉發至該機器
3. `Caddyfile`：

```
v.gravity.tw {
	reverse_proxy localhost:8080 {
		# 不緩衝，維持 HLS 即時性；放寬逾時以容納冷啟動
		flush_interval -1
		transport http {
			read_timeout 300s
		}
	}
}
```

Caddy 會自動申請並續期 Let's Encrypt 憑證。

---

## 5. Cloudflare 設定注意事項

若採用 Tunnel（流量經 Cloudflare 代理）：

| 項目 | 建議 | 理由 |
|---|---|---|
| SSL/TLS 模式 | Full | Tunnel 已加密至來源 |
| Caching Level | Standard | 產物不可變，快取有益 |
| Always Online | 關閉 | 可能提供過期的播放清單 |
| Rocket Loader / Minify | 無所謂 | 本服務不提供 HTML/JS |

**快取行為**：本服務已設定適當的 `Cache-Control`——媒體檔案為
`immutable, max-age=31536000`（產物只在封裝完成後才發布，位址包含
影片 ID、畫質與容器，內容永不改變），命令端點的導向為 `no-store`
（哪一支訊息影片會隨服務狀態改變）。

**但實測顯示 Cloudflare 目前並未快取 segment**（`cf-cache-status: DYNAMIC`）。
原因是 `.ts` 與 `.m3u8` 不在 Cloudflare 預設的可快取副檔名清單中，服務端
送出的 `Cache-Control` 不足以改變此行為，需另外建立 **Cache Rule
（Cache Everything）**。

因此目前**所有流量均由來源機器承載**。以 5 人規模無妨；若要啟用邊緣快取，
請一併考量 §2.1 的服務條款問題——啟用影片快取正是該條款針對的行為。

---

## 6. VRChat 實測檢查表

在有影片播放器的世界中依序輸入，並記錄結果：

### 6.1 M1 驗收（spec §12）

| # | 輸入 | 預期 | 結果 |
|---|---|---|---|
| 1 | `v.gravity.tw/dQw4w9WgXcQ` | 1080p 正常播放 | |
| 2 | 同上，拖動進度條至中段 | seek 正確、畫面對應 | |
| 3 | 同上，拖動至末段 | 可跳轉、無異常 | |
| 4 | `v.gravity.tw/dQw4w9WgXcQ/720` | 播放且畫質較低 | |
| 5 | 一支 60 分鐘以上的影片 | 冷啟動 10 秒內開始播放 | |

### 6.2 M2 驗收

| # | 輸入 | 預期 | 結果 |
|---|---|---|---|
| 6 | `v.gravity.tw/s` | 藍色狀態畫面，文字清晰可讀 | |
| 7 | `v.gravity.tw/h` | 說明畫面 | |
| 8 | `v.gravity.tw/aaaaaaaaaaa` | 紅色「Video Unavailable」 | |
| 9 | `v.gravity.tw/notacommand` | 紅色「Unrecognised Command」 | |

### 6.3 併發去重（spec §12 M5 驗收）

| # | 操作 | 預期 | 結果 |
|---|---|---|---|
| 10 | 找朋友同時在同一 instance 貼同一個新影片網址 | 都能播放；伺服器記錄中 `"msg":"resolved"` 只出現一次 | |

檢查方式：於服務端終端機觀察輸出，或
`Select-String -Path <log> -Pattern '"msg":"resolved"'`。

### 6.4 需要一併記錄的觀察（spec §13.1 待驗證項目）

- **訊息影片的文字在 VR 中夠不夠大？** 目前字級為畫面高度的 1/19
  （spec §4.3.3 要求不小於 1/20）。若不易閱讀，調整
  `internal/infra/render/png.go` 的 `bodySize`
- **VRChat 的影片載入逾時上限為何？** 找一支長到冷啟動超過 10 秒的影片，
  觀察播放器在放棄前等待多久。此數值決定 `MAX_DURATION` 的合理設定
- **AVPro 是否接受 15 秒的訊息影片？** spec §13.1 第 3 項推測 15 秒為保守值

---

## 7. 疑難排解

| 症狀 | 可能原因 |
|---|---|
| 播放器完全無反應 | 憑證問題。以瀏覽器開啟同一網址確認憑證受信任 |
| 錯誤 524 | 冷啟動超過 Cloudflare 的 100 秒上限。改用較短的影片，或採 §4 方案 |
| 播放到一半停止 | 檢查服務端記錄是否有 ffmpeg 或下載錯誤 |
| 顯示「Blocked by YouTube」 | 該影片被速率限制，換一支或稍後再試（implementation.md §8.2） |
| seek 後畫面錯亂 | 記錄下來——這會推翻 implementation.md §3 的設計假設 |
