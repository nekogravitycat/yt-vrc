# 專案現況與交接

**更新於**：2026-08-22
**用途**：讓新的工作階段快速上手。閱讀順序建議為本文件 → `implementation.md` → `spec.md`。

---

## 1. 文件的權威順序

| 文件 | 角色 |
|---|---|
| `spec.md` | 原始需求與設計。**部分內容已被實測推翻**，見下 |
| `implementation.md` | 實測數據與所有偏離 spec 的決策。**與 spec 衝突時以此為準** |
| `deployment.md` | 對外暴露與 VRChat 實測步驟 |
| `handoff.md`（本文件） | 現況總覽 |

---

## 2. 目前完成度

| 里程碑 | 狀態 | 備註 |
|---|---|---|
| M1 播放路徑 | **完成**（本機驗收） | VRChat 內實測尚未進行 |
| M2 訊息影片 | **完成** | 顯示文字為英文 |
| M3 上線閘門 | 未開始 | 下一個工作項目 |
| M4 熱更新與健康度 | 未開始 | |
| M5 韌性與快取 | **部分完成** | singleflight 已提前實作；LRU 淘汰、長影片滑動視窗、client fallback 鏈未做 |
| M6 MP4 與預熱 | 未開始 | 訊息影片已支援 MP4，但影片本體的 MP4 packager 未做 |

程式碼規模：21 個 Go 檔、約 3,000 行。測試：`internal/adapter/httpapi`、
`internal/usecase/playvideo`，`-race` 下全過。

---

## 3. 三個推翻 spec 的實測發現

這是本專案最重要的背景知識，**動手改 HLS 相關程式碼前務必理解**。

### 3.1 googlevideo 對單一長連線 GET 限速（62 倍差距）

| 取得方式 | 速率 |
|---|---|
| 單一 GET（ffmpeg 的預設行為） | 312 KB/s |
| 4MB ranged 分塊 | 19.8 MB/s |

**絕不可把 googlevideo URL 直接交給 ffmpeg。** 必須經過
`internal/infra/fetch` 的並行分塊下載器。`FETCH_WORKERS` 預設 8。

### 3.2 關鍵影格不規則，segment 長度無法預先計算

實測 segment 長度介於 2.0–11.5 秒（標稱 6 秒）。因此 spec §4.2.1
「由 duration 算出 segment 數、預先生成完整 playlist」**會產生錯誤的時間軸**。
現行做法是**完整封裝後才交付**帶有真實 `EXTINF` 的 playlist。

連帶使 spec §6.4.2（阻塞等待）、§3.3（503 行為）、§6.4.3（中途續接）
**都不需要實作**。

### 3.3 YouTube 對「同一支影片」的重複解析做速率限制

觸發後回應 `Sign in to confirm you're not a bot`，但**僅影響該影片**，
同 IP 的其他影片正常，等待即可恢復，不需要 cookies。

這使 singleflight 從效能最佳化變成**防封鎖的必要機制**，因此已提前自 M5 實作。

---

## 4. 效能實測（開發機，HiNet 300Mbps）

| 影片 | 冷啟動 | 快取命中 |
|---|---|---|
| 4:56 | 2.8 s | 1.1 ms |
| 75:27（268 MB） | 8.1 s | 1.1 ms |

長影片的階段拆解：resolve 1.7 s、下載 18.6 s（4 workers）、remux 1.8 s。
**下載是唯一瓶頸**，提高 `FETCH_WORKERS` 至 8 後總時間降至 8.1 s。

---

## 5. 如何執行與驗證

### 5.1 開發

```powershell
$env:DATA_DIR = ".\data"
go run .\cmd\yt-vrc          # 預設 :8080
```

### 5.2 自動化驗收（45 項）

```powershell
.\scripts\verify.ps1                      # 預設影片
.\scripts\verify.ps1 -VideoId <ID>        # 該影片被限流時換一支
```

**注意**：失敗的影片請求同樣以 200 結束（錯誤是可播放的訊息影片），
所以不能只看狀態碼。腳本比對最終 URL 是否落在 `/m/` 之下來區分。

### 5.3 環境陷阱

- **ffmpeg 在 winget 的 shim 目錄**：`%LOCALAPPDATA%\Microsoft\WinGet\Links`。
  若 `ffmpeg: command not found`，是 PATH 未更新，重開終端機或手動加入
- **Windows 上重建二進位前必須先結束執行中的行程**，否則 `go build` 會靜默
  失敗而留下舊檔（曾因此誤判行為）
- **不要用 `-o $null`** 丟棄 curl 輸出，Windows 上要用 `-o NUL`

---

## 6. 對外暴露現況

`https://v.gravity.tw` **已上線**，經 Cloudflare Tunnel。

- 重用既有的 `Dorm Windows` 通道（`7a045f06-6432-4e7b-82d8-1772c9203b73`），
  以 Windows 服務常駐，`--token` 模式
- **這是儀表板管理的通道**：ingress 規則在 Cloudflare 儀表板，本機
  `config.yml` 完全無效。要改主機名稱對應只能到儀表板
- 已驗證：TLS 有效、`/h` 與 `/s` 回傳可播放媒體、影片播放正常
  （h264 1080p + aac）、Range 回應 206

### 6.1 Cloudflare 目前**沒有**快取 segment

實測 `cf-cache-status: DYNAMIC`。原因是 `.ts` 與 `.m3u8` 不在 Cloudflare
預設的可快取副檔名清單中。服務端已送出
`Cache-Control: public, max-age=31536000, immutable`，但需要額外建立
**Cache Rule（Cache Everything）** 才會生效。

目前狀態代表**所有流量都由來源機器承載**。以 5 人規模無妨。若要啟用邊緣
快取需自行評估——注意 Cloudflare 服務條款 §2.8 限制非 Enterprise 方案以
CDN 大量提供影片，啟用快取正是踩在該條款上。

---

## 7. 尚未驗證的事項

### 7.1 VRChat 內實測（M1 的正式驗收條件）

**完全尚未進行。** 檢查表在 `deployment.md` §6，共 10 項。特別需要回報：

1. **seek 是否正確** —— 若異常，會推翻 §3.2 的設計判斷
2. **訊息影片文字在 VR 中是否夠大** —— 目前字級為畫面高度的 1/19，
   要調整改 `internal/infra/render/png.go` 的 `bodySize`
3. **VRChat 的影片載入逾時上限** —— 決定 `MAX_DURATION` 的合理值
4. **AVPro 是否接受 15 秒的訊息影片**

### 7.2 實驗室網路的解析成功率

spec §3.1 列為部署前的**必要驗收條件**，尚未執行。目前所有測試都在家中
HiNet IP 完成。

---

## 8. 建議的下一步

依序：

1. **VRChat 實測**（阻塞其他決策；§7.1 的四個問題會影響後續參數）
2. **M3 上線閘門** —— Discord 憑證尚未取得，因此先做 manual override
   （`/on`、`/off`）與環境變數驅動的 fake signal，Discord 實作照介面寫好待測
3. **`MAX_CONCURRENT_JOBS` 併發上限**（spec §8）—— 程式碼很小，同屬防封鎖
4. **M5 剩餘部分** —— LRU 淘汰、長影片滑動視窗、client fallback 鏈
5. **Dockerfile** —— 需含新版 deno（yt-dlp 解 `n` 參數挑戰用；目前分塊下載
   已不依賴它，但缺少會使部分格式無法取得）

---

## 9. 架構速覽

```
cmd/yt-vrc/main.go              組裝與啟動（唯一連接具體實作之處）
internal/
  domain/                       無外部相依
    video/                      ID、OutputSpec、CacheKey、MediaAsset、領域錯誤
    message/                    View 與內容雜湊
    port/                       Resolver / MediaFetcher / Packager / AssetStore
  usecase/playvideo/            解析→下載→封裝，含雙層 singleflight
  adapter/
    httpapi/                    路徑解析（spec §4.1.4）、HTTP handler
    presenter/                  領域結果 → View
  infra/
    ytdlp/                      Resolver 實作（--dump-single-json）
    fetch/                      並行分塊下載器 ★ spec 中沒有，但為必要
    ffmpeg/                     HLS packager、訊息影片 renderer
    render/                     PNG 版面（嵌入 Noto Sans TC）
    store/                      檔案系統 AssetStore
    config/                     環境變數設定
```

**依賴方向嚴格由外向內**，domain 層不 import 任何外部套件。

### 9.1 需要知道的實作細節

- **`-bsf:a aac_adtstoasc` 不可省略** —— MP4 容器中的 AAC 沒有 ADTS header，
  直接複製進 MPEG-TS 會產生無法播放的音軌
- **`net/http` 會把 `//` 正規化為 `/`** —— 貼上的 `https://youtu.be/x` 會變成
  `https:/youtu.be/x`，已由 `restoreSchemeSlash` 還原
- **`watch?v=` 的查詢字串會被當成伺服器自己的 query** —— handler 需重新接回
  `RawQuery` 才能解析
- **解析層 singleflight 的鍵含畫質** —— 與 spec §4.7.3 不同。因為格式選擇器
  由畫質推導，僅以 `video_id` 為鍵會讓 720p 請求拿到 1080p 軌道
- **共用工作使用 `context.WithoutCancel`** —— 先送請求者放棄時不得中斷其他
  仍在等待的觀看者
