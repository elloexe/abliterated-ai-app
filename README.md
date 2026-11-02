# Kestrel

Kestrel is a chat app for Windows that runs a local AI fully offline on an NVIDIA GPU. It was built
for an RTX 4060 Laptop GPU (8 GB VRAM). The engine, both models and the runtime ship with the app.
Once it is installed, there is nothing more to download and it works with no internet.

- **Main model:** Huihui Qwen3.5 9B abliterated, GGUF `i1-Q4_K_M` (5.6 GB). It writes every answer
  and makes every tool call. It is never swapped for a smaller model.
- **Helper model:** Qwen3.5 0.8B, GGUF `Q8_0` (0.8 GB), used twice (see below).
- **Engine:** llama.cpp `llama-server` b12058, Windows CUDA 12.4 build, plus the CUDA runtime DLLs.

## Install and launch

1. Open the latest [release](https://github.com/elloexe/abliterated-ai-app/releases) and download
   **every file** into one folder (Downloads is fine). The 9B model comes in three `.partNN` files,
   because GitHub caps each release file at 2 GB.
2. Double-click **`Kestrel.exe`**. On first run it:
   - finds the other files (next to the exe, in a `download\` folder or in your Downloads folder),
   - checks each one with SHA-256 against the checksums compiled into the exe,
   - joins the parts into the model and checks the joined file again,
   - unpacks llama.cpp and the CUDA DLLs into `bin\`,
   - installs to `%LOCALAPPDATA%\Programs\Kestrel` and adds Start menu and desktop shortcuts.
     You can also pick **Portable** to set up in the current folder.

   If you downloaded only `Kestrel.exe`, it offers to fetch the remaining ~7 GB from the release
   once. Interrupted downloads resume where they stopped.
3. After that, start it from the **Kestrel** shortcut. The UI opens in its own window (Microsoft Edge
   in app mode, with a private profile). Closing the window quits the app and stops both
   `llama-server` processes.

Windows SmartScreen may warn that the exe is unsigned. Click *More info*, then *Run anyway*.

If you start `Kestrel.exe` again while it is already running, it opens another window instead of a
second copy.

## How the two models work together

```
                 GPU (8 GB)                                CPU
  ┌───────────────────────────────────────┐      ┌──────────────────────┐
  │ llama-server :47822                   │      │ llama-server :47823  │
  │  9B i1-Q4_K_M  ◄── verifies ──┐       │      │  0.8B Q8_0, 4 threads│
  │  0.8B Q8_0 draft ── proposes ─┘       │      │  chat titles         │
  │  32k context, q8_0 KV cache, FA on    │      │  summaries of old    │
  └───────────────────────────────────────┘      │  turns               │
                                                 └──────────────────────┘
```

1. **Speculative decoding (GPU).** The 0.8B guesses the next 2 to 16 tokens and the 9B checks
   them in one batch. Accepted tokens are kept. At the first wrong guess, the 9B's own token is
   used instead. The output is therefore exactly what the 9B alone would write, only faster. On the
   reference laptop: 7.8 tok/s without the draft and 16 tok/s with it on battery; 28 to 33 tok/s
   plugged in, with 60 to 70% of tokens drafted. Each reply shows its speed, its token count and
   the share drafted by the 0.8B. Hover over it for acceptance details.
2. **Side jobs (CPU).** A second copy of the 0.8B runs on the CPU only (`-ngl 0`). It names new chats.
   When a chat grows past about 60% of the context window, it also folds the oldest turns into a
   running summary. These jobs run in the background, so the GPU never waits for them. If a request
   doesn't fit before a summary is ready, the oldest whole turns are left out of that request.

### Launch modes and fallback

The main server starts with:

```
llama-server -m <9B> -ngl 99 --jinja -np 1 -c 32768 -fa on -ctk q8_0 -ctv q8_0 \
  -md <0.8B> --spec-type draft-simple -ngld 99 -ctkd q8_0 -ctvd q8_0 \
  --spec-draft-n-max 16 --spec-draft-n-min 2
```

`--spec-type draft-simple` is required. Without it, llama.cpp b12058 reads the 0.8B as an MTP head
and aborts with `GGML_ASSERT ... MTP input row width`. The old `--draft-max/--draft-min` flags no
longer exist.

If a launch fails (crash, out of memory, not ready in time), Kestrel tries the next mode by itself:

| Mode | Context | Draft |
|---|---|---|
| Speculative · 32k | 32768 | yes |
| No draft · 32k | 32768 | no |
| No draft · 16k | 16384 | no |
| Safe mode · 8k | 8192 | no, no KV quantisation |

The active mode shows in the header badge and in the sidebar's status dots: green means ready,
yellow means loading and red means failed. Settings › Models has a **Restart models** button.
Settings › Log shows the live output of both servers.

## Using it

- **Sidebar:** new chat (Ctrl+Shift+O), search (titles and message text), chats grouped by date,
  rename (double-click or ⋯) and delete, model status and settings.
- **Composer:** Enter sends, Shift+Enter adds a new line. The **Thinking** chip turns Qwen's
  reasoning on or off per request (`chat_template_kwargs.enable_thinking`). The **Tools** chip turns
  function calling on or off. The send button turns into **Stop** while a reply is being generated.
- **Replies:** streaming Markdown with syntax highlighting and copy buttons on code blocks. The
  reasoning appears in a collapsible *Thought for Ns* block. Tool cards show each tool's input and
  result. You can edit and resend any of your messages, and regenerate the last reply.
- **Settings:** system prompt, sampling (temperature 0.6, top P 0.95, top K 20 by default), max
  tokens, agent steps, auto-approve, speculative decoding on/off, context size, restart models,
  live log, light and dark themes.

### Tools

OpenAI-style function calling through `llama-server --jinja`. The agent loop runs up to 10 steps per
reply. On the last step, tools are switched off so the model has to answer.

| Tool | What it does | Approval |
|---|---|---|
| `web_search` | DuckDuckGo HTML results, no API key | – |
| `fetch_url` | page → clean text (main content only, empty menu bullets removed, ~14k chars) | – |
| `run_javascript` | sandboxed JavaScript (goja, 10 s limit, no file or network access) | **ask** |
| `run_shell` | PowerShell in the workspace folder (60 s limit) | **ask** |
| `list_files`, `read_file` | inside `workspace\` only | – |
| `write_file` | inside `workspace\` only | **ask** |
| `get_datetime` | local date, time and time zone | – |

Risky tools show **Allow / Deny** buttons. Turning on *Auto-approve* in Settings skips the
question. File tools refuse absolute paths, `..` and links that lead outside `workspace\`. Only
`web_search` and `fetch_url` use the internet. Without a connection they return an error, and the
model carries on without them.

## Folder layout

```
Kestrel\
  Kestrel.exe          the app (UI, agent loop, tools, installer); no other runtime needed
  config.json          optional settings (see config.example.json)
  bin\                 llama-server.exe, ggml-cuda.dll, cudart64_12.dll, cublas…
  models\              Huihui-Qwen3.5-9B-abliterated.i1-Q4_K_M.gguf, Qwen3.5-0.8B-Q8_0.gguf
  data\chats\          one JSON file per chat
  data\logs\           app.log (and app.prev.log)
  data\window\         the app window's private Edge profile
  workspace\           the only folder the file and shell tools can touch
```

`config.json` can be edited with Notepad. A UTF-8 byte-order mark, which Notepad and PowerShell
add, is stripped before parsing. Settings changed in the app are written back to this file.

The Go program is the portable runtime. JavaScript runs in an embedded interpreter, and PowerShell
is part of Windows, so `runtime\` stays empty.

## Building from source

Needs Go 1.26 or newer (any OS can cross-compile the Windows exe):

```bash
go test ./...
```

```bash
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H=windowsgui -s -w" -o Kestrel.exe .
```

A source build has no release manifest. Put `bin\` and `models\` next to the exe by hand, or set
`KESTREL_HOME` (or `--dir`) to a folder that has them. `--no-window` serves the UI on
`http://127.0.0.1:47821/` without opening a window.

### Making a release

`.github/workflows/release.yml` builds everything on GitHub's servers. It downloads llama.cpp
b12058 (CUDA 12.4), the cudart zip and both GGUFs, and checks the GGUFs against Hugging Face's
SHA-256. It splits the 9B into 1900 MiB parts and writes `manifest.json` with every checksum. It
then compiles that manifest into `Kestrel.exe` and publishes all of it as a release. Run it from the
Actions tab (*Release* › *Run workflow*) or push a `v*` tag.

`bin/`, `models/`, `runtime/`, `data/` and `workspace/` are in `.gitignore`. Model and engine files
travel only through releases.

## Troubleshooting

- **Red dot / "Every launch mode failed":** open Settings › Log. You need an NVIDIA driver that
  supports CUDA 12.4 (driver 551.61 or newer).
- **Slow on battery:** Windows limits the GPU on battery. Plug in for full speed.
- **Port in use:** change `ui_port`, `main_port` or `util_port` in `config.json`.
- **No Edge:** Kestrel falls back to Chrome, then to your default browser. Set `browser` in
  `config.json` to choose one yourself.

## Credits

llama.cpp (MIT), marked (MIT), DOMPurify (Apache-2.0 / MPL-2.0), highlight.js (BSD-3-Clause) and
goja (MIT) are bundled under their licences (see `web/vendor/`). Model weights:
[Huihui-Qwen3.5-9B-abliterated imatrix GGUF](https://huggingface.co/mradermacher/Huihui-Qwen3.5-9B-abliterated-i1-GGUF)
by mradermacher and [Qwen3.5-0.8B GGUF](https://huggingface.co/ggml-org/Qwen3.5-0.8B-GGUF) by
ggml-org, each under its own licence. The abliterated model has had its refusal behaviour removed,
so you are responsible for how you use it.
