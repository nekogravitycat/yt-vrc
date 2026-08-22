# 專案現況與交接

**更新於**：2026-08-22（M4 程式碼完成）
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
| M1 播放路徑 | **完成，VRChat 驗收通過** | 播放與 seek 均已實測（implementation.md §11.5） |
| M2 訊息影片 | **完成，VRChat 驗收通過** | 顯示文字為英文 |
| M3 上線閘門 | **完成，待 VRChat 實測** | Discord 實作未經真實憑證驗證（implementation.md §12.4） |
| M4 熱更新與健康度 | **程式碼完成，完全未經驗證** | 見 §2.1——沒有單元測試、驗收腳本未涵蓋、從未跑過一次真實升級 |
| M5 韌性與快取 | **完成**（滑動視窗刻意廢除） | singleflight、`MAX_CONCURRENT_JOBS`、LRU、client fallback、`/l` `/e` `/p` `/d` `/i` 全數完成；廢除理由見 implementation.md §14.2 |
| M6 MP4 與預熱 | 未開始 | 訊息影片已支援 MP4，但影片本體的 MP4 packager 未做 |

另已修正 implementation.md §11.3 的訊息影片網址問題（穩定 slot 路徑，見 §13）。

程式碼規模：48 個 Go 檔、約 7,700 行。測試：`internal/adapter/httpapi`、
`internal/domain/availability`、`internal/infra/store`、`internal/usecase/playvideo`，
`-race` 下全過。`scripts/verify.ps1` 共 72 項，全過（**尚未涵蓋 M4**）。

### 2.1 M4 的驗證缺口（動手前先讀）

M4 的程式碼通過 `go build`、`go vet`、Linux 交叉編譯與現有全部測試，
但**沒有任何一行 M4 的行為被驗證過**：

1. **新程式碼零單元測試** —— `internal/domain/health`、`internal/infra/ytdlp`
   （manager／marker／install／smoketest）、`internal/usecase/upgrade`、
   `internal/usecase/healthcheck` 全部沒有測試檔
2. **`ytdlp.Manager` 目前不可測** —— 它會真的打 GitHub、真的執行下載回來的
   二進位檔。要測必須先加三個接縫：`Version` 回呼（取代 `binaryVersion`）、
   `APIBase`、`DownloadBase`
3. **從未執行過真實升級** —— spec §12 的 M4 驗收條件
   「容器不重啟的情況下完成 yt-dlp 版本升級與回滾」**未達成**
4. **`scripts/verify.ps1` 未加入 M4 檢查**

換句話說：M4 目前的狀態是「寫完了」，不是「能用」。

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
# 閘門是 fail-closed。沒有訊號來源時所有影片端點都會回「服務離線」，
# 開發時用假訊號打開（或啟動後在瀏覽器／VRChat 輸入 /on）。
$env:FAKE_SIGNAL_ONLINE = "true"
go run .\cmd\yt-vrc          # 預設 :8080
```

### 5.2 自動化驗收（72 項）

```powershell
.\scripts\verify.ps1                      # 預設影片
.\scripts\verify.ps1 -VideoId <ID>        # 該影片被限流時換一支
```

**注意**：失敗的影片請求同樣以 200 結束（錯誤是可播放的訊息影片），
所以不能只看狀態碼。腳本比對 playlist 中的 segment 路徑是否落在 `/m/`
之下來區分「真的播出來」與「播出一則錯誤訊息」。

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

### 7.1 VRChat 內實測

**M1 與 M2 已驗收通過**（implementation.md §11.5）。影片播放、seek、
訊息影片皆正常，AVPro 亦接受 15 秒的訊息影片。

尚未取得答案的項目：

1. **訊息影片文字在 VR 中是否夠大** —— 目前字級為畫面高度的 1/19，
   要調整改 `internal/infra/render/png.go` 的 `bodySize`
2. **VRChat 的影片載入逾時上限** —— 決定 `MAX_DURATION` 的合理值。
   需要一支冷啟動超過 10 秒的影片才能測
3. **長影片（60 分鐘以上）的冷啟動是否可接受**

### 7.2 M3 的 VRChat 實測

閘門的所有行為都只在瀏覽器／curl 驗證過（`scripts/verify.ps1` 72 項全過），
尚未在 VRChat 內確認：

1. 灰色的「Service Offline」訊息影片在 AVPro 中是否正常播放
2. `/on`、`/off`、`/e`、`/p`、`/i`、`/d` 的訊息影片
3. Discord Presence 訊號（憑證未取得，程式碼從未執行過）

### 7.3 實驗室網路的解析成功率

spec §3.1 列為部署前的**必要驗收條件**，尚未執行。目前所有測試都在家中
HiNet IP 完成。

---

## 8. 建議的下一步

依序：

1. **補齊 M4 的測試與驗證**（見 §2.1）—— 這是目前最大的風險：熱更新是唯一
   會**替換掉正在服務的執行檔**的功能，而它一次都沒跑過。先加測試接縫與
   單元測試，再以 `YTDLP_MODE=managed` 實跑一次 `/u` 與 `/u/back`
2. **取得 Discord Bot 憑證並實測 M3** —— `internal/infra/signal/discord.go`
   已照介面寫好但**從未跑過**。需要 Developer Portal 啟用 Presence Intent，
   且 Bot 與被監測使用者同在一個 guild
3. **VRChat 內實測閘門與新端點** —— `/on`、`/off`、`/e`、`/p`、`/i`、`/d`、
   `/u` 的訊息影片都只在瀏覽器驗證過
4. **Dockerfile** —— 需含 `python3`（yt-dlp zipapp 的執行環境）與新版 deno
   （解 `n` 參數挑戰用；分塊下載已不依賴它，但缺少會使部分格式無法取得），
   並設 `YTDLP_MODE=managed`。**不要用 `yt-dlp_linux`**：它連結 glibc，
   在 Alpine（musl）上起不來
5. **M6 MP4 路徑與預熱** —— `/w`、`/r`，以及影片本體的 MP4 packager

---

## 9. 架構速覽

```
cmd/yt-vrc/main.go              組裝與啟動（唯一連接具體實作之處）
internal/
  domain/                       無外部相依
    video/                      ID、OutputSpec、CacheKey、MediaAsset、領域錯誤
    availability/               Signal 介面 ★ 核心解耦點、Gate 聚合與去抖動
    message/                    View 與內容雜湊
    event/                      /e 與 /s 讀的事件型別
    health/                     滾動解析視窗與 spec §4.6 的門檻評分
    port/                       Resolver / MediaFetcher / Packager / AssetStore
                                / ToolchainManager / ToolchainVerifier
  usecase/
    playvideo/                  解析→下載→封裝，含雙層 singleflight 與併發上限
    upgrade/                    yt-dlp 熱更新：背景執行、維護模式、排空
    healthcheck/                主動探測（輪流一次一支影片）
  adapter/
    httpapi/                    路徑解析（spec §4.1.4）、HTTP handler、訊息 slot 表
    presenter/                  領域結果 → View
  infra/
    ytdlp/                      Resolver（--dump-single-json）、版本化目錄與
                                原子切換、煙霧測試、非受管模式
    diskfree/                   磁碟可用空間（Windows／Unix 兩份實作）
    fetch/                      並行分塊下載器 ★ spec 中沒有，但為必要
    ffmpeg/                     HLS packager、訊息影片 renderer
    render/                     PNG 版面（嵌入 Noto Sans TC）
    signal/                     Discord Presence ★（未實測）、開發用 fake
    state/                      覆寫與事件記錄的持久化
    store/                      檔案系統 AssetStore，含 LRU 淘汰
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
- **閘門是 fail-closed** —— 啟用（預設）且無訊號來源時，所有影片端點回
  「服務離線」，只有 `/on` 能打開。命令端點永不受閘門限制
- **`/on` 的覆寫寫入 `/data/state/override.json`** —— 沒有持久化的話，重啟
  會靜默地讓服務整個停擺
- **命令端點的媒體走穩定 slot 路徑** —— `/m/status_hls/…`，內容雜湊只用於
  磁碟去重。被 slot 指到的產物不可被淘汰（見 implementation.md §13）
- **Resolver 不持有固定的 yt-dlp 路徑** —— 改用 `Locate` 回呼每次重新解析
  current 指標，否則熱更新要等重啟才生效（implementation.md §16.9）
- **`/u` 是背景執行** —— 立即回應，再次輸入看進度，完成後 90 秒內顯示結果。
  阻塞版本必須同時活過 AVPro 與 Cloudflare 兩個未知／已知的逾時
  （implementation.md §16.2）
- **主動探測每次只跑一支影片** —— 跑完整份清單會逼近 §3.3 的每支影片速率
  限制（implementation.md §16.4）
- **維護模式在閘門之前檢查** —— 更新中的不可用是可以等的；回「服務離線」
  會把使用者導向 `/on` 去修一個沒壞的東西

### 9.2 AVPro 的硬性要求（實測，詳見 implementation.md §10、§11）

- **AVPro 在 PC 上就是 Windows Media Foundation**（UA 為 `NSPlayer/WMFSDK`）
- **VRChat 會先用自己的解析器跑過網址**，再交給 AVPro。輸出必須同時讓
  yt-dlp 與 WMF 都能理解
- **不可用轉址交付 playlist** —— AVPro 不跟隨轉址，會把 302 的 HTML 主體
  當成無效格式
- **必須產生 master playlist**（含 `EXT-X-STREAM-INF` 與 `CODECS`），否則
  yt-dlp 解析出的 vcodec/acodec 為 None
- **HLS 版本用 3** —— 不要加 `-hls_flags independent_segments`（會升到 v6）
- **WMF 對每個 segment 都送 `Range: bytes=0-`** —— Range 支援是必要條件
- **「Media cannot be played, maybe due to invalid format」是通用載入失敗
  訊息**，最常見的真因是 404，不是編碼問題。先確認產物是否存在
