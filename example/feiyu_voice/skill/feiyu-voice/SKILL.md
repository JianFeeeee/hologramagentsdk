---
name: feiyu-voice
description: 肥鱼声线：文字转语音、换声、按谱唱歌。按需插件，用前先 plugin_enable。
version: 0.1.0
---

# 肥鱼声库（feiyu voice）

这是**按需插件**：工具一开始不在清单里，第一次使用前先
`plugin_enable("feiyu_voice")`（或 `plugin_start`），工具随即出现。

## 它是什么

一个 HTTP 客户端，连的是独立部署的肥鱼声库服务（TTS / RVC 换声 / SVS 唱歌）。
插件本身**不含推理**，服务在哪台机器都行，配 `feiyu_voice.base_url` 指过去即可。

## 工具

| 工具 | 用途 | 返回 |
|---|---|---|
| `feiyu_voice_say(text, style)` | 文字 → 语音（cute / gentle） | 本地音频文件路径 |
| `feiyu_voice_sing(score, lyrics, bpm, track)` | 按谱唱（MIDI/ABC + 歌词） | 本地音频文件路径 |
| `feiyu_voice_convert(audio)` | 换声（RVC） | 本地音频文件路径 |
| `feiyu_voice_job(job_id)` | 查任务状态 | JSON |

## 拿到文件之后

**把返回的路径交给 QQ 插件发出去**：

- 短句语音条：`output_send__qq`，`type=voice`，`payload=<路径>`
- 唱歌 / 长音频：`output_send__qq`，`type=file`，`payload=<路径>`
  （语音条有格式与时长限制，几分钟的歌会被拒或听不清）

## 什么时候该用

- 该用：用户明确要听声音、要安慰、要唱一段、要念一段文稿
- 别用：普通聊天。打字更快，语音是特殊场合

## 失败怎么读

- `肥鱼声库不可达` ⇒ 服务没起或 `base_url` 配错。**不是模型问题**，别重试同一调用
- `声库任务失败: …` ⇒ 服务端推理失败（素材/谱面问题），看错误里的原因
- `上传素材未返回 id` ⇒ 素材格式不对（谱要 MIDI/ABC，换声要 WAV/FLAC）

## 依赖

服务端需要独立安装（模型权重 + torch）。CPU 也能跑：运行时明确支持
`backend: "cpu"`，TTS 走 CPU ONNX，只是 RVC/SVS 会慢很多。
