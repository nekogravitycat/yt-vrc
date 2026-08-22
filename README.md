# yt-vrc

把 YouTube 影片即時重新封裝成 VRChat 播放器能直接播放的串流。

- **現況與交接：[`docs/handoff.md`](docs/handoff.md)** ← 從這裡開始
- 需求與設計：[`docs/spec.md`](docs/spec.md)
- 實作決策與實測紀錄：[`docs/implementation.md`](docs/implementation.md)（與 spec 衝突時以此為準）
- 部署與 VRChat 實測：[`docs/deployment.md`](docs/deployment.md)

## 開發

需要 `go`、`ffmpeg`、`yt-dlp` 在 PATH 上。

```bash
go test ./...
go run ./cmd/yt-vrc
```

預設監聽 `:8080`，狀態寫入 `./data`。

| 常用設定 | 預設 | 說明 |
|---|---|---|
| `LISTEN_ADDR` | `:8080` | 監聽位址 |
| `DATA_DIR` | `./data` | 狀態根目錄 |
| `DEFAULT_QUALITY` | `1080` | 預設畫質上限 |
| `FETCH_WORKERS` | `8` | 分塊下載並行度 |
| `FFMPEG_PATH` | `ffmpeg` | ffmpeg 位置 |
| `YTDLP_PATH` | `yt-dlp` | yt-dlp 位置 |

完整設定見 `docs/spec.md` §8 與 `docs/implementation.md` §1.1。

## 用法

```
/{影片ID}              預設格式播放
/{影片ID}.m3u8         指定 HLS
/{影片ID}/720          指定畫質上限
/https://youtu.be/...  直接貼上 YouTube 網址
/h                     說明
/s                     服務狀態
```
