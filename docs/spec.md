# VRC Video Proxy 需求與設計文件

**專案代號**：`yt-vrc`
**服務網域**：`v.gravity.tw`
**文件版本**：v1.0
**撰寫日期**：2026-08-22
**目標讀者**：本專案唯一開發者與維運者
**Github**：`https://github.com/nekogravitycat/yt-vrc`

---

## 1. 專案概述

### 1.1 一句話定位

一個自架的 HTTP 服務，把 YouTube 影片即時重新封裝成 VRChat 播放器能直接播放的串流，並以「在遊戲內輸入網址」作為唯一操作介面。

### 1.2 核心價值主張

本專案的價值**不是**「我的 yt-dlp 比 VRChat 內建的新」。這個描述不精確，而且會誤導設計方向。

真正的價值是：**伺服器端具備 mux（多工封裝）能力**。

Phase 0 的實測顯示，YouTube 已經不再對一般 client 提供影音合一（progressive）的格式，只剩下影像軌與音訊軌分離的 DASH 格式。VRChat 的播放器建立在 AVPro 之上，而 AVPro 只接受**單一 URL**，客戶端沒有任何機會把兩條軌合併。這是架構層級的限制，不是版本新舊的問題——VRChat 就算把 yt-dlp 更新到最新版，也無法解決。

因此本服務的不可取代性在於：它站在一個能同時抓取兩條軌、用 ffmpeg 合併、再以單一 URL 交付的位置上。

### 1.3 使用情境

- 使用者：專案作者本人與少數朋友（估計 5 人以內）
- 場景：在 VRChat 世界中的影片播放器裡貼上網址一起看影片
- 頻率：間歇性，僅在作者本人遊玩 VRChat 期間
- 規模：同時併發觀看者上限預估 10 人，同時處理的不同影片上限預估 3 支

### 1.4 明確的非目標

以下項目**刻意不做**，列出以避免範圍蔓延：

- 不對外公開、不做使用者註冊、不做多租戶
- 不做轉碼（transcoding），只做重新封裝（remux）
- 不支援 YouTube 以外的來源（首版）
- 不支援直播與正在進行的首播
- 不做網頁管理介面（操作介面就是 VRChat 內的影片播放器）
- 不追求高可用；單機、單一實例、可接受計畫性停機

---

## 2. 已驗證前提（Phase 0 結論）

以下資料於 2026-08-22 實測取得，是本設計的事實基礎。

### 2.1 網路環境

| 項目 | 實測值 | 意義 |
|---|---|---|
| 對外 IP | 220.135.209.102（中華電信固定 IP） | 住宅級固定 IP |
| NAT 層數 | 單層，非 CGNAT | 可直接開放對外連接埠 |
| 上行頻寬 | 303.6 Mbps | 頻寬完全不是限制因素 |

### 2.2 yt-dlp 解析能力

以 15 支從 VRChat log 實際撈出的影片測試，yt-dlp 2026.08.19：

| 項目 | 結果 |
|---|---|
| 解析成功率 | 15/15（100%） |
| 是否需要 cookies | 否 |
| 是否需要 `--extractor-args` | 否（預設 client 即可） |
| 解析耗時 | 中位數 1.6 秒，最慢 2.3 秒 |
| SABR-only 格式 | 0 支出現 |
| **progressive（影音合一）格式** | **0 支存在** |
| 分離軌最高 avc1 畫質 | 15 支中 14 支有 1080p，1 支 720p |

### 2.3 替代 client 的驗證結果

| player_client | 結果 |
|---|---|
| 預設（tv） | 48 個可用分離軌格式，正常 |
| `web` | 僅剩 SABR 格式，無可用播放連結 |
| `ios` | 僅剩 storyboard 縮圖，無任何影音格式 |

### 2.4 由前提推導出的設計結論

1. **純 302 導向架構（L1）不可行**——沒有任何影音合一的 URL 可以導向。伺服器端 mux 是唯一路徑。
2. **不需要 cookies**——避免了帳號綁定與封號風險，這是重要的簡化。
3. **不需要固定 `player_client`**——但需要 fallback 鏈（見 §3.2）。
4. **1080p avc1 + mp4a 的取得率極高**——格式選擇器可以相當激進。

---

## 3. 未驗證假設與風險

本節列出設計所依賴、但尚未取得證據的假設。每一項都標註了驗證方式與應對策略。

### 3.1 【高風險】實驗室 Linux 的出口 IP 未經驗證

§2.2 的 100% 成功率是在**家中 HiNet 住宅 IP** 量測的。目標部署環境是學校實驗室的教育網路，YouTube 對教育網路 IP 的判定嚴格程度介於住宅與機房之間，**完全沒有資料**。

**驗證方式**：部署前必須在實驗室機器上重跑 `vrcproxy_poc.py resolve`，以相同的 15 支影片取得成功率。這是部署的**驗收條件**，不是選配。

**應對策略**：架構上提供可選的出口代理設定（`RESOLVER_PROXY`，支援 `socks5://` 與 `http://`）。若實驗室 IP 表現不佳，可將**解析流量**（僅 metadata，流量極小）繞經家中網路，而**媒體下載流量**仍走實驗室的高頻寬直連。這個切換不需要改動架構，只需改設定。

**次要風險**：使用學校資源運行違反 YouTube 服務條款的服務，若產生 abuse 通知，收件對象是學校網管而非本人。建議控制流量規模，並保留隨時遷移的能力（這也是 §7 要求所有狀態都可攜的原因之一）。

### 3.2 【中風險】SABR-only 是 session 級的開關，不是 client 級的屬性

yt-dlp 在 `ios` client 的警告訊息中明確指出，SABR-only 是針對 **current session** 啟用的實驗。這代表目前 100% 的成功率並非穩定屬性，而是「tv client 這條路目前尚未被納入實驗」的暫時狀態。失效時會是**一夜之間整批失敗**，而非逐步劣化。

**應對策略**：
- Client fallback 鏈設計為一等公民（可設定的有序清單），而非例外處理補丁
- 內建健康度自我監測（見 §4.6），成功率跌破門檻時主動改變 `/s` 端點的狀態顯示
- 保留未來接上 yt-dlp 原生 SABR 下載器的擴充點

### 3.3 【低風險】AVPro 對 HTTP 503 的行為未知

HLS 播放時若使用者 seek 到尚未 remux 完成的 segment，伺服器需要回應某種「稍後再試」。AVPro 收到 503 後會重試還是直接放棄，沒有實測資料。

**應對策略**：採用「阻塞等待優先、503 為最後手段」的策略（見 §6.4）。第一階段實作後應以實測確認。

### 3.4 【低風險】Discord Presence 的可靠性

Discord presence 僅在桌面用戶端執行、且使用者未關閉「將目前的活動顯示為狀態訊息」時才會回報。若使用者以其他方式啟動 VRChat（例如 Discord 未開啟），偵測會失效。

**應對策略**：訊號來源設計為可插拔介面（§6.3），並支援多來源 OR 邏輯。首版僅實作 Discord，但介面預留。另提供手動覆寫端點作為逃生門（`/on`、`/off`）。

---

## 4. 功能需求

### 4.1 URL 與端點規格

#### 4.1.1 設計原則

- **短**：在 VR 環境中輸入文字非常痛苦，路徑與參數盡可能精簡
- **雙形式**：所有命令端點同時支援簡寫與全稱
- **不衝突**：YouTube 影片 ID 固定為 11 字元 `[A-Za-z0-9_-]{11}`，因此任何長度不等於 11 的路徑片段都可安全地作為命令，不會與影片 ID 碰撞
- **可降級**：影片 URL 形式設計為「移除前綴即為原始 YouTube 連結」，服務停擺時使用者可自行退回原生播放

#### 4.1.2 影片播放端點

| 形式 | 範例 | 說明 |
|---|---|---|
| 影片 ID | `v.gravity.tw/NJ1tne9u8YM` | 最短形式，預設 HLS 輸出 |
| 完整 URL 前綴 | `v.gravity.tw/https://www.youtube.com/watch?v=NJ1tne9u8YM` | 方便從瀏覽器直接複製貼上 |
| 短網址前綴 | `v.gravity.tw/https://youtu.be/NJ1tne9u8YM` | 同上 |

**輸出格式與畫質的修飾語**，以副檔名與路徑片段表示：

| 形式 | 範例 | 說明 |
|---|---|---|
| 預設 | `/NJ1tne9u8YM` | HLS，畫質上限取自設定（預設 1080p） |
| 指定 MP4 | `/NJ1tne9u8YM.mp4` | progressive MP4，供 Unity VideoPlayer 使用 |
| 指定 HLS | `/NJ1tne9u8YM.m3u8` | 顯式指定，語意同預設 |
| 指定畫質 | `/NJ1tne9u8YM/720` | 畫質上限 720p，容器沿用預設 |
| 兩者並用 | `/NJ1tne9u8YM/720.mp4` | 720p MP4 |

畫質上限接受 `360` / `480` / `720` / `1080` / `1440` / `2160`，實際輸出取「不超過此上限的最高可用畫質」。上限值不寫死於程式碼，預設值由設定檔決定（見 §8）。

#### 4.1.3 命令端點

所有命令端點回傳**訊息影片**（見 §4.3），而非 JSON 或 HTML，因為唯一的使用介面是 VRChat 影片播放器。

命令端點同樣支援 `.mp4` 與 `.m3u8` 副檔名以指定容器；預設依 `DEFAULT_CONTAINER` 設定。

| 簡寫 | 全稱 | 功能 |
|---|---|---|
| `/s` | `/status` | 服務狀態總覽：yt-dlp 版本與版齡、健康度、上線閘門狀態、快取用量、近期成功率 |
| `/u` | `/upgrade` | 觸發 yt-dlp 熱更新（見 §4.5） |
| `/h` | `/help` | 端點速查表，列出所有可用命令 |
| `/l` | `/list` | 快取內容列表（最近 N 筆，含標題、畫質、大小、建立時間） |
| `/e` | `/errors` | 最近 N 筆錯誤摘要，含影片 ID 與失敗原因分類 |
| `/p` | `/purge` | 清空快取。需二次確認（見下方說明） |
| `/on` | `/enable` | 手動覆寫上線閘門為開啟，有效期限可設定（預設 4 小時） |
| `/off` | `/disable` | 解除手動覆寫，回歸自動偵測 |

**額外建議端點**（超出原始需求，但在實際使用中價值很高）：

| 簡寫 | 全稱 | 功能 | 理由 |
|---|---|---|---|
| `/w/{id}` | `/warm/{id}` | 預熱：立即開始準備指定影片但不等待，回傳「已排入佇列」訊息影片 | MP4 路徑冷啟動慢，開播前先預熱可消除等待 |
| `/r/{id}` | `/refresh/{id}` | 強制重新解析與重新封裝，繞過快取 | 影片產物損壞或畫質不如預期時的自救手段 |
| `/i/{id}` | `/info/{id}` | 顯示該影片的解析結果：標題、長度、可用格式、快取狀態、準備進度 | 排查「為什麼這支播不出來」的第一站 |
| `/d/{id}` | `/drop/{id}` | 從快取移除單支影片 | 比 `/p` 全清溫和 |

**`/p` 的二次確認機制**：由於 URL 輸入無法互動，採用「令牌確認」。首次呼叫 `/p` 回傳訊息影片，畫面上顯示一組 4 字元隨機令牌與有效期限（60 秒）；使用者需輸入 `/p/{token}` 完成清除。此機制同樣套用於任何未來新增的破壞性操作。

#### 4.1.4 端點解析優先序

請求路徑的解析依下列順序，先命中者勝出：

1. 路徑為 `/` → 回傳 `/h` 的內容
2. 第一個路徑片段長度為 11 且符合 `[A-Za-z0-9_-]{11}` → 視為影片 ID
3. 第一個路徑片段為已註冊的命令關鍵字 → 視為命令
4. 路徑以 `http://` 或 `https://` 開頭（去除前導斜線後）→ 視為完整 URL，抽取其中的影片 ID
5. 皆不符合 → 回傳「無法辨識的指令」訊息影片，並附上 `/h` 的提示

### 4.2 影片交付

#### 4.2.1 兩條輸出路徑

由於使用環境同時存在 AVPro（支援 HLS）與 Unity VideoPlayer（不支援 HLS），兩條路徑都必須實作。

**HLS 路徑（預設）**

- 產物：`master.m3u8` + 一系列 `.ts` segment
- 關鍵技巧：**playlist 於第一時間完整生成**。因為 yt-dlp 的 metadata 已提供影片總長度（`duration`），可立即算出 segment 數量並寫出含 `EXT-X-PLAYLIST-TYPE:VOD` 與 `EXT-X-ENDLIST` 的完整播放清單。播放器因此立刻取得完整時間軸，進度條與 seek 馬上可用，不需等待 remux 完成。
- Segment 長度：6 秒（可設定）
- 冷啟動延遲：僅需等待第一個 segment 就緒，實測預期 3–8 秒

**MP4 路徑**

- 產物：單一 `.mp4`，以 `-movflags +faststart` 將 moov atom 置於檔首
- 限制：**必須完整下載並封裝完成後才能交付**，否則無法提供正確的 `Content-Length`，播放器也無法 seek
- 冷啟動延遲：與影片長度、下載速度成正比。以片單中最長的 68 分鐘影片估算，1080p 約 700MB–1GB，可能需要數分鐘
- 冷啟動處理：見 §4.2.3

#### 4.2.2 格式選擇策略

yt-dlp 格式選擇器（`H` 為畫質上限）：

```
bv*[height<=H][vcodec^=avc1]+ba[acodec^=mp4a]/bv*[height<=H][vcodec^=avc1]+ba/b[height<=H]
```

設計理由：
- 強制 `avc1`（H.264）與 `mp4a`（AAC）—— 這是 AVPro 與 Unity VideoPlayer 在所有平台上都能硬體解碼的組合。VP9 與 AV1 在部分環境會失敗，Opus 在 MP4 容器中相容性差。
- 鎖定 avc1 也保證了 ffmpeg 可以用 `-c copy` 純重封裝，**不需要轉碼**。這讓處理速度遠快於實時播放，是整個架構的效能基礎。
- Fallback 鏈逐步放寬，最後一層接受任何格式。若最終選到非 avc1 的格式，系統應**標記為需要轉碼**並在訊息影片中警示——首版可選擇直接拒絕並回報，而非啟動昂貴的轉碼。

Phase 0 資料顯示 15 支中 14 支有 1080p avc1，此選擇器的命中率極高。

#### 4.2.3 MP4 冷啟動的處理

MP4 路徑無法邊下載邊交付，因此需要明確的等待語意：

1. 請求 `/{id}.mp4`，快取未命中
2. 伺服器立即啟動背景準備工作，並回傳一支**進度訊息影片**（15 秒循環），畫面顯示：影片標題、目前進度百分比、預估剩餘時間、以及「請於 N 秒後重新輸入相同網址」的指示
3. 使用者重新輸入網址；若已完成則播放影片，若仍在進行則再次回傳更新後的進度影片
4. 準備完成後，該影片進入快取，後續請求直接命中

此設計把 §4.3 的訊息影片系統從「錯誤回報工具」提升為**核心互動機制**。

#### 4.2.4 Range 請求支援

兩條路徑都必須正確支援 HTTP Range：

- 回應 `Accept-Ranges: bytes`
- 部分請求回應 `206 Partial Content` 與正確的 `Content-Range`
- 完整請求回應 `200` 與正確的 `Content-Length`

Go 的 `http.ServeContent` 已正確處理上述所有語意，應直接使用而非自行實作。

### 4.3 訊息影片子系統

#### 4.3.1 目的

將所有需要傳達給使用者的資訊——狀態、錯誤、進度、確認提示——算繪為可播放的影片，使操作者無需離開 VR 環境即可完成日常維運。

#### 4.3.2 算繪管線

```
結構化訊息資料 (MessageView)
        ↓
   版面配置與繪製（Go，產生 PNG）
        ↓
   ffmpeg 封裝（靜態影像 + 無聲音軌）
        ↓
   HLS 與 MP4 兩種產物
        ↓
   以內容雜湊為鍵快取
```

**繪製階段**使用 Go 的影像處理能力產生 PNG。必須嵌入支援繁體中文的字型（建議 Noto Sans TC 的子集），以 `embed` 打包進二進位檔，避免執行環境的字型相依。

**封裝階段**參考指令：

```bash
ffmpeg -loop 1 -i frame.png \
       -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=48000 \
       -t 15 -r 15 \
       -c:v libx264 -preset veryfast -tune stillimage -pix_fmt yuv420p \
       -c:a aac -b:a 64k -shortest \
       -movflags +faststart out.mp4
```

#### 4.3.3 關鍵設計約束

- **必須包含音軌**：部分播放器在遇到無音軌的影片時行為異常，因此即使內容無聲也要以 `anullsrc` 產生靜音 AAC 軌
- **長度 10–15 秒**：過短的影片可能觸發播放器的異常處理；此長度也足夠閱讀畫面內容
- **靜態畫面**：使用 `-tune stillimage`，檔案極小（預估 100KB 以內），產生速度極快
- **解析度 1280×720**：兼顧可讀性與檔案大小；在 VR 中的影片播放面板上，此解析度的文字清晰可辨
- **高對比配色**：VR 環境的顯示條件不佳，應使用深色背景配高亮度文字，字級不小於畫面高度的 1/20
- **內容雜湊快取**：相同內容的訊息影片不重複算繪。狀態類訊息因含即時數據，快取存活時間設為 10 秒

#### 4.3.4 訊息影片的分類

| 類別 | 觸發時機 | 視覺標示 |
|---|---|---|
| 狀態 | `/s` 等查詢命令 | 藍色標題列 |
| 進度 | MP4 準備中、預熱中 | 黃色標題列 + 進度條 |
| 成功 | `/u` 更新成功、`/p` 清除完成 | 綠色標題列 |
| 警示 | yt-dlp 版齡過久、成功率下降 | 橘色標題列 |
| 錯誤 | 解析失敗、格式不支援、影片不可用 | 紅色標題列 |
| 閘門關閉 | 上線偵測判定為離線 | 灰色標題列 |

### 4.4 上線閘門（Availability Gate）

#### 4.4.1 行為

服務僅在判定「作者正在遊玩 VRChat」時提供影片服務。閘門關閉時：

- 影片端點回傳「服務目前離線」的訊息影片（而非 HTTP 錯誤，以確保使用者看得到原因）
- `/s`、`/h`、`/on` 等管理端點**不受閘門限制**，始終可用
- 進行中的準備工作不中斷，讓其自然完成

#### 4.4.2 訊號來源介面

上線判定必須抽象為可插拔的介面，首版實作 Discord，但架構上支援任意數量的來源與組合邏輯。

**首版實作：Discord Presence**

- 註冊一個 Discord Bot（非 self-bot）
- 於 Developer Portal 啟用 **Presence Intent**（privileged intent；Bot 加入的伺服器少於 100 個時無需審核）
- 將 Bot 邀請至一個作者也在其中的 guild
- 透過 Gateway 訂閱 `PRESENCE_UPDATE` 事件，比對目標使用者 ID 的 activity 是否包含名稱為 `VRChat` 的項目
- Go 函式庫：`github.com/bwmarrin/discordgo`

**明確排除的做法**：使用個人帳號 token 連接 Gateway（self-bot）違反 Discord 服務條款且有封號風險，不予採用。

**已知限制**：需 Discord 桌面用戶端執行中，且未關閉活動狀態顯示。

**預留的其他來源**（介面已定義，實作延後）：

- 本機程序偵測（需部署於遊戲機或搭配回報 agent）
- VRChat 官方網站的線上狀態查詢
- 心跳回報端點（由遊戲機主動 POST）
- 手動覆寫（`/on` / `/off`，首版即實作，作為所有自動偵測的逃生門）

#### 4.4.3 組合邏輯與去抖動

- 多個來源之間預設採 **OR** 邏輯：任一來源回報上線即視為上線
- 手動覆寫具有最高優先權，且有明確的到期時間
- **去抖動**：偵測到離線後，延遲 `GATE_GRACE_PERIOD`（預設 10 分鐘）才真正關閉閘門。避免 Discord 短暫斷線或遊戲重啟造成服務中斷
- 閘門狀態變化應寫入事件記錄，供 `/s` 顯示最近一次狀態轉換時間

### 4.5 yt-dlp 熱更新

#### 4.5.1 需求

容器不重啟的前提下更新 yt-dlp，允許更新期間服務暫停。

#### 4.5.2 版本化目錄與原子切換

yt-dlp 存放於掛載的 volume，採版本化目錄結構：

```
/data/ytdlp/
  ├── versions/
  │   ├── 2026.08.19/yt-dlp
  │   └── 2026.09.02/yt-dlp
  ├── current -> versions/2026.09.02      # symlink
  └── previous -> versions/2026.08.19     # symlink，供 rollback
```

程式**不快取 symlink 的解析結果**，每次執行時重新解析 `current`，使切換立即生效。

#### 4.5.3 更新流程

1. **進入維護模式**：設置全域旗標，新的影片請求回傳「更新中」訊息影片；`/s` 仍可用
2. **等待排空**：等待進行中的解析與封裝工作完成，上限 60 秒；逾時則強制終止
3. **查詢最新版本**：透過 GitHub Releases API 取得最新的 tag
4. **短路判斷**：若最新版與目前版本相同，直接結束並回報「已是最新版」
5. **下載至暫存**：下載至 `versions/{new_version}.tmp/`，驗證檔案大小與可執行性
6. **煙霧測試**：以新版執行 `--version`，再對設定中的固定測試影片清單（建議 3 支）執行實際解析。**全部通過**才算成功
7. **原子切換**：測試通過則將 `previous` 指向現行版本，`current` 原子性地指向新版本（先建立臨時 symlink 再 `rename`）
8. **失敗回滾**：任一步驟失敗即刪除暫存目錄，保持現行版本不變，並在訊息影片中回報失敗階段與錯誤訊息
9. **退出維護模式**
10. **快取失效判斷**：yt-dlp 版本變更不影響既有的影片產物（產物是 ffmpeg 產生的），因此**不需要清空媒體快取**

#### 4.5.4 自動更新

- 排程檢查（預設每 24 小時）僅**檢查**而不自動執行，結果反映在 `/s` 的健康度顯示中
- 當版齡超過 `YTDLP_STALE_DAYS`（預設 30 天）時，`/s` 顯示橘色警示
- 是否自動執行更新由 `YTDLP_AUTO_UPGRADE` 設定控制，預設關閉（避免無人值守時的自動變更引入問題）

### 4.6 健康度與自我監測

`/s` 端點顯示的健康度由下列指標構成：

| 指標 | 計算方式 | 警戒條件 |
|---|---|---|
| yt-dlp 版齡 | 由版號 `YYYY.MM.DD` 推算 | > 30 天警示，> 90 天嚴重 |
| 近期解析成功率 | 最近 50 次解析的成功比例 | < 90% 警示，< 70% 嚴重 |
| 解析耗時中位數 | 最近 50 次的中位數 | > 8 秒警示 |
| 快取用量 | 已用 / 上限 | > 85% 警示 |
| 上線閘門 | 目前狀態與最近轉換時間 | — |
| 進行中工作 | 目前的解析與封裝任務數 | — |
| 磁碟可用空間 | 掛載點的剩餘空間 | < 10 GB 警示 |

另設一個**主動探測排程**（預設每 6 小時），對設定中的固定測試影片清單執行解析，將結果納入成功率統計。此舉確保即使長時間無人使用，健康度資料仍保持新鮮，使用者一查詢 `/s` 就能看到真實狀態。

### 4.7 快取

#### 4.7.1 快取鍵

```
{video_id}_{max_height}_{container}
```

例：`NJ1tne9u8YM_1080_hls`、`NJ1tne9u8YM_720_mp4`

不同畫質與容器視為不同產物，各自獨立快取。

#### 4.7.2 淘汰策略

- LRU，依最後存取時間排序
- 總容量上限由 `CACHE_MAX_BYTES` 設定，預設 50 GB
- 超過上限時淘汰至 `CACHE_TARGET_RATIO`（預設 0.8）以下，避免頻繁觸發
- **進行中的產物不可淘汰**
- **長影片特殊處理**：超過 `LONG_VIDEO_THRESHOLD`（預設 30 分鐘）的影片，其 HLS segment 採用滑動視窗保留策略——僅保留最近存取位置前後各 `LONG_VIDEO_WINDOW`（預設 10 分鐘）的 segment，其餘可淘汰並於需要時重新產生。此策略避免單支長片佔用過多空間

#### 4.7.3 併發去重（Singleflight）

同一個快取鍵的併發請求必須合併為單一準備工作。這在 VRChat 場景中極為關鍵：同一 instance 中的多名使用者會在數秒內送出相同請求，若無去重，將同時啟動多個 yt-dlp 程序抓取同一支影片——這不僅浪費資源，更是最容易觸發 YouTube 反機器人判定的行為模式。

實作使用 `golang.org/x/sync/singleflight`，去重粒度為**完整快取鍵**（含畫質與容器），因為不同畫質確實是不同產物。

但**解析階段**（yt-dlp metadata 抽取）的去重粒度應為 `video_id`，因為同一支影片的不同畫質共用同一份 metadata。因此系統有兩層 singleflight：

- 第一層：`resolve:{video_id}` —— 保護 yt-dlp 呼叫
- 第二層：`prepare:{cache_key}` —— 保護 ffmpeg 封裝工作

---

## 5. 非功能需求

| 項目 | 目標 |
|---|---|
| HLS 冷啟動延遲 | 從請求到第一個 segment 可播放，< 10 秒 |
| 快取命中延遲 | < 200 ms |
| 解析延遲 | 中位數 < 3 秒（Phase 0 實測 1.6 秒） |
| 併發影片準備 | 同時處理 3 支不同影片不劣化 |
| 記憶體用量 | 常駐 < 256 MB（不含 ffmpeg 子程序） |
| 容器映像大小 | < 200 MB |
| 重啟後狀態 | 快取產物與 yt-dlp 版本存活於 volume，重啟後立即可用 |
| 可攜性 | 所有狀態集中於單一 volume，整個服務可透過複製 volume 遷移 |

**安全性**：依 §4.4 決策，不實作額外的存取控制，僅以上線閘門作為暴露面控制。此決策的前提是服務網址不公開流通。需注意 VRChat 中同一 instance 的其他玩家可見播放器 URL，因此網址有外流可能；緩解措施為：

- 全域速率限制（`RATE_LIMIT_RPS`，預設每 IP 每分鐘 20 次請求）
- 全域併發準備工作上限（`MAX_CONCURRENT_JOBS`，預設 3）
- 上線閘門本身即為最強的暴露面控制——服務多數時間處於關閉狀態

---

## 6. 架構設計

### 6.1 分層與依賴方向

採 Clean Architecture，依賴方向嚴格由外向內：

```
┌─────────────────────────────────────────────────┐
│  Infrastructure（框架與驅動）                     │
│  HTTP server / yt-dlp exec / ffmpeg exec /      │
│  Discord gateway / 檔案系統 / 設定載入            │
│         ↓ 實作內層定義的介面                      │
├─────────────────────────────────────────────────┤
│  Interface Adapter（介面配接）                    │
│  HTTP handler / router / 訊息影片 presenter /    │
│  DTO 轉換                                        │
│         ↓ 呼叫                                   │
├─────────────────────────────────────────────────┤
│  Use Case（應用業務規則）                         │
│  PlayVideo / GetStatus / UpgradeYtdlp /         │
│  WarmVideo / PurgeCache / ...                   │
│         ↓ 操作                                   │
├─────────────────────────────────────────────────┤
│  Domain（企業業務規則）                           │
│  VideoRequest / MediaAsset / Health /           │
│  Availability / MessageView + 純介面定義          │
└─────────────────────────────────────────────────┘
```

**核心原則**：

- Domain 層不 import 任何外部套件（標準庫的 `time`、`errors` 等除外）
- Use Case 層僅依賴 Domain 層定義的介面，不知道 yt-dlp、ffmpeg、Discord 的存在
- 所有外部依賴透過介面注入，於 `main` 組裝
- 「偵測是否在玩 VRChat」的訊號來源作為介面定義於 Domain 層，實作置於 Infrastructure 層——這正是本專案要求可抽換的核心解耦點

### 6.2 目錄結構

```
vrcvp/
├── cmd/
│   └── vrcvp/
│       └── main.go                 # 組裝與啟動
├── internal/
│   ├── domain/                     # 最內層，無外部相依
│   │   ├── video/
│   │   │   ├── entity.go           # VideoID, VideoMeta, MediaAsset
│   │   │   ├── format.go           # Container, QualityCap, FormatSelector
│   │   │   └── errors.go           # 領域錯誤型別
│   │   ├── availability/
│   │   │   ├── signal.go           # Signal 介面 ★ 核心解耦點
│   │   │   └── gate.go             # Gate 聚合、組合邏輯、去抖動
│   │   ├── health/
│   │   │   └── health.go           # Health 指標與警戒判定
│   │   ├── message/
│   │   │   └── view.go             # MessageView：訊息的結構化表示
│   │   └── port/                   # Use Case 所需的介面（由外層實作）
│   │       ├── resolver.go         # Resolver
│   │       ├── packager.go         # Packager
│   │       ├── store.go            # AssetStore
│   │       ├── renderer.go         # MessageRenderer
│   │       ├── toolchain.go        # ToolchainManager
│   │       └── clock.go            # Clock（供測試注入）
│   ├── usecase/
│   │   ├── playvideo/
│   │   ├── status/
│   │   ├── upgrade/
│   │   ├── warm/
│   │   ├── cachemgmt/
│   │   └── info/
│   ├── adapter/
│   │   ├── http/
│   │   │   ├── router.go           # 路徑解析（§4.1.4）
│   │   │   ├── handler_video.go
│   │   │   ├── handler_command.go
│   │   │   └── middleware.go       # 速率限制、記錄、panic 回復
│   │   └── presenter/
│   │       └── message.go          # Domain 結果 → MessageView
│   └── infra/
│       ├── ytdlp/
│       │   ├── resolver.go         # Resolver 實作
│       │   └── manager.go          # ToolchainManager 實作（熱更新）
│       ├── ffmpeg/
│       │   ├── packager_hls.go     # Packager 實作（HLS）
│       │   ├── packager_mp4.go     # Packager 實作（MP4）
│       │   └── renderer.go         # MessageRenderer 實作
│       ├── signal/
│       │   ├── discord.go          # Discord Presence ★
│       │   ├── manual.go           # 手動覆寫
│       │   ├── process.go          # 本機程序偵測（預留）
│       │   ├── heartbeat.go        # 心跳端點（預留）
│       │   └── vrcweb.go           # VRChat 官網查詢（預留）
│       ├── store/
│       │   └── fsstore.go          # AssetStore 實作（檔案系統 + LRU）
│       └── config/
│           └── config.go
├── assets/
│   ├── fonts/                      # 嵌入的 Noto Sans TC 子集
│   └── templates/                  # 訊息影片版面模板
├── Dockerfile
├── docker-compose.yml
└── go.mod
```

### 6.3 核心介面定義

以下介面是本設計的骨架。所有簽章皆為示意，實作時可調整細節，但**依賴方向與抽象邊界不可改變**。

#### 6.3.1 訊號來源（核心解耦點）

```go
package availability

// Signal 代表一個「作者是否正在遊玩 VRChat」的訊號來源。
// 這是本專案要求可抽換的核心介面：新增偵測方式只需實作此介面，
// 無需改動 Gate、Use Case 或任何上層程式碼。
type Signal interface {
    // Name 回傳來源識別名稱，用於記錄與 /s 顯示。
    Name() string

    // Status 回傳目前的判定結果。
    // 實作應為非阻塞：長輪詢或 WebSocket 類型的來源應在背景維護狀態，
    // 此方法僅回傳最後已知狀態。
    Status(ctx context.Context) (Status, error)

    // Start 啟動背景作業（如 Discord Gateway 連線）。
    // 對無需背景作業的實作，可為 no-op。
    Start(ctx context.Context) error

    // Close 釋放資源。
    Close() error
}

type Status struct {
    Online     bool
    Confidence Confidence   // High / Medium / Low
    ObservedAt time.Time
    Detail     string       // 供 /s 顯示，例如 "Discord: playing VRChat"
}

// Gate 聚合多個 Signal，套用組合邏輯與去抖動，產出最終判定。
type Gate interface {
    IsOpen(ctx context.Context) (bool, Reason)
    Sources(ctx context.Context) []SourceStatus  // 供 /s 顯示各來源明細
    Override(open bool, until time.Time)         // 手動覆寫（/on、/off）
}
```

**設計說明**：`Confidence` 欄位的存在，是為了未來多來源並存時能做更細緻的組合——例如「本機程序偵測」的可信度高於「Discord presence」，當兩者衝突時可依可信度裁決。首版僅有單一來源，但介面預留此能力，避免日後破壞性變更。

#### 6.3.2 解析與封裝

```go
package port

// Resolver 負責從影片 ID 取得可下載的媒體資訊。
// 實作隱藏 yt-dlp 的存在。
type Resolver interface {
    Resolve(ctx context.Context, id video.VideoID, cap video.QualityCap) (*video.Resolution, error)
}

// Resolution 是解析結果，包含足以驅動封裝的一切資訊。
type Resolution struct {
    VideoID     video.VideoID
    Title       string
    Duration    time.Duration
    VideoURL    string        // 影像軌直連 URL
    AudioURL    string        // 音訊軌直連 URL；若為合一格式則與 VideoURL 相同
    Height      int
    VideoCodec  string
    AudioCodec  string
    NeedsRecode bool          // 選到非 avc1/mp4a 時為 true
    ExpiresAt   time.Time     // googlevideo URL 的預估失效時間
    ResolvedAt  time.Time
}

// Packager 將 Resolution 封裝為可交付的媒體產物。
type Packager interface {
    // Package 啟動封裝工作並立即回傳 Job；不阻塞至完成。
    Package(ctx context.Context, res *video.Resolution, spec video.OutputSpec) (Job, error)
}

// Job 代表一個進行中的封裝工作。
type Job interface {
    ID() string
    Progress() video.Progress          // 完成比例、已產出 segment 數、預估剩餘時間
    // Ready 回傳一個 channel，於「最小可播放單位」就緒時關閉。
    // HLS：第一個 segment 完成；MP4：整個檔案完成。
    Ready() <-chan struct{}
    Done() <-chan struct{}
    Err() error
    Cancel()
}
```

**關鍵設計**：`Ready()` 與 `Done()` 分離，是 HLS 與 MP4 兩條路徑能共用同一抽象的關鍵。HLS 的 `Ready` 遠早於 `Done`，MP4 的兩者則同時發生。Use Case 層只需等待 `Ready`，不必知道底層差異。

#### 6.3.3 產物儲存

```go
package port

type AssetStore interface {
    Get(key video.CacheKey) (*video.MediaAsset, bool)
    Put(key video.CacheKey, asset *video.MediaAsset) error
    Drop(key video.CacheKey) error
    Purge() error
    List(limit int) []*video.MediaAsset
    Usage() video.StorageUsage

    // OpenSegment 取得 HLS segment 的內容。
    // 若 segment 尚未就緒，阻塞至就緒或 ctx 逾時（見 §6.4）。
    OpenSegment(ctx context.Context, key video.CacheKey, seq int) (io.ReadSeekCloser, error)
}
```

#### 6.3.4 訊息算繪與工具鏈

```go
package port

type MessageRenderer interface {
    // Render 將結構化訊息算繪為媒體產物，並以內容雜湊快取。
    Render(ctx context.Context, view message.View, spec video.OutputSpec) (*video.MediaAsset, error)
}

type ToolchainManager interface {
    CurrentVersion() (string, error)
    CheckLatest(ctx context.Context) (string, error)
    Upgrade(ctx context.Context) (*UpgradeResult, error)
    Rollback(ctx context.Context) error
    // BinaryPath 每次呼叫時重新解析 symlink，使熱更新立即生效。
    BinaryPath() string
}

type UpgradeResult struct {
    From, To    string
    Stage       string    // 失敗時指出停在哪個階段
    SmokeTests  []SmokeTestResult
    Succeeded   bool
    Err         error
}
```

### 6.4 關鍵流程

#### 6.4.1 影片請求（HLS，快取未命中）

```
1. HTTP handler 解析路徑 → VideoID + OutputSpec（容器、畫質上限）
2. 檢查上線閘門 → 關閉則回傳「服務離線」訊息影片，結束
3. 組出 CacheKey，查詢 AssetStore → 未命中
4. singleflight("prepare:{key}") 進入：
   a. singleflight("resolve:{video_id}") → Resolver.Resolve()
   b. 若 NeedsRecode 為 true → 回傳「格式不支援」訊息影片，結束
   c. Packager.Package() → 取得 Job
   d. 依 Resolution.Duration 立即生成完整的 VOD playlist 並寫入 store
   e. 等待 Job.Ready() 或逾時（HLS_READY_TIMEOUT，預設 30 秒）
5. 回傳 master.m3u8（此時完整時間軸已可用，第一個 segment 已就緒）
6. 播放器後續請求各 segment → OpenSegment()
7. 背景的 Job 持續產出 segment 直到 Done()
```

#### 6.4.2 Segment 尚未就緒的處理

這是本設計最需要實測驗證的環節（見 §3.3）。策略採三段式：

```
請求 segment N：
  ├─ 已就緒 → 200/206 直接回傳
  ├─ 未就緒但工作進行中 →
  │    阻塞等待，上限 SEGMENT_WAIT_TIMEOUT（預設 20 秒）
  │      ├─ 期間就緒 → 200/206 回傳
  │      └─ 逾時 → 503 + Retry-After: 3
  └─ 工作已結束但 segment 不存在（長影片滑動視窗淘汰）→
       觸發該區段的重新產生，同上阻塞等待
```

**選擇阻塞優先而非直接回 503 的理由**：AVPro 對 503 的重試行為未知，而阻塞等待對播放器而言只是一個「慢的請求」，行為完全可預測。風險是佔用連線，因此必須設定上限並限制同時阻塞的請求數。

**追不上的情境**：使用者一開播就將進度拖到影片末段。由於 remux 是循序進行的，此時目標 segment 距離當前進度可能有數十分鐘。應對方式是偵測「請求的 segment 遠超目前產出進度」時，直接回傳訊息影片提示「該位置尚未準備完成，目前已準備至 MM:SS」，而非讓使用者面對無回應的播放器。

#### 6.4.3 googlevideo URL 於封裝中途失效

長影片的封裝時間可能接近或超過 googlevideo URL 的有效期（約 6 小時）。雖然以 `-c copy` 進行 remux 的速度遠快於實時，正常情況下不會遇到，但下載被限速時仍有風險。

```
1. ffmpeg 程序回報 403 或連線中斷
2. 判斷已完成的 segment 數 → 推算已完成的時間位置 T
3. 重新呼叫 Resolver.Resolve() 取得新的直連 URL
4. 以 -ss T 重新啟動 ffmpeg，從 T 續接
5. 續接產出的 segment 編號從中斷處接續
6. 重試上限 MAX_RESUME_ATTEMPTS（預設 3），超過則標記工作失敗
```

**HLS 續接的正確性要求**：必須使用 `-hls_flags independent_segments` 並確保 segment 邊界對齊關鍵影格，否則續接處會出現播放異常。以 `-c copy` 進行時，segment 邊界由來源的關鍵影格決定，`-ss` 的落點應對齊至最近的關鍵影格。

#### 6.4.4 yt-dlp 熱更新

見 §4.5.3 的十步驟流程。實作要點：

- 維護模式旗標以 `atomic.Bool` 實作，讀取路徑無鎖
- 排空等待使用 `sync.WaitGroup` 搭配逾時 context
- symlink 原子切換：`os.Symlink(target, tmpPath)` 後 `os.Rename(tmpPath, currentPath)`
- 煙霧測試的影片清單由設定提供，預設使用 Phase 0 驗證過的影片 ID

---

## 7. 資料模型

### 7.1 持久化狀態

所有狀態集中於單一 volume，確保可攜性：

```
/data/
├── ytdlp/                      # §4.5.2 的版本化目錄
├── cache/
│   └── {video_id}_{height}_{container}/
│       ├── meta.json           # 標題、長度、解析時間、格式資訊
│       ├── master.m3u8         # HLS 產物
│       ├── seg_00000.ts
│       ├── ...
│       └── video.mp4           # MP4 產物
├── messages/                   # 訊息影片快取，以內容雜湊命名
│   └── {sha256_prefix}/
├── state/
│   ├── health.json             # 成功率統計的滾動視窗
│   └── events.jsonl            # 閘門轉換、更新、錯誤的事件記錄
└── config.yaml                 # 可選的設定檔覆寫
```

### 7.2 核心型別

```go
type VideoID string          // 恆為 11 字元

type Container string
const (
    ContainerHLS Container = "hls"
    ContainerMP4 Container = "mp4"
)

type QualityCap int          // 360 / 480 / 720 / 1080 / 1440 / 2160

type OutputSpec struct {
    Container Container
    Quality   QualityCap
}

type CacheKey string         // fmt.Sprintf("%s_%d_%s", id, quality, container)

type MediaAsset struct {
    Key          CacheKey
    VideoID      VideoID
    Title        string
    Duration     time.Duration
    Spec         OutputSpec
    Height       int
    SizeBytes    int64
    Path         string
    State        AssetState   // Preparing / Ready / Failed / Partial
    CreatedAt    time.Time
    LastAccessAt time.Time
    Progress     Progress
}
```

---

## 8. 設定

全部透過環境變數提供，可選擇性地由 `/data/config.yaml` 覆寫。

| 變數 | 預設值 | 說明 |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | 監聽位址 |
| `PUBLIC_BASE_URL` | `https://v.gravity.tw` | 用於產生 HLS playlist 中的絕對 URL |
| `DATA_DIR` | `/data` | 狀態根目錄 |
| `DEFAULT_QUALITY` | `1080` | 預設畫質上限，**不寫死於程式碼** |
| `MAX_QUALITY` | `1080` | 允許的最高畫質，防止 URL 參數要求過高畫質 |
| `DEFAULT_CONTAINER` | `hls` | 未指定副檔名時的預設容器 |
| `HLS_SEGMENT_SECONDS` | `6` | HLS segment 長度 |
| `HLS_READY_TIMEOUT` | `30s` | 等待第一個 segment 的上限 |
| `SEGMENT_WAIT_TIMEOUT` | `20s` | 等待未就緒 segment 的上限 |
| `CACHE_MAX_BYTES` | `50GB` | 快取容量上限 |
| `CACHE_TARGET_RATIO` | `0.8` | 淘汰目標水位 |
| `LONG_VIDEO_THRESHOLD` | `30m` | 長影片判定門檻 |
| `LONG_VIDEO_WINDOW` | `10m` | 長影片 segment 保留視窗 |
| `MAX_CONCURRENT_JOBS` | `3` | 同時進行的封裝工作上限 |
| `RATE_LIMIT_RPM` | `20` | 每 IP 每分鐘請求上限 |
| `GATE_GRACE_PERIOD` | `10m` | 離線去抖動延遲 |
| `GATE_OVERRIDE_TTL` | `4h` | `/on` 手動覆寫的預設有效期 |
| `DISCORD_BOT_TOKEN` | — | Discord Bot token（必填） |
| `DISCORD_USER_ID` | — | 要監測的使用者 ID（必填） |
| `DISCORD_ACTIVITY_NAME` | `VRChat` | 比對的 activity 名稱 |
| `RESOLVER_PROXY` | 空 | 解析流量的出口代理，見 §3.1 |
| `YTDLP_AUTO_UPGRADE` | `false` | 是否自動執行更新 |
| `YTDLP_CHECK_INTERVAL` | `24h` | 版本檢查週期 |
| `YTDLP_STALE_DAYS` | `30` | 版齡警示門檻 |
| `HEALTH_PROBE_INTERVAL` | `6h` | 主動探測週期 |
| `HEALTH_PROBE_VIDEOS` | 內建清單 | 探測與煙霧測試用的影片 ID |
| `LOG_LEVEL` | `info` | 記錄層級 |

---

## 9. 部署

### 9.1 容器映像

基底建議使用 `alpine`，需安裝 `ffmpeg` 與 `ca-certificates`。yt-dlp **不打包進映像**，而是於首次啟動時下載至 volume，以支援熱更新。

多階段建置：

```dockerfile
FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /vrcvp ./cmd/vrcvp

FROM alpine:3.20
RUN apk add --no-cache ffmpeg ca-certificates tzdata python3
COPY --from=builder /vrcvp /usr/local/bin/vrcvp
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/vrcvp"]
```

**注意**：yt-dlp 的 Linux standalone 版本需要 Python 執行環境，因此映像必須包含 `python3`。若改用 `yt-dlp_linux` 這個自帶執行環境的建置版本則可省略，但檔案較大。此取捨應於實作時依實測決定。

### 9.2 反向代理與 TLS

服務本身不處理 TLS。前方部署 Caddy 或 nginx 負責：

- Let's Encrypt 憑證（`v.gravity.tw`）
- **AVPro 對 TLS 憑證驗證嚴格，自簽憑證必定失敗**，必須使用受信任的憑證
- 建議 Caddy，設定極簡且自動處理憑證續期

反向代理設定的關鍵要求：

- 不緩衝回應（`proxy_buffering off`），否則 HLS 的即時性會受影響
- 放寬讀取逾時至 60 秒以上，以容納 §6.4.2 的阻塞等待
- 正確傳遞 `Range` 標頭與 `206` 回應

### 9.3 網路暴露

依 §3.1 的部署決策（實驗室 Linux），需與實驗室網路管理協調對外連接埠的開放。若無法開放，備案為透過家中固定 IP 反向代理至實驗室機器（WireGuard 隧道）——此時媒體流量會經過家中上行，但 §2.1 顯示 303 Mbps 完全足夠。

---

## 10. 錯誤處理與邊界情況

| 情境 | 處理方式 |
|---|---|
| 影片 ID 格式錯誤 | 訊息影片：「無法辨識的影片連結」+ `/h` 提示 |
| 影片不存在 / 私人 / 已刪除 | 訊息影片：紅色，顯示 yt-dlp 的原始錯誤摘要 |
| 影片有年齡限制 | 訊息影片：明確指出原因；首版不嘗試繞過 |
| 直播 / 進行中的首播 | 訊息影片：「暫不支援直播」 |
| 影片過長（> `MAX_DURATION`） | 訊息影片：警示並詢問是否仍要處理（以 `/w` 明確觸發） |
| 選到非 avc1/mp4a 格式 | 訊息影片：「此影片格式需要轉碼，暫不支援」 |
| yt-dlp 解析失敗 | 依錯誤訊息分類（bot 偵測 / PO token / 影片不可用 / 網路），訊息影片顯示分類與建議動作 |
| ffmpeg 封裝失敗 | 保留 stderr 最後 20 行至事件記錄，訊息影片顯示摘要 |
| 磁碟空間不足 | 先觸發 LRU 淘汰；仍不足則拒絕新工作並在 `/s` 顯示嚴重警示 |
| 上線閘門關閉 | 灰色訊息影片，顯示最後偵測時間與 `/on` 的使用提示 |
| 更新期間收到影片請求 | 訊息影片：「服務更新中，預計 N 秒後恢復」 |
| 同時超過 `MAX_CONCURRENT_JOBS` | 訊息影片：「目前有 N 項工作進行中，請稍後再試」 |
| 請求的 segment 遠超產出進度 | 訊息影片：「該位置尚未準備完成，目前已準備至 MM:SS」 |
| panic | middleware 回復，記錄堆疊，回傳通用錯誤訊息影片 |

**共通原則**：任何錯誤都不回傳純文字或 JSON。使用者的唯一介面是影片播放器，因此**所有輸出都必須是可播放的媒體**。HTTP 狀態碼仍應正確設定（供記錄與除錯），但回應主體恆為媒體。

---

## 11. 可觀測性

由於維運介面在 VR 內，可觀測性的重點是**讓 `/s` 與 `/e` 能回答真正的問題**，而非堆砌指標。

**結構化記錄**（JSON Lines，輸出至 stdout 供 Docker 收集）：

- 每次解析：影片 ID、耗時、成功與否、選中的格式、yt-dlp 版本
- 每次封裝：快取鍵、耗時、產出大小、是否發生續接
- 閘門狀態轉換：來源、新舊狀態、時間
- 更新事件：版本變更、煙霧測試結果

**事件記錄**（`/data/state/events.jsonl`）：僅記錄值得在 `/e` 顯示的事件，採滾動保留（最近 500 筆）。

**不做的事**：不引入 Prometheus、Grafana 或任何額外的監控堆疊。以本專案的規模，`/s` 與 `/e` 兩個端點加上 `docker logs` 已完全足夠，額外的監控基礎設施只會增加維運負擔。

---

## 12. 開發里程碑

### M1：可播放（最小可行）

- HTTP 路由與路徑解析（§4.1.4）
- yt-dlp Resolver（固定畫質、無 fallback 鏈）
- ffmpeg HLS Packager（不含續接、不含滑動視窗）
- 檔案系統 AssetStore（不含 LRU）
- **驗收**：在 VRChat 世界中輸入 `v.gravity.tw/{id}`，1080p 影片正常播放且可 seek

### M2：訊息影片子系統

- MessageRenderer 與版面配置
- `/s`、`/h` 端點
- 所有錯誤路徑改為回傳訊息影片
- **驗收**：在 VR 內輸入 `/s` 可看見服務狀態；輸入無效影片 ID 可看見錯誤說明

### M3：上線閘門

- `availability.Signal` 介面與 Gate 聚合
- Discord Presence 實作
- 手動覆寫（`/on`、`/off`）
- 去抖動邏輯
- **驗收**：關閉 VRChat 後經過去抖動期，影片端點回傳離線訊息影片

### M4：熱更新與健康度

- ToolchainManager 與版本化目錄
- `/u` 端點與煙霧測試
- 健康度指標與主動探測
- **驗收**：容器不重啟的情況下完成 yt-dlp 版本升級與回滾

### M5：韌性與快取管理

- Singleflight 雙層去重
- LRU 淘汰與長影片滑動視窗
- googlevideo URL 續接（§6.4.3）
- Client fallback 鏈
- `/l`、`/e`、`/p`、`/d` 端點
- **驗收**：5 個併發請求同一影片僅觸發一次 yt-dlp；快取超過上限時正確淘汰

### M6：MP4 路徑與預熱

- MP4 Packager
- 進度訊息影片
- `/w`、`/r`、`/i` 端點
- **驗收**：在使用 Unity VideoPlayer 的世界中，`.mp4` 形式可正常播放

**里程碑順序的理由**：M1 先驗證核心假設（AVPro 能播我們產生的 HLS），M2 立刻建立維運介面（後續所有除錯都會用到），M3 才加上閘門（過早加入會妨礙開發期測試）。M6 的 MP4 路徑排在最後，因為 HLS 覆蓋了主要使用情境，MP4 是相容性補完。

---

## 13. 附錄

### 13.1 待實測驗證的項目

實作過程中應優先取得下列問題的答案，它們會影響設計細節：

1. AVPro 收到 HTTP 503 + `Retry-After` 時的行為（重試或放棄）
2. AVPro 對 HLS playlist 中相對路徑與絕對路徑的處理差異
3. 訊息影片的最短可接受長度（15 秒是保守估計）
4. VRChat 的影片載入逾時上限（決定 `HLS_READY_TIMEOUT` 的合理值）
5. Unity VideoPlayer 對 MP4 的具體要求（是否必須 faststart、支援的 profile level）
6. 實驗室網路的 yt-dlp 解析成功率（§3.1，部署前必須完成）

### 13.2 相關工具與函式庫

| 用途 | 選擇 |
|---|---|
| HTTP | 標準庫 `net/http`；路由簡單，不需要框架 |
| 併發去重 | `golang.org/x/sync/singleflight` |
| Discord | `github.com/bwmarrin/discordgo` |
| 設定 | `github.com/caarlos0/env` 或標準庫 + 手寫 |
| 影像繪製 | `golang.org/x/image/font` + `github.com/fogleman/gg`（可選） |
| 檔案服務 | 標準庫 `http.ServeContent`（正確處理 Range） |

### 13.3 術語

| 術語 | 說明 |
|---|---|
| Remux | 重新封裝。不改變影音編碼，僅更換容器格式。速度極快 |
| Transcode | 轉碼。重新編碼影音串流。速度慢、耗 CPU，本專案刻意避免 |
| Progressive | 影音合一的單一檔案格式。YouTube 已大幅移除 |
| DASH | 影音分離的自適應串流格式。YouTube 目前的主要提供形式 |
| SABR | YouTube 自有的伺服器端自適應串流協定，對第三方工具不友善 |
| PO Token | Proof of Origin Token，YouTube 的反自動化機制 |
| Singleflight | 將同一鍵值的併發請求合併為單次執行的模式 |