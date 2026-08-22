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
| `YTDLP_MODE` | `managed` | `managed` = 版本化目錄（spec §4.5.2）；`path` = 直接使用 PATH 上的 yt-dlp（開發用） |

### 1.2 ToolchainManager 的跨平台處理

spec §4.5.2 以 symlink 做原子切換。Windows 建立 symlink 需要管理員權限或開發者模式，不可靠。

**決策**：抽象為「current 指標」，兩種後端實作：

- Linux／容器：symlink（`os.Symlink` → `os.Rename`，維持 spec 的原子性保證）
- Windows：`current.txt` 純文字指標檔（寫入 `.tmp` 後 `os.Rename`，同樣是原子操作）

兩者都滿足 spec §4.5.2「程式不快取解析結果，每次執行時重新解析」的要求。

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
