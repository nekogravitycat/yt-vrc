# 實作決策紀錄

**專案**：`yt-vrc`
**文件版本**：v1.0
**建立日期**：2026-08-22
**關係**：本文件是 `spec.md` 的補充與修訂。凡本文件與 `spec.md` 衝突之處，**以本文件為準**，並在對應章節標註被取代的 spec 條目。

---

## 1. 開發與部署環境決策

| 項目 | 決策 | 理由 |
|---|---|---|
| 開發環境 | Windows 原生（`go run`），依賴走 PATH | 迭代速度快 |
| 部署目標 | Docker 映像（Alpine） | 與 spec §9.1 一致 |
| Module 路徑 | `github.com/nekogravitycat/yt-vrc` | 對齊 GitHub repo，`go get` 才正確 |
| 二進位名稱 | `yt-vrc`，進入點 `cmd/yt-vrc` | **取代 spec §6.2 的 `vrcvp`**；該名稱為歷史遺留 |
| Discord 憑證 | 尚未取得 | M3 先實作 `manual` 與 `fake` 兩個可測 Signal；Discord 實作照介面寫好，待憑證到位再實測 |
| M1 驗收 | 使用者即時架設對外環境（Caddy + TLS），於 VRChat 內實測 | — |

### 1.1 外部工具路徑

開發機的 ffmpeg 由 winget 安裝於
`C:\Users\gravity\AppData\Local\Microsoft\WinGet\Links\ffmpeg.exe`（Gyan build **9.0**）。

因此工具路徑**一律不寫死**，改由設定提供，預設值為裸名稱（走 PATH）：

| 變數 | 預設 | 說明 |
|---|---|---|
| `FFMPEG_PATH` | `ffmpeg` | ffmpeg 執行檔 |
| `FFPROBE_PATH` | `ffprobe` | ffprobe 執行檔 |
| `YTDLP_MODE` | `path` | `managed` = 版本化目錄（spec §4.5.2）；`path` = 直接使用 PATH 上的 yt-dlp。**預設為 `path`**（開發機的常態），容器部署須顯式設為 `managed`——見 §16.8 |

### 1.2 ToolchainManager 的跨平台處理

spec §4.5.2 以 symlink 做原子切換。Windows 建立 symlink 需要管理員權限或開發者模式，不可靠。

**決策**：抽象為「current 指標」，兩種後端實作：

- Linux／容器：symlink（`os.Symlink` → `os.Rename`，維持 spec 的原子性保證）
- Windows：`current.txt` 純文字指標檔（寫入 `.tmp` 後 `os.Rename`，同樣是原子操作）

兩者都滿足 spec §4.5.2「程式不快取解析結果，每次執行時重新解析」的要求。

**M4 實作時的修正**：後端不再依平台選擇，而是**嘗試建立 symlink、失敗才退回
文字指標檔**。在 Windows 上失敗的是權限而非平台，開發者模式開啟的機器應該
拿到比較好的那一種。見 §16.5。

---

## 2. Phase 0.5 實測結果（2026-08-22，開發機 / HiNet）

以下量測**推翻了 spec 的核心效能前提**，是第 3 節架構修訂的依據。

### 2.1 googlevideo 對單一長連線 GET 進行限速

以同一支影片的影像軌 URL，下載相同的前 24MB：

| 取得方式 | 耗時 | 速率 |
|---|---|---|
| 單一 GET（**ffmpeg 目前的行為**） | 80.6 s | **312 KB/s** |
| 4MB ranged 分塊循序抓取 | 1.29 s | **19.5 MB/s** |

**差距 62 倍。** 全檔（44.1 MB）循序分塊下載耗時 2.2 s，即 19.8 MB/s，等同 133× 實時，且速率可持續、不隨下載量衰減。

**結論**：**絕對不可將 googlevideo URL 直接交給 ffmpeg。** spec §6.4.1 的流程若照字面實作，1080p（約 3–5 Mbps）的 remux 會慢於實時播放，HLS 冷啟動與 seek 全部失效。

> 附註：yt-dlp 會警告找不到 JS runtime（`n` 參數的挑戰需 JS 解密）。開發機雖裝有 deno 2.2.3，但版本低於 yt-dlp 要求而未被採用。分塊抓取已足以取得全速，故本專案**不依賴 JS runtime**；但容器映像仍應安裝新版 deno 作為保險（見 §5）。

### 2.2 關鍵影格間隔不規則

實測某 4:56 影片的關鍵影格時間點：
`0.000, 3.003, 10.010, 13.313, 16.149, 18.352, 19.553, 20.821, 23.223, ...`

這是**依場景切換決定的可變 GOP**，不是固定間隔。以 `-hls_time 6 -c copy` 切出的 49 個 segment，實際長度分布於 **3.0 s – 9.8 s**。

**結論**：spec §4.2.1「由 `duration` 立即算出 segment 數量並寫出完整 playlist」**不可行**。該做法會宣告 50 個各 6.0 s 的 segment，與實際的 49 個不等長 segment 不符，造成時間軸偏移、seek 落點錯誤、尾段 segment 404。

### 2.3 remux 本身極快

資料已在本機後，整支 4:56 影片的 remux（`-c copy`）耗時 **0.24 秒**。

### 2.4 長影片的規模

75 分鐘（4527 s）影片，1080p avc1 影像軌 **165 MB**（googlevideo URL 的 `clen=` 查詢參數即為長度，無需 HEAD 請求）。

循序分塊（每塊都重新跟隨 302）實測 4.1 MB/s，推估全檔 40 s；**改為跟隨一次 302 後重用解析結果並以多 worker 並行，可降至約 10 s**。

---

## 3. 架構修訂：完整封裝後交付

### 3.1 決策

**取代 spec §4.2.1（HLS 路徑）、§6.4.1、§6.4.2、§6.4.3 的漸進交付設計。**

新流程：

```
1. Resolve（yt-dlp）取得兩條軌的直連 URL 與 metadata
2. 以並行 ranged 分塊下載器，將影像軌與音訊軌抓到本機暫存
3. ffmpeg -c copy remux 為 HLS（或 MP4）
4. 產物完整後才寫入 store 並交付 playlist
```

playlist 中的 `EXTINF` **全部取自 ffmpeg 實際產出的值**，時間軸與 seek 完全正確。

### 3.2 連帶簡化

以下 spec 條目因本決策而**不再需要實作**：

| spec 條目 | 狀態 |
|---|---|
| §4.2.1 playlist 於第一時間完整生成 | **廢除**——改為封裝完成後交付真實 playlist |
| §6.4.2 segment 未就緒的三段式阻塞策略 | **廢除**——交付時所有 segment 均已存在 |
| §3.3 AVPro 對 503 的行為未知 | **不再是風險**——不會回 503 |
| §6.4.3 googlevideo URL 於封裝中途失效 | **大幅降級**——下載窗口從數十分鐘縮短至數十秒，遠小於 URL 的 6 小時有效期。仍保留下載階段的分塊重試 |
| §13.1 第 1、2 項待驗證問題 | **消滅** |

### 3.3 保留的代價與後續選項

冷啟動不再是固定的「等第一個 segment」，而與影片長度成正比。**M1 完成後的實測值**（`FETCH_WORKERS=8`）：

| 影片 | 長度 | 產物 | 冷啟動 | 快取命中 |
|---|---|---|---|---|
| `NJ1tne9u8YM` | 4:56 | 51 MB / 49 segments | **2.8 s** | 1.1 ms |
| `BGXOYfZMR0w` | 75:27 | 268 MB / 754 segments | **8.1 s** | 1.1 ms |

75 分鐘影片的階段拆解（4 workers 時）：resolve 1.7 s、下載 18.6 s、remux 1.8 s。
**下載是唯一的瓶頸**，因此 `FETCH_WORKERS` 由 4 調高為 **8**，同一支影片的冷啟動自 22.1 s 降至 8.1 s。

兩者皆滿足 spec §5 的「HLS 冷啟動 < 10 秒」與「快取命中 < 200 ms」。若日後實測發現超長影片（>2 小時）超出 VRChat 的載入逾時，再導入「短片全備、長片漸進」的混合路徑；屆時漸進路徑的 playlist 必須依**實測**的 segment 長度推估，不得沿用 spec 的固定 6 s。

### 3.4 分塊下載器設計

新增 infra 元件 `internal/infra/fetch`（Domain 層以 `port.MediaFetcher` 介面表示）：

- 由 URL 的 `clen=` 參數取得總長度，省去 HEAD
- 先跟隨一次 302，之後所有分塊重用最終 URL 與 keep-alive 連線
- 預設 **8** 個 worker、4 MB 分塊（`FETCH_WORKERS`、`FETCH_CHUNK_BYTES`）；worker 數對長影片的冷啟動影響極大（見 §3.3）
- 分塊層級的重試；連續失敗則重新 Resolve 取得新 URL

---

## 4. 其他實作決策

### 4.1 Resolver 使用 JSON 而非 `--get-url`

`--get-url` 的輸出會與警告訊息混在一起且無結構。改用
`yt-dlp --dump-single-json -f <selector>`，讀取 `requested_formats` 取得兩條軌的 URL、codec、height，以及 `duration`、`title`、`is_live`。

### 4.2 M1 的實際範圍

依 spec §12 的 M1 定義，並套用上述修訂：

- HTTP 路由與路徑解析（spec §4.1.4）
- yt-dlp Resolver（固定畫質、無 client fallback 鏈）
- 並行分塊下載器（**新增**，M1 即需要，否則效能不可用）
- ffmpeg HLS Packager（完整封裝後交付）
- 檔案系統 AssetStore（不含 LRU）
- 錯誤此階段以純文字回應；M2 才改為訊息影片

---

## 5. 容器映像的補充要求

除 spec §9.1 所列，映像另需：

- **deno**（新版）——供 yt-dlp 解 `n` 參數挑戰。本專案的分塊下載已不依賴它，但缺少 JS runtime 會使部分格式無法取得（yt-dlp 已將無 JS runtime 的抽取標為 deprecated）
- Go 版本改用 `1.26`（開發機為 go1.26.5），非 spec 所寫的 1.23


---

## 6. M1 完成紀錄（2026-08-22）

### 6.1 已實作

| 元件 | 路徑 | 備註 |
|---|---|---|
| 路徑解析 | `internal/adapter/httpapi/route.go` | 完整實作 spec §4.1.4 的五段優先序 |
| yt-dlp Resolver | `internal/infra/ytdlp/` | 固定畫質、無 client fallback 鏈（M5 補上） |
| 分塊下載器 | `internal/infra/fetch/` | **新增元件**，非 spec 原有 |
| HLS Packager | `internal/infra/ffmpeg/` | 完整封裝後交付 |
| AssetStore | `internal/infra/store/` | 檔案系統，無 LRU（M5 補上） |
| PlayVideo UseCase | `internal/usecase/playvideo/` | 無 singleflight（M5 補上） |

### 6.2 實作過程中發現並修正的問題

1. **`net/http` 會把 `//` 正規化為 `/`**——貼上的 `https://youtu.be/x` 到達 handler 時變成 `https:/youtu.be/x`。已加入 `restoreSchemeSlash` 還原，並有回歸測試。
2. **`watch?v=` 的查詢字串會被伺服器當成自己的 query 解析**——`ParsePath` 收到的路徑只剩 `/https:/www.youtube.com/watch`。已在 handler 中重新接回 `RawQuery`。
3. **AAC 需要 `-bsf:a aac_adtstoasc`**——MP4 容器中的 AAC 沒有 ADTS header，直接複製進 MPEG-TS 會產生無法播放的音軌。

### 6.3 M1 驗收結果（本機）

- 6 種 URL 形式（裸 ID、`.m3u8`、`/720`、`youtu.be`、`watch?v=`、含 `&t=`）皆回應 200
- `ffprobe` 讀取服務輸出：h264 1920×1080 + aac，duration 4526.76 s vs 來源 4527 s
- seek 至 60:00 正確落在 3595.44 s（最近的 segment 邊界）
- Range 請求回應 206 與正確的 `Content-Range`，完整請求回應 200
- 錯誤分類正確：不存在的影片回 404 並顯示「影片不存在、為私人影片或已被移除」

**尚待使用者於 VRChat 內實測**（需對外網域與 TLS）。

### 6.4 M1 已知未完成項目

- 錯誤與命令回應仍為純文字，M2 改為訊息影片
- `/l`、`/s` 為簡化版；`/u`、`/e`、`/p`、`/on`、`/off` 等回 501
- 無 MP4 Packager（M6）、無上線閘門（M3）、無快取淘汰與 singleflight（M5）

---

## 7. M2 完成紀錄（2026-08-22）

### 7.1 顯示語言決策

訊息影片的**顯示文字一律使用英文**（使用者指定）。但**影片標題屬於資料而非顯示文字**，會原樣呈現，因此算繪字型仍必須涵蓋 CJK。

### 7.2 字型

| 項目 | 決定 |
|---|---|
| 字型 | Noto Sans TC Regular（SIL OFL，`assets/fonts/OFL.txt`） |
| 格式 | 靜態 CFF OTF，5.4 MB，20,950 glyphs |
| 嵌入 | `go:embed`，二進位由 10.6 MB 增為 16.3 MB |

**未採用可變字型**：Google Fonts 的 `NotoSansTC[wght].ttf`（11.4 MB）為 variable font，Go 的 `x/image/font/sfnt` 不支援字型變異。

**未做子集化**：以常用範圍子集後為 3.7 MB，僅省 1.7 MB。影片標題可能包含任意字元（日文漢字、簡體、符號），為 1.7 MB 而承擔缺字風險不值得。

### 7.3 交付方式

所有端點（含錯誤）改為 **302 導向至算繪好的訊息影片**：

```
/s        → 302 → /m/{hash}_hls/master.m3u8 → 200 media
/badpath  → 302 → /m/{hash}_hls/master.m3u8 → 200 media
```

**與 spec §10 的差異**：spec 要求「HTTP 狀態碼仍應正確設定，但回應主體恆為媒體」。實務上兩者無法同時成立——播放器不會播放 404 回應的內容。因此改為：**分類寫入結構化記錄**，回應走 302 → 200 媒體，另提供 `?debug=1` 回傳純文字供終端除錯。

### 7.4 實測

- HLS 與 MP4 兩種產物皆為 h264 1280×720 + AAC stereo、15 秒
- MP4 僅 49 KB（spec §4.3.3 預估 100 KB 以內）
- 訊息以內容雜湊快取，`MESSAGE_CACHE_ENTRIES`（預設 200）上限淘汰——狀態類訊息含即時數據，每次數值變動即為不同雜湊，必須有上限

### 7.5 M2 未完成

`/u`、`/e`、`/p`、`/on`、`/off`、`/w`、`/r`、`/i`、`/d` 仍回「Not Implemented Yet」的橘色訊息影片，分屬 M3–M6。

---

## 8. 驗收

### 8.1 自動化驗收腳本

```powershell
.\scripts\verify.ps1
.\scripts\verify.ps1 -VideoId <影片ID>   # 換一支測試影片（見 §8.2）
.\scripts\verify.ps1 -Port 9000          # 換連接埠
.\scripts\verify.ps1 -KeepData           # 保留產物供檢查
```

腳本以獨立的暫存資料目錄啟動服務，涵蓋 43 項檢查：建置與測試、6 種 URL
形式的**實際播放**、HLS 輸出正確性、seek 與 Range、快取延遲、所有端點的
訊息影片與其音軌，以及錯誤是否寫入記錄。

**設計要點**：失敗的影片請求同樣以 200 結束（錯誤是可播放的訊息影片），
因此腳本不能只看狀態碼——它比對最終 URL 是否落在 `/m/` 之下來區分
「真的播出來」與「播出一則錯誤訊息」。

### 8.2 【重要】YouTube 的每支影片速率限制

**2026-08-22 開發期間實測發現**：對同一支影片在短時間內重複解析，會觸發
`Sign in to confirm you're not a bot`。實測特性：

| 觀察 | 結果 |
|---|---|
| 觸發條件 | 同一支影片當日約十餘次解析 + 數百 MB 下載 |
| 影響範圍 | **僅該影片**——未大量測試過的影片同時仍可正常解析 |
| 是否 IP 層封鎖 | **否**。同一 IP 對其他影片正常 |
| 是否需要 cookies | 否。等待即可恢復 |

因此 §2.2 的「不需要 cookies」結論仍然成立，但**重複解析同一支影片是明確
的觸發因子**。這使 spec §4.7.3 的 singleflight 去重從效能最佳化升格為
**反封鎖的必要機制**——VRChat 同一 instance 中多人於數秒內送出相同請求，
正是最容易觸發此判定的模式。M5 應優先實作。

驗收時若遇到此情況，腳本會偵測並提示改用其他影片 ID，不會誤報為失敗。

### 8.3 新增的錯誤分類

依此發現補上 spec §10 要求但先前缺漏的兩類：

| 錯誤 | 訊息影片 |
|---|---|
| `ErrBotDetected` | 橘色警示「Blocked by YouTube」，說明影響所有影片且會自行恢復 |
| `ErrAgeRestricted` | 紅色「Age Restricted」 |

### 8.4 尚待人工驗收

自動化腳本無法涵蓋、必須由使用者完成的項目：

1. **VRChat 內實測**（M1 驗收條件，spec §12）——需 `v.gravity.tw` 與受信任
   憑證。AVPro 對自簽憑證必定失敗（spec §9.2）
2. **訊息影片在 VR 中的可讀性**——字級與對比是依 spec §4.3.3 推算，未經實地確認
3. **VRChat 的影片載入逾時上限**（spec §13.1 第 4 項）——決定長影片的可用長度上限

---

## 9. Singleflight 併發去重（提前自 M5）

依 §8.2 的發現提前實作 spec §4.7.3。這不是效能最佳化，而是**防止觸發
YouTube 每支影片速率限制的必要機制**。

### 9.1 兩層去重

| 層 | 鍵 | 保護對象 |
|---|---|---|
| 第一層 | `{cache_key}` | 整個準備工作（下載 + ffmpeg） |
| 第二層 | `{video_id}_{quality}` | yt-dlp 呼叫 |

### 9.2 與 spec §4.7.3 的差異：解析層的鍵包含畫質

spec 主張解析層應以 `video_id` 為鍵，理由是「同一支影片的不同畫質共用同一份
metadata」。這對 metadata 成立，但對本實作的 `Resolver.Resolve` **不成立**：
格式選擇器由畫質推導（`bv*[height<=H]...`），因此回傳的**軌道 URL 隨畫質而異**。
若僅以 `video_id` 為鍵，720p 的請求會收到 1080p 的軌道。

故解析鍵為 `{video_id}_{quality}`。實務上絕大多數請求使用預設畫質，去重效果
與 spec 的設想幾乎相同。

若日後要完全達成 spec 的粒度，需改為「一次抽取完整格式清單、在 Go 內做格式
選擇」，屆時 metadata 才真正可跨畫質共用。

### 9.3 呼叫者取消不影響共用工作

共用工作以 `context.WithoutCancel` 執行，並套用獨立的 `PREPARE_TIMEOUT`。
理由：先送出請求的播放器若放棄（使用者切換影片、離開世界），不得中斷其他
仍在等待同一支影片的觀看者。離開的呼叫者收到 `context.Canceled`，工作本身
繼續進行至完成。

### 9.4 驗證

單元測試（`internal/usecase/playvideo/playvideo_test.go`，含 `-race`）：

| 測試 | 保證 |
|---|---|
| `TestConcurrentRequestsResolveOnce` | 8 個併發請求 → 1 次 resolve、1 次 package |
| `TestDifferentQualitiesAreSeparate` | 不同畫質不被合併，且各自拿到正確高度 |
| `TestCancelledCallerDoesNotAbortSharedWork` | 呼叫者取消不影響其他等待者 |
| `TestCacheHitSkipsResolve` | 快取命中完全不碰 resolver |
| `TestSharedFailureReachesAllCallers` | 失敗傳達給所有加入者 |
| `TestRetryAfterFailure` | 失敗後該鍵可再次使用，不會被毒化 |

實機驗證（`scripts/verify.ps1`）：6 個併發請求同一支未快取影片 →
記錄中 `resolved`、`downloaded`、`packaged` 各 1 次。**達成 spec §12 M5 的
驗收條件**「5 個併發請求同一影片僅觸發一次 yt-dlp」。

---

## 10. AVPro 播放失敗的修正（2026-08-22）

### 10.1 症狀

VRChat 內 AVPro 回報 `Media cannot be played, maybe due to invalid format`，
但相同網址於瀏覽器可正常播放。伺服器記錄**無任何錯誤**——請求成功、產物正確。

### 10.2 三個原因

瀏覽器會自動跟隨轉址並嗅探內容型別，AVPro 兩者都不做，因此差異只在交付方式：

| # | 問題 | 修正 |
|---|---|---|
| 1 | 影片端點回傳 **302，且主體為 `text/html`** | 改為**直接內嵌回傳 playlist**，不再轉址 |
| 2 | 網址**無副檔名**（`/dQw4w9WgXcQ`），AVPro 依網址判斷後端 | 同上；現在直接回傳 `application/vnd.apple.mpegurl` |
| 3 | `EXT-X-VERSION:6` | 移除 `-hls_flags independent_segments` 後降為 **v3**，相容性最佳 |

`independent_segments` 原本是 spec §6.4.3 為中途續接而設；該機制已因 §3 的
架構修訂而不存在，故可安全移除。

### 10.3 連帶調整

playlist 直接於影片網址回傳後，ffmpeg 寫入的相對 segment 名稱
（`seg_00000.ts`）會以錯誤的基準解析，因此 `servePlaylist` 於送出時將其
改寫為絕對路徑 `/{cache_key}/seg_00000.ts`。

### 10.4 訊息影片一律回傳 200

先前為 302 導向，狀態碼可自由設定。改為內嵌回傳後，**非 200 的狀態會使
播放器拒絕算繪主體**——一支播不出來的錯誤訊息影片毫無意義。

因此所有訊息影片一律回傳 `200`，錯誤分類改由結構化記錄與 `?debug=1` 提供。
這比 §7.3 的舊做法更貼近 spec §10「回應主體恆為媒體」的原則，但完全放棄了
「狀態碼仍應正確」的部分。

### 10.5 驗收腳本新增 AVPro 相容性檢查

| 檢查 | 內容 |
|---|---|
| 不得轉址 | 影片網址須直接回應 200 |
| 內容型別 | `application/vnd.apple.mpegurl` |
| HLS 版本 | `EXT-X-VERSION:3` |
| segment 路徑 | 已改寫為絕對路徑 |

全套驗收現為 **50 項**。

---

## 11. AVPro 實測的關鍵發現（2026-08-22）

### 11.1 AVPro 在 PC 上就是 Windows Media Foundation

由 User-Agent 記錄確認的請求鏈：

| User-Agent | 身分 | 行為 |
|---|---|---|
| `Mozilla/5.0 (Windows NT 10.0...)` | VRChat 的解析器 | 先抓 master playlist 與 media.m3u8 |
| `NSPlayer/12.x WMFSDK/12.x` | **AVPro = Windows Media Foundation** | 抓 media.m3u8，再依序抓每一個 segment |

**WMF 對每一個 segment 都送 `Range: bytes=0-`。** 因此 §4.2.4 的 Range 支援
不是選配，是播放的必要條件。

**VRChat 會先用自己的解析器（yt-dlp）處理貼上的網址**，再把結果交給 AVPro。
因此服務端輸出必須同時讓 yt-dlp 與 WMF 都能正確理解。

### 11.2 「Media cannot be played, maybe due to invalid format」是通用載入失敗訊息

此訊息**不代表格式有問題**。實測過程中，同一批訊息影片時而可播、時而報此錯誤，
而以 ffmpeg 完整解碼驗證為 225 frames、零錯誤。

真正的原因是**產物在除錯過程中被刪除**（`rm -rf data/messages`），使 VRChat
已快取的解析結果指向不存在的路徑，回應 404 後 AVPro 即顯示此訊息。

**除錯時務必先確認產物是否存在，不要一看到此訊息就假設是編碼問題。**

### 11.3 內容雜湊網址與用戶端快取的衝突（待修）

訊息影片以內容雜湊為網址（spec §4.3.3）。但 `/s` 的內容含即時快取統計，
**每次數值變動就產生新的雜湊與新網址**。VRChat 會快取網址解析結果，於是：

- 舊產物仍存在（LRU 保留 200 筆）→ 使用者看到**過期的狀態畫面**
- 舊產物已被淘汰 → **404**，AVPro 報載入失敗

兩種結果都不正確。可能的解法是讓命令端點的媒體走**穩定路徑**（如
`/m/status/media.m3u8`）並標記為 `no-store`，內容雜湊僅用於磁碟上的去重。

**此問題尚未修正。**

### 11.4 已確認可在 AVPro 中播放

`/`、`/s`、`/status`、`/h`、`/help` 全部正常播放。訊息影片子系統（M2）
**於 VRChat 中驗收通過**。

### 11.5 M1 於 VRChat 驗收通過（2026-08-22）

`v.gravity.tw/dQw4w9WgXcQ`（1080p，4:53，36 個 segment）在 VRChat 中**播放成功**。

伺服器記錄佐證：

| 指標 | 觀測值 |
|---|---|
| 該影片的總請求數 | 60 |
| segment 抓取次數 | 53 |
| 涵蓋範圍 | `seg_00000` – `seg_00035`（**全部 36 個**） |
| 抓取順序 | **非循序** —— 代表使用者確實執行了 seek |
| 播放客戶端 | `NSPlayer/WMFSDK`（Windows Media Foundation） |

非循序的 segment 存取是關鍵證據：它直接證實 §3.2 的架構決策正確——
**完整封裝後才交付、playlist 帶真實 EXTINF**，使 seek 能落在正確位置。
若沿用 spec §4.2.1 由 duration 推算的預先生成 playlist，此處就會偏移。

**至此 M1 與 M2 的驗收條件（spec §12）全部達成。**

---

## 12. M3 完成紀錄：上線閘門（2026-08-22）

### 12.1 已實作

| 元件 | 路徑 | 備註 |
|---|---|---|
| `Signal` 介面、`Status`、`Confidence` | `internal/domain/availability/signal.go` | 依 spec §6.3.1 |
| `Gate` 聚合（OR、去抖動、覆寫） | `internal/domain/availability/gate.go` | 含背景輪詢 |
| Discord Presence 訊號 | `internal/infra/signal/discord.go` | **未實測**，見 §12.4 |
| Fake 訊號（開發用） | `internal/infra/signal/fake.go` | `FAKE_SIGNAL_ONLINE` |
| 覆寫持久化、事件記錄 | `internal/infra/state/state.go` | `/data/state/` |
| `/on`、`/off`、`/e` 端點 | `internal/adapter/httpapi/command.go` | |

### 12.2 決策：無訊號來源時 fail-closed

`GATE_ENABLED` 預設 `true`。**啟用且沒有任何訊號來源時，閘門為關閉**，只有 `/on` 能打開。

理由：一個沒設定的偵測器不構成「有人正在玩」的證據。逃生門是 spec §4.4.1 已經要求的——命令端點完全不受閘門限制，所以 `/s` 永遠能診斷、`/on` 永遠能打開。

**這對現行部署有直接影響**：`v.gravity.tw` 在取得 Discord 憑證前，每次重啟後都需要在 VRChat 內輸入一次 `/on`（預設有效 4 小時，`GATE_OVERRIDE_TTL`）。這是刻意保留的行為，不設過渡用的假訊號。

### 12.3 手動覆寫必須持久化

覆寫寫入 `/data/state/override.json`，重啟後仍然生效。

這在 fail-closed 之下不是選配：若重啟遺忘了一個有效的 `/on`，服務會**靜默地**整個停擺，而唯一能察覺的方式是戴上頭盔發現影片播不出來。

### 12.4 Discord 實作的狀態

依 discordgo 的文件行為寫成，**尚未以真實 Bot 驗證**（憑證未取得）。已處理的兩個要點：

- 訂閱 `GUILD_CREATE` 取得**初始 presence 快照**。只處理 `PRESENCE_UPDATE` 會讓閘門一直關到使用者下次切換活動為止
- Gateway 未連線時 `Status` 回傳 **error 而非 stale offline**。Gate 把 errored 來源視為「沒有證據」，由 `GATE_GRACE_PERIOD` 的去抖動吸收短暫斷線

需要在 Developer Portal 啟用 **Presence Intent**（privileged），且 Bot 必須與被監測的使用者同在一個 guild——presence 只會逐 guild 傳遞。

### 12.5 `/off` 的語意

依 spec §4.1.3，`/off` 是「解除手動覆寫、回歸自動偵測」，**不是**「強制關閉」。這一對是「接手」與「交還」，不是開與關。強制關閉仍可經由 `Gate.SetOverride(false, …)` 表達，但沒有端點暴露它。

---

## 13. §11.3 訊息影片網址問題的修正（2026-08-22）

### 13.1 做法：穩定 slot 路徑

命令端點的媒體改由**穩定的具名 slot** 交付，內容雜湊退回為純粹的磁碟去重鍵：

```
GET /s  → 200 master.m3u8（no-store）
             /m/status_hls/media.m3u8
GET /m/status_hls/media.m3u8  → 查 slot 表 → 目前的雜湊目錄
```

- slot 名稱描述**訊息關於什麼**（`status`、`help`、`v-{videoID}`、`gate`），從不描述它**當下說什麼**
- slot 表持久化於 `/data/state/slots.json`——VRChat 記住的是它解析到的媒體網址，重啟後忘記對應就會回 404
- slot 路徑一律 `no-store`；未登記於 slot 表的路徑段視為內容雜湊，仍為 `immutable`
- **被 slot 指到的產物不可被 `MESSAGE_CACHE_ENTRIES` 淘汰**（`MessageRenderer.Pinned`）。一個已經交給播放器的 slot 網址，壽命長於當時的那份算繪

### 13.2 驗證

`scripts/verify.ps1` 新增 5 項：`/s` 的媒體網址在快取統計改變前後**相同**、是具名 slot、先前取得的網址仍然 200、slot 為 `no-store`、影片產物仍為 `immutable`。

---

## 14. M5 進度（2026-08-22）

### 14.1 已完成

| 項目 | 實作 |
|---|---|
| 雙層 singleflight | §9（先前完成） |
| `MAX_CONCURRENT_JOBS` | `playvideo` 的 semaphore，**額滿立即拒絕不排隊** |
| LRU 淘汰 | `FSStore.evict()`，`CACHE_MAX_BYTES` / `CACHE_TARGET_RATIO` |
| client fallback 鏈 | `ytdlp.Resolver.Clients` |
| `/l`、`/e`、`/p`、`/d`、`/i` | 全部完成 |

**`MAX_CONCURRENT_JOBS` 額滿時立即回「Server Busy」訊息影片，不排隊。**排隊會讓使用者面對無回應的播放器；而且不受限的併發解析正是 YouTube 封鎖的流量形狀（§8.2）。

**client fallback 只在「換一個 client 有機會成功」的錯誤上重試**——`ErrResolveFailed` 與 `ErrBotDetected`。影片不存在、年齡限制、直播都是影片本身的事實，重試只會多送請求、逼近每支影片的速率限制。預設鏈為 `default,mweb,tv_embedded`；第一順位是 yt-dlp 自己的選擇（Phase 0 實測 15/15），所以正常情況下 fallback 不花任何成本。

### 14.2 決策：**廢除**長影片 segment 滑動視窗（取代 spec §4.7.2 的該段）

spec 要求超過 30 分鐘的影片以滑動視窗保留 segment，其餘可淘汰並於需要時重新產生。**此項不實作。**

理由：§3 的架構修訂後，segment 由**單一 ffmpeg pass** 產出。淘汰其中幾個之後，要重生任何一個都必須重新下載兩條軌並重新 remux 整支影片——等於用一次必然的昂貴重跑，去換取少量空間。而 1080p 實測約 3.5 MB/分鐘，50 GB 可容納約 240 小時影片；以本專案 5 人以內的規模，整支影片粒度的 LRU 已經足夠。

若日後真的撞到容量上限，成本更低的方向是限制單支影片的大小（超過門檻改用較低畫質），而不是部分淘汰。

### 14.3 尚未實作

- `/w`（預熱）、`/r`（強制重解析）——屬 M6
- `/u`（yt-dlp 熱更新）、健康度指標與主動探測——屬 M4
- MP4 packager（影片本體；訊息影片的 MP4 已可用）——屬 M6

---

## 15. 新增的設定（補充 spec §8）

| 變數 | 預設 | 說明 |
|---|---|---|
| `GATE_ENABLED` | `true` | `false` 完全移除閘門檢查 |
| `GATE_GRACE_PERIOD` | `10m` | 離線去抖動延遲 |
| `GATE_OVERRIDE_TTL` | `4h` | `/on` 的有效期 |
| `GATE_POLL_INTERVAL` | `30s` | 背景重新評估週期。**必要**：去抖動由「最後一次觀測到上線」起算，沒有請求時無人觀測 |
| `FAKE_SIGNAL_ONLINE` | 未設定 | 設定即啟用開發用假訊號（`true`／`false`） |
| `DISCORD_BOT_TOKEN` / `DISCORD_USER_ID` / `DISCORD_ACTIVITY_NAME` | — / — / `VRChat` | 兩者皆設定才會啟用 Discord 訊號 |
| `MAX_CONCURRENT_JOBS` | `3` | 同時準備的產物上限 |
| `CACHE_MAX_BYTES` | `50GB` | 接受 `50GB`、`500MB` 等字尾寫法 |
| `CACHE_TARGET_RATIO` | `0.8` | 淘汰目標水位 |
| `EVENT_LOG_ENTRIES` | `500` | `events.jsonl` 保留筆數 |
| `MESSAGE_SLOTS` | `200` | slot 表上限 |
| `YTDLP_CLIENTS` | `default,mweb,tv_embedded` | client fallback 鏈 |

---

## 16. M4 熱更新與健康度（2026-08-22）

### 16.1 已實作

| 元件 | 路徑 | 備註 |
|---|---|---|
| 健康度模型 | `internal/domain/health/` | 滾動視窗、成功率／中位數、spec §4.6 六項門檻評分 |
| `ToolchainManager` 介面 | `internal/domain/port/toolchain.go` | 含 `ToolchainVerifier`（煙霧測試策略） |
| 版本化目錄與原子切換 | `internal/infra/ytdlp/manager.go`、`marker.go`、`install.go` | spec §4.5.2、§4.5.3 |
| 煙霧測試 | `internal/infra/ytdlp/smoketest.go` | 對固定影片清單實際 resolve |
| 非受管模式 | `internal/infra/ytdlp/pathmanager.go` | `YTDLP_MODE=path` 時 `/u` 明確拒絕 |
| 升級／回滾流程 | `internal/usecase/upgrade/` | 背景執行、維護模式、排空 |
| 主動探測 | `internal/usecase/healthcheck/` | spec §4.6 的定期解析 |
| 健康度持久化 | `internal/infra/state/health.go` | `/data/state/health.json`（spec §7.1） |
| 磁碟可用空間 | `internal/infra/diskfree/` | Windows／Unix 兩份 build tag 實作 |
| `/u`、`/u/back` 端點 | `internal/adapter/httpapi/command.go` | |

### 16.2 決策：`/u` 背景執行而非阻塞

一次升級需下載約 30 MB 並實際解析 3 支影片，估計 20–60 秒。若阻塞至完成，
這個請求必須同時活過**兩個尚未量測的上限**——AVPro 的影片載入逾時
（spec §13.1 第 4 項，至今未知）與 Cloudflare 免費方案的 100 秒來源逾時。
播放器一旦放棄，使用者就無從得知版本到底換了沒有。

因此 `/u` 立即回傳黃色「Upgrade Started」訊息影片，工作在背景進行；再次輸入
`/u` 顯示目前階段（draining／checking／downloading／verifying／smoke test／
switching），完成後 90 秒內顯示結果。**這沿用 spec §4.2.3 已為 MP4 冷啟動
建立的「重新輸入同一網址看進度」互動模式**，不是新發明的語意。

`resultLinger` 定為 90 秒：夠走回影片面板讀完結果，又短到 `/u` 仍是一個動詞
而非一份報表。

### 16.3 決策：新增 `/u/back` 手動回滾（超出 spec §4.1.3）

spec §6.3.4 定義了 `Rollback()` 卻沒有對應端點。但煙霧測試只能證明
「新版能解析這 3 支影片」；若新版通過測試卻在實際使用中出問題，沒有端點就
只能離開 VR 去碰檔案系統。回滾本身只是切換一個指標檔，成本極低。

**回滾不因煙霧測試失敗而中止。** 回滾正是「現行版本壞掉」時要用的手段，
若因為舊版也測不過就拒絕，等於完全沒有退路——YouTube 端的破壞會讓所有版本
同時失敗。測試結果只記錄為警告。

`/u/back` 亦接受 `/u/rollback`、`/u/undo`。

### 16.4 決策：主動探測每次只輪詢一支影片

spec §4.6 要求每 6 小時對固定影片清單執行解析。若每次都跑完整份清單，
以 3 支影片計為每支每天 4 次解析——而 §8.2 的實測正是「同一支影片當日
十餘次解析」會觸發 `Sign in to confirm you're not a bot`。

改為**每次 tick 只探測一支，輪流推進**，同一支影片降至約每天 1.3 次。
探測只做 resolve 不下載，流量遠小於 §8.2 的觸發條件。

探測結果與使用者請求的解析共用同一個滾動視窗（spec §4.6 要求兩者都納入）。

### 16.5 跨平台的 current 指標：嘗試 symlink 再退回

implementation.md §1.2 原本規劃依平台選擇後端。實作改為**嘗試建立 symlink，
失敗才退回 `current.txt`**——在 Windows 上失敗的是權限而不是平台，開發者模式
開啟的機器應該拿到比較好的那一種。兩種形式都以「寫入 `.tmp` 後 `rename`」
達成原子性，讀取時 symlink 優先，寫入成功後刪除另一種形式以免兩者不一致。

### 16.6 下載改為 SHA-256 校驗（強化 spec §4.5.3 步驟 5）

spec 要求「驗證檔案大小與可執行性」。但被截斷的代理回應同樣有合理的大小、
同樣可執行。改為比對 release 一併發布的 `SHA2-256SUMS`，並額外執行
`--version` 確認回報的版號與 tag 相符（nightly 版號較長，故接受互為前綴）。

### 16.7 `/s` 版面重排

新增的 yt-dlp 版本與解析成功率必須進入 `/s`，但畫面只容得下約 6 行
（`headerH` 132 + subtitle 74，行高 56，下限 600 px）。因此：

- **移除** 「Default output」一列，改併入副標題（它是靜態設定，從不變動）
- **磁碟可用空間只在低於門檻時才佔一列**——沒在告警的空間不值得一行
- 標題列顏色改為跟隨**最差的指標**而非只看閘門：這是整個畫面唯一能隔著
  房間讀到的部分

「no samples yet」與「0%」刻意區分：全新啟動的服務是**沒有證據**，
不等於證據顯示它壞了。

### 16.8 `YTDLP_MODE=path` 下的行為

開發機預設 `path`（走 PATH 上的 yt-dlp）。此模式下：

- `/s` 照常顯示版本、版齡與「有新版可用」——這些資訊仍然有用
- `/u` 回傳橘色「yt-dlp Is Not Managed」，說明服務不會替換一個不是它安裝的
  二進位檔，而不是在下載流程深處失敗

容器部署應設 `YTDLP_MODE=managed`。首次啟動會下載最新版至 volume
（spec §9.1「不打包進映像」）；**bootstrap 失敗不阻擋啟動**，僅記錄錯誤並
退回 PATH——因為 GitHub 一時不通而讓整個服務起不來，比起讓管理端點活著
解釋問題要糟。

### 16.9 Resolver 改用 `Locate` 回呼

`ytdlp.Resolver` 原本持有固定的 `BinPath`。改為可選的 `Locate func() string`，
每次解析重新呼叫。這是 spec §4.5.2「程式不快取解析結果」在呼叫端的落實——
否則熱更新要等到下次重啟才生效，正是版本化目錄要避免的事。

### 16.10 排空與維護模式

- 維護旗標以 `atomic.Bool` 實作（spec §6.4.4），影片端點在**檢查閘門之前**
  先看它：更新中的不可用是可以等的，回「服務離線」會把使用者導向 `/on` 去
  修一個沒壞的東西
- 排空以輪詢 `ActiveJobs()` 實作而非條件變數：這個等待每年只發生幾次，
  不值得在請求路徑上加同步
- **排空逾時不中止升級**。已經啟動的工作握著它當時取得的執行檔路徑，
  切換只影響下一次 resolve

### 16.11 驗證缺口（已於 §17 補齊）

M4 首次提交時只通過 `go build`、`go vet` 與既有測試，沒有任何一行 M4 的
行為被驗證過：新程式碼零單元測試、`ytdlp.Manager` 因為會真的打 GitHub
與執行下載回來的二進位檔而不可測、`scripts/verify.ps1` 未涵蓋、
spec §12 的 M4 驗收條件從未達成。

**這些缺口已全數補上，見 §17。**

### 16.12 新增的設定

| 變數 | 預設 | 說明 |
|---|---|---|
| `YTDLP_MODE` | `path` | `managed` 啟用版本化目錄與 `/u`；容器應設此值 |
| `YTDLP_ASSET` | 依平台 | Windows `yt-dlp.exe`，其餘 `yt-dlp`（zipapp）。**不要在 Alpine 上用 `yt-dlp_linux`**，它連結 glibc，在 musl 上起不來 |
| `YTDLP_AUTO_UPGRADE` | `false` | 排程檢查是否也自動執行升級 |
| `YTDLP_CHECK_INTERVAL` | `24h` | 版本檢查週期；啟動時先檢查一次 |
| `YTDLP_STALE_DAYS` | `30` | 版齡警示門檻；嚴重門檻為其 3 倍 |
| `UPGRADE_DRAIN_TIMEOUT` | `60s` | 排空等待上限（spec §4.5.3 步驟 2） |
| `UPGRADE_TIMEOUT` | `10m` | 單次升級的總上限 |
| `HEALTH_PROBE_INTERVAL` | `6h` | 主動探測週期 |
| `HEALTH_PROBE_VIDEOS` | `dQw4w9WgXcQ,NJ1tne9u8YM,BGXOYfZMR0w` | 探測與煙霧測試共用的影片清單 |

---

## 17. M4 驗證（2026-08-22）

§16.11 列出的四個缺口全數補上，spec §12 的 M4 驗收條件達成。

### 17.1 `ytdlp.Manager` 的三個測試接縫

新增欄位 `APIBase`、`DownloadBase`、`Version`：前兩者讓測試把 GitHub 換成
`httptest` 伺服器，第三個取代對 `binaryVersion` 的直接呼叫。

沒有這三個接縫，`go test` 會**真的**打 GitHub、**真的**執行下載回來的二進位
檔。這正是「替換掉正在服務中的執行檔」那條路徑，是這個專案裡最不該被
一次隨手的測試執行誤觸的東西。

### 17.2 新增的測試

| 套件 | 涵蓋 |
|---|---|
| `internal/domain/health` | 滾動視窗與淘汰、成功率與中位數（失敗不計入延遲）、六項門檻、`Overall` 取最差、版號年齡解析 |
| `internal/infra/ytdlp`（manager、install） | 安裝順序、SHA-256 不符中止、版號不符中止、nightly 後綴容忍、煙霧測試失敗**不切換**、no-change 短路、指標指向空目錄時重裝、回滾（含煙霧測試失敗仍放行）、無前一版拒絕、prune 後仍可回滾、`BinaryPath` 每次重讀 |
| `internal/infra/ytdlp`（marker） | 兩種指標形式只能存在其一、暫存檔不殘留、二進位遺失時 `resolveMarker` 拒絕但版號仍可讀 |
| `internal/infra/ytdlp`（resolver、pathmanager） | 錯誤分類、可重試判定、`Locate` 每次重解析、非受管模式拒絕安裝與回滾 |
| `internal/usecase/upgrade` | 8 個併發 Trigger 只跑一次、維護旗標涵蓋整段執行且失敗後仍解除、結果殘留視窗、排空先於切換、排空逾時不中止、呼叫者取消不中斷、自動升級的三個前提、事件記錄 |
| `internal/usecase/healthcheck` | 每次只探測一支並輪流推進、結果納入視窗、失敗寫入事件、`Run` 隨 context 結束 |
| `internal/infra/config` | `.env` 解析與「環境變數優先」 |

全部在 `-race` 下通過。

### 17.3 測試抓到的兩個 bug

兩個都在寫測試的當下才浮現，都不是測試寫錯。

**（a）年齡限制被誤判為 bot 偵測**（`ytdlp/resolver.go`）

yt-dlp 對年齡限制影片的訊息是 `Sign in to confirm your age`，而 bot 偵測那條
的比對字串是 `sign in to confirm`，排在前面就先吃掉了。後果有兩層：使用者
看到橘色「YouTube 擋住我們，會自己恢復」，但這支影片其實永遠不會好；而且
`ErrBotDetected` 是可重試的，於是整條 client fallback 鏈跑滿三次解析——正是
§14.1 明言不該對「影片本身的事實」做的事。

修法是把年齡限制用具體片語（`confirm your age`、`age-restricted`、
`inappropriate for some users`）比對，並排在 bot 偵測**之前**。

**（b）網路逾時被誤判為影片不存在**（同一個 `switch`）

舊碼有一條裸的 `strings.Contains(s, "age")`。`unable to download web**page**`
裡有 `age`，於是逾時走進年齡分支、內層比對失敗、`fallthrough` 到
`ErrNotFound`：使用者被告知「影片已刪除」，而且因為 `ErrNotFound` 不可重試，
fallback 鏈也不會啟動。該條件已刪除。

**（c）`/u/back` 在升級後的 90 秒內被靜默吞掉**（`usecase/upgrade`）

`Trigger` 原本只看 `state.Fresh(now)`，不分種類。於是一次 `/u` 完成後的
`resultLinger` 期間，`/u/back` 會被當成「你剛剛才問過，這是結果」而不執行，
畫面顯示同一份升級成功報告——完全看不出指令沒生效。

而那 90 秒正是會想回滾的時間窗：升級完 → 看結果 → 覺得不對 → 輸入
`/u/back`。§16.3 把 `/u/back` 定位成「不必離開 VR 的逃生門」，這個 bug 讓
逃生門在最需要它的那一刻鎖上。

修法：**執行中**的工作仍然阻擋任何一種（兩者移動同一個指標，讓回滾去搶一個
切到一半的狀態，就是volume 指向暫存目錄的來源）；**已完成**的結果只短路
同一種。重複 `/u` 仍是「給我看結果」，重複 `/u/back` 也是（回滾兩次會往前走）。

連帶修正 presenter：`/u/back` 進行中顯示「Rollback Started／Running」、失敗
顯示「Rollback Failed」，不再一律說 Upgrade。

### 17.4 真實升級與回滾

以 `YTDLP_MODE=managed`、全新 volume 實跑。

| 階段 | 觀測 |
|---|---|
| 空 volume 啟動 | bootstrap 下載 2026.08.19 至 `versions/`，寫 `current.txt`（symlink 建立失敗後退回，如 §16.5 設計） |
| 已是最新版時 `/u` | 藍色「Already Up To Date」，不下載 |
| 手動把 current 指向 2026.07.04 後 `/s` | 橘色、`49d old`、`/u to update` |
| 真實升級 `/u` | 12 秒完成，煙霧測試 3/3，`current`→2026.08.19、`previous`→2026.07.04 |
| 升級期間請求影片 | 黃色「Updating」+ 階段名，**不是**「Service Offline」（§16.10 的排序正確） |
| **不重啟**查 `/s` | 立刻顯示 2026.08.19 —— `BinaryPath` 每次重讀指標生效 |
| `/u/back` | 9 秒完成，兩個指標對調，`/s` 回到橘色 |
| 非受管模式（`YTDLP_MODE=path`）的 `/u`、`/u/back` | 橘色「yt-dlp Is Not Managed」 |

**spec §12「容器不重啟的情況下完成 yt-dlp 版本升級與回滾」至此達成。**

### 17.5 `verify.ps1` 的 M4 檢查

新增三節：`/s` 的版本與成功率兩列、非受管模式的 `/u` 與 `/u/back` 拒絕，
以及一個獨立的受管實例（自有連接埠與 volume）驗 bootstrap 安裝、指標**只**
存在一種形式、`/s` 不再顯示 unmanaged、最新版時 `/u` 為 no-change，以及
`/u/back` 在該 `/u` 結果仍在殘留視窗內時**以回滾的身分回應**而非重播升級
結果。§17.3c 的完整回歸（跨越殘留視窗真的啟動一次回滾）留在單元測試——
端對端要做到需要 volume 裡有兩個版本，代價是每次驗收多一次 30 MB 下載與
三支影片的煙霧測試。

腳本現為 **84 項**。

### 17.6 `.env` 載入（新增）

`config.Load()` 於讀取環境變數**之前**先讀工作目錄下的 `.env`，**已設定的
環境變數永遠優先**。存在的理由只有一個：`DISCORD_BOT_TOKEN` 是這個服務唯一
需要、而人無法憑記憶重打的值，不該每次啟動都手輸入或貼進聊天視窗。

檔案缺席不是錯誤——`.env` 是開發機提供憑證的方式，不是部署的方式；容器仍
以真實環境變數供應，且檔案不得蓋過它。範本見 `.env.example`（`.env` 本身
已在 `.gitignore`）。

**連帶影響 `verify.ps1`**：驗收腳本啟動的實例改以 `-WorkingDirectory $dataDir`
執行。否則「fail-closed、無訊號來源」那一節會從 repo 根目錄撿到 `.env`，
拿到真的 Discord 來源，整節測試失去意義。

### 17.7 M3 Discord 訊號實測（§12.4 的缺口）

取得真實憑證後實測通過：

| 檢查 | 結果 |
|---|---|
| Gateway 連線 | `connected: true` |
| **初始 presence 快照** | 服務在使用者已經在玩時啟動，仍立即判定 online |
| 閘門僅靠 discord 開啟 | `/s` 顯示 `open · discord`、`discord online · playing VRChat`；事件記錄 `gate opened (discord)` |
| 影片經開啟的閘門交付 | 正常，resolve 2.3 s |

其中**初始快照**是 §12.4 特別擔心的一項：`PRESENCE_UPDATE` 在這個情境下
根本不會觸發，能判定 online 只可能是 `GUILD_CREATE` 的快照生效。若那段沒
寫對，閘門會一直關到使用者下次切換活動為止。

Bot 端設定：只需在 Developer Portal 開 **Presence Intent**；guild 權限**一個
都不需要**（presence 來自 Gateway intent，不是 permission），邀請連結的
`permissions=0` 即可。Bot 必須與被監測使用者同在一個 guild。

---

## 18. 對外解析額度（rate limit，2026-08-22）

### 18.1 為什麼需要

在此之前，對 YouTube 的節流只有兩個**間接**的機制：singleflight 把同一支影片
的併發請求收成一次，`MAX_CONCURRENT_JOBS` 限制同時準備的產物數。兩者都只涵蓋
「同一瞬間」，**沒有任何東西限制一段時間內總共打了幾次 yt-dlp**。

而 §8.2 實測的觸發條件正是跨時間的：「同一支影片當日約十餘次解析」。最容易
達成它的路徑是使用者反覆輸入同一個網址——快取被淘汰、用 `/r` 強制重解析、
或最常見的：**那支影片解析失敗，於是他一直重試**。

### 18.2 設計

`internal/domain/throttle` 的滑動視窗計數器，兩個維度：

| 變數 | 預設 | 說明 |
|---|---|---|
| `RESOLVE_LIMIT_PER_VIDEO` | `5` | 單一影片在視窗內的解析次數 |
| `RESOLVE_LIMIT_GLOBAL` | `40` | 全服務在視窗內的解析次數 |
| `RESOLVE_LIMIT_WINDOW` | `1h` | 視窗長度 |

以 `throttle.Resolver` 這個 `port.Resolver` 裝飾器套用，於 `main` 組裝。
放裝飾器而不是塞進 `ytdlp.Resolver`：額度是「這個部署與 YouTube 的整體關係」
這種政策，而 `ytdlp.Resolver` 的職責只是跑一個指令並讀它的輸出。副作用是
播放路徑與健康探測**共用同一份額度**，而兩者都不需要知道對方存在。

**鍵是影片 ID，不含畫質。** 與 §9.2 的 singleflight 鍵刻意不同：畫質是我們
自己的事，從 YouTube 的角度，問 720p 和問 1080p 是同一支影片被問了兩次。

### 18.3 三個容易寫錯的決定

**（a）失敗的解析一樣扣額度。** `Allow` 在知道結果之前就扣款。只算成功等於
讓「一直重試一支解析不了的影片」這個情境完全不受限——而那正是實測觸發條件
描述的形狀。

**（b）被自己擋下的解析不計入健康度視窗。** 它根本沒碰到 YouTube，對「解析
還能不能用」沒有證據價值。若計入，服務的自我克制會把 `/s` 的成功率一路壓成
紅色，而那個數字存在的目的恰好相反（spec §4.6）。`playvideo` 與
`healthcheck.Probe` 兩處都有這個守衛。

**（c）煙霧測試只扣款、不會被拒絕。** 升級的煙霧測試要判斷候選 yt-dlp 能不能
用，擋下它只會讓升級失敗、保護不到任何東西。但它的請求確實會送到 YouTube：
四輪升級／回滾就是同樣三支影片被解析十二次，正是問題的形狀。因此
`SmokeTester.Charge` 記帳但不設限。

### 18.4 超額時的行為

**立即拒絕，不排隊**——與 `MAX_CONCURRENT_JOBS` 一致（§14.1）。回一支橘色
「Slowing Down」訊息影片，並依照是哪一個維度用完而給不同建議：

- 每影片：「這支影片剛查過太多次，`in N min` 再試；已快取的照常播，**其他
  影片不受影響**」——因為換一支影片確實就解決了
- 全域：「服務的查詢額度用完了，快取的照常播，新影片 `in N min` 恢復」

標題刻意**不**寫成「Blocked by YouTube」。把自我節流說成被封鎖，會讓使用者
去追一個還沒發生的問題——而「還沒發生」正是這整個機制的目的。

### 18.5 `/s` 只在快用完時才顯示

沿用 §16.7 對磁碟可用空間的同一條規則：任一維度用掉四分之三以上才佔一列。
在那之下這個數字回答的是沒有人在問的問題；在那之上，下一支新影片就可能被
拒絕，而「被告知 Slowing Down 卻完全沒有預兆」正是這一列要避免的。

每影片維度接近上限時會**指名是哪一支影片**佔著額度——這個維度換一支影片就
能繞過，但使用者無從得知是哪一支。

### 18.6 沒有做的：懲罰箱

曾考慮在某支影片真的收到 `ErrBotDetected` 後把它關進冷卻期。**未採用**：
一次誤判就會把一支本來能播的影片鎖住半小時。退而求其次的保護來自 §18.3(a)
——被擋的解析同樣扣額度，所以一支持續失敗的影片會在幾次之後自然被本地拒絕，
不必額外的懲罰狀態。

---

## 19. M6：MP4 路徑與預熱（2026-08-22）

### 19.1 spec §4.2.3 的前提已經不成立

spec 為 MP4 設計了一整套「冷啟動進度互動」，理由是「MP4 必須完整下載並封裝
完成後才能交付，HLS 只需等第一個 segment」。

**§3 的架構修訂之後，這個對比消失了。** 兩條路徑現在都是完整封裝後才交付，
所以 MP4 的冷啟動與 HLS **一樣快**（同樣的下載 + 同樣的 `-c copy`）。因此
影片端點對 MP4 **不做**特殊的進度處理，維持與 HLS 一致的阻塞行為——那個行為
已經在 VRChat 內驗收過。

進度互動仍然實作，但用在它真正有價值的地方：`/w`。

### 19.2 MP4 packager

`internal/infra/ffmpeg/packager_mp4.go`。與 HLS 版本的兩個差異都是必要的：

- **不加 `-bsf:a aac_adtstoasc`**。那個 filter 是為了 MPEG-TS 補 ADTS header
  （§6.2 第 3 點），來源與目的都是 MP4 時套用它反而會把能播的音軌變成靜音
- **`-movflags +faststart`**，把 moov atom 搬到檔首。沒有它，播放器必須讀到
  檔尾才能開始，在 HTTP 上等於先下載整支影片

實測（`NJ1tne9u8YM`，4:56）：h264 1920×1080 + aac stereo、51.2 MB、
duration 295.96 s、`moov` 在 offset 0x24（早於 `mdat`）、Range 回 206。

### 19.3 `/w` 與 `/r`

兩者共用一個 handler，因為它們只差一步：**要不要先把已快取的丟掉**。

**`/w/{id}`（預熱）**
- 已快取 → 綠色「Ready To Play」，附標題、大小、長度
- 未快取 → 背景啟動，**先等 `warmGrace`（4 秒）看它會不會失敗**

那 4 秒是刻意的：值得立刻告知的失敗——影片不存在、解析額度用完、工作槽滿——
都落在頭兩秒內（Phase 0 的 resolve 中位數 1.6 s）。撐過去的就是真的在下載，
於是回傳進度畫面。這比「一律回排入佇列」誠實：貼錯網址的人當場就知道。

**`/r/{id}`（強制重建）**
- 丟掉該影片的**所有**變體，不只請求的那一個。`/r` 是「這個產物壞了」的按鈕，
  會去按它的人沒有理由知道 720p 在快取裡是另一筆
- 丟棄數量寫入事件記錄
- 之後與 `/w` 相同

**進度來源**：`playvideo` 為進行中的工作維護一份狀態（stage、bytes、標題），
在工作結束時刪除——產物一旦進了 store，store 就是所有問題更好的答案。

估算**只根據下載階段**。那是唯一「剩餘工作量可知」的階段：resolve 是一次不透
明的呼叫，而其後的 remux 對五分鐘影片只花 0.24 秒（§2.3）。把任一者折進百分比
只會讓數字更不誠實，而不是更完整。

進度畫面顯式列出 **Stage**：下載完成後位元組數就不再變動，一個凍結的
「234 MB / 234 MB」看起來像卡住，而不像它實際上正在做的 remux。

### 19.4 `/h` 重排

`/w` 與 `/r` 進入速查表後，命令列從 5 行增為 6 行（畫面上限）。

---

## 20. 容器映像（2026-08-22）

### 20.1 基底改用 Alpine 3.24.1，JS runtime 改用 node

handoff §8 要求映像包含「新版 deno」。實作時發現這件事比想像中麻煩，而且
spec §9.1 與 §5 都需要修正。

**yt-dlp 對 JS runtime 有最低版本要求**（`yt_dlp/utils/_jsruntime.py`）：

| runtime | 最低版本 | Alpine 3.20 | Alpine 3.24.1 |
|---|---|---|---|
| deno | 2.3.0 | 1.43.5 ✗ | 2.7.4 ✓ |
| node | 22.0.0 | 20.15.1 ✗ | 24.18.1 ✓ |
| bun | 1.2.11 | 未打包 | 未打包 |
| quickjs | 2023.12.9 | 0.2024… ✗ | — |

**Alpine 3.20 的每一個候選都低於門檻。** 照 handoff 的字面裝 deno，會付出
82 MB 換一個 yt-dlp 標記為 unsupported、永遠不會呼叫的執行檔。這也解釋了
§2.1 的舊觀察：開發機的 deno 2.2.3 之所以被拒，就是差在 2.3.0 這條線。

3.24.1 上兩個都可用，選 **node**：

- **體積**：node 加 66 MB，deno 加 120 MB
- **餘裕**：node 24.18 高於門檻兩個大版本；3.22 的 deno 2.3.1 只比門檻高一個
  patch，yt-dlp 一動門檻就斷

### 20.2 `--js-runtimes node` 不是選配

裝了 node 還不夠。**yt-dlp 預設只啟用 deno**，其餘一律回報 unavailable：

```
[debug] JS runtimes: none
[debug] [youtube] [jsc] JS Challenge Providers: bun (unavailable),
        deno (unavailable), node (unavailable), quickjs (unavailable)
WARNING: No supported JavaScript runtime could be found. Only deno is
         enabled by default; to use another runtime add --js-runtimes …
```

因此新增設定 `YTDLP_JS_RUNTIMES`（預設空，即沿用 yt-dlp 的預設），由
`ytdlp.Resolver` 與 `SmokeTester` 傳給 `--js-runtimes`，映像中設為 `node`。
加上之後：`[debug] JS runtimes: node-24.18.1`，警告消失。

**少了這個環境變數，映像就是白背 66 MB。**

### 20.3 `.dockerignore` 用白名單

黑名單 fail-open：下一個掉進 repo 的憑證或狀態檔預設會進 build context，而
context 裡的東西離映像只差一行 `COPY . .`。白名單 fail-closed——漏列的後果是
編譯失敗，不是洩漏。

實測 context 內容確認只有 `go.mod`、`go.sum`、`cmd/`、`internal/`、
`assets/fonts/OFL.txt`；`.env`、`data/`、`docs/`、`.git/` 全部不在其中。

`OFL.txt` 對編譯不必要（嵌入用的字型副本在 `internal/infra/render/`），但二進位
檔帶著 Noto Sans TC，散布映像即散布字型，SIL OFL 要求授權隨附，因此它被
`COPY` 進 `/usr/local/share/licenses/`。

### 20.4 映像大小：261 MB（**未達成 spec §5 的 < 200 MB**）

| 組成 | 大小 |
|---|---|
| alpine + ffmpeg + ca-certificates + tzdata + python3 | 181 MB |
| nodejs | +66 MB |
| yt-vrc 二進位（含 5.4 MB 嵌入字型） | +14.5 MB |

**未達標，且是刻意的。** spec §5 的 200 MB 寫在「JS runtime 是選配」的假設下。
真要達標只能拿掉 node（196 MB），代價是部分格式無法取得且走上 yt-dlp 已標記
為 deprecated 的路徑。

yt-dlp 本身**不在**映像裡（spec §9.1），首次啟動時安裝到 volume。

### 20.5 其他決定

- **HEALTHCHECK 打 `/h` 而不是影片端點**。上線閘門會在沒人玩的時候關閉影片
  端點——那是設計，不是故障。用影片端點做健康檢查，等於讓 Docker 週期性重啟
  一個完全正常的服務。`/h` 不受閘門限制，且走同一條 handler 鏈
- **compose 檔案設 `cpus` 與 `mem_limit`**。remux 短而吃滿 CPU，三個併發工作
  在沒有上限時會把整台機器佔滿——部署目標是共用的實驗室機器（spec §3.1）
- **憑證只走真實環境變數**。compose 從主機的 `.env` 代入，檔案不進映像；
  `.dockerignore` 的白名單也保證了這一點
