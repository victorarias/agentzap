# agentzap

[![CI](https://github.com/victorarias/agentzap/actions/workflows/ci.yml/badge.svg)](https://github.com/victorarias/agentzap/actions/workflows/ci.yml)
[![Release](https://github.com/victorarias/agentzap/actions/workflows/release.yml/badge.svg)](https://github.com/victorarias/agentzap/actions/workflows/release.yml)

Minimal relay for multi‑agent chat over TCP/JSONL (great over Tailscale).

## Why use it
Your agents have opinions. Opus wants to talk, Codex wants to double‑check, and you don’t want to be the human DM relay between them. This tool lets your **agents talk to each other directly** while you stay out of the loop.

I built this so I can stop manually connecting agents that own different concerns: infra vs app, client vs server, backend vs frontend, research vs implementation. Start a conversation once, copy the join command, and let them resolve the issue together.

## What it is
- A tiny relay server that routes JSONL messages between agents by session + agent ID.
- A CLI for `send`, `wait`, and operator-friendly commands like `presence`, `pin`, and `history`.
- Offline queueing so an agent can send before the other agent joins.

## What it isn’t
- Not an orchestrator: it does not launch agents or keep them polling.
- Not a scheduler: a human (or a separate orchestrator) must trigger agents to start/continue.

## Quick start

Build:
```
go build -o agentzap ./
```

Run server (default history dir: `./data/history`):
```
./agentzap server --addr 0.0.0.0:9800
```
Pins are stored under `./data/pins` (disabled if history is disabled).

## Install

From source:
```
git clone https://github.com/victorarias/agentzap
cd agentzap
go build -o agentzap ./
sudo install -m 0755 agentzap /usr/local/bin/agentzap
sudo install -m 0755 agentzap-send /usr/local/bin/agentzap-send
```
From release binaries:
1) Download the correct binary from GitHub Releases.
2) Make it executable and move it into PATH:
```
chmod +x agentzap_*
sudo install -m 0755 agentzap_<os>_<arch> /usr/local/bin/agentzap
```

Homebrew (HEAD until first stable release):
```
brew tap victorarias/agentzap
brew install --HEAD agentzap
```

## Install the agent prompt
```
agentzap prompt
```
Interactive installer that writes the base prompt to the right place:
- Codex user-level: `~/.codex/AGENTS.md`
- Claude Code user-level: `~/.claude/CLAUDE.md`
- Or project-level `AGENTS.md` / `CLAUDE.md`

Non-interactive examples:
```
agentzap prompt --target codex --scope user --force
agentzap prompt --target claude --scope user --force
agentzap prompt --target codex --scope project --project /path/to/repo --force
```

Initialize config (optional):
```
./agentzap init --addr 100.x.y.z:9800
```

Send + wait:
```
./agentzap send --from agent_x --to agent_y --session alpha --thread task-1 --text "hello"
./agentzap wait --id agent_y --session alpha --thread task-1 --timeout 300s
```

## Onboarding (agent-to-agent)
This tool is for **agents talking to each other**, not for direct user chat.
`BASE_PROMPT.md` is written for agents; this README is for humans.

1) Copy the base prompt from `BASE_PROMPT.md` into your project’s `AGENTS.md` or `CLAUDE.md`, or your user-level `AGENTS.md`/`CLAUDE.md`.
2) Ask one agent: “start a conversation about \<topic\> with other agents.”
3) The agent will output a session/thread and a join command. Copy that output and paste it into the other agent session.
4) The agents then talk to each other to resolve the issue. You can observe or intervene if needed.

## Real-world example (trimmed)

**User → Opus (infra agent):** “start a conversation about what is needed in ai‑sandbox to compile things, another agent will join”

Opus:
```
agentzap send --from opus --to all --session zap-20260118 --thread ai-sandbox-compile \
  --text "Managing ai-sandbox build deps—join this thread."
# ack: broadcast

Session: zap-20260118
Thread: ai-sandbox-compile
My ID: opus
Join:
agentzap wait --id <their-id> --session zap-20260118 --thread ai-sandbox-compile --timeout 300s
```

**Codex joins and responds:**
```
agentzap send --from codex --to opus --session zap-20260118 --thread ai-sandbox-compile \
  --text "Rust link error: rust-lld can’t find -lz. Please install zlib and ensure libz.so is on disk; add ~/.nix-profile/lib to LIBRARY_PATH/LD_LIBRARY_PATH."
# ack: delivered
```

Later, Opus replies while Codex is offline:
```
agentzap send --from opus --to codex --session zap-20260118 --thread ai-sandbox-compile \
  --text "Fixed: libz.so now present; VM back up."
# ack: queued
```

Codex returns and closes it out:
```
agentzap send --from codex --to opus --session zap-20260118 --thread ai-sandbox-compile \
  --text "All set. Rust tests pass with LIBRARY_PATH/LD_LIBRARY_PATH/PKG_CONFIG_PATH set."
# ack: delivered
```

## CLI overview

Send and wait:
```
agentzap send --from agent_x --to agent_y --session alpha --thread t1 --text "hi"
agentzap wait --id agent_y --session alpha --thread t1 --timeout 300s
```

Presence:
```
agentzap presence --session alpha
agentzap who --session alpha --verbose
```

Pins (session notes):
```
agentzap pin set --session alpha topic "glossary"
agentzap pin get --session alpha topic
agentzap pin list --session alpha
agentzap pin del --session alpha topic
```

History:
```
agentzap history --session alpha --thread t1 --last 10
agentzap history --session alpha --thread t1 --since 1737072000 --json
```

Config:
```
agentzap config show
agentzap config get addr
agentzap config set session alpha
```

Doctor:
```
agentzap doctor
agentzap status --json
```

## Output modes
- `--json` for machine-readable output
- `--verbose` for detailed output
- `--quiet` to suppress output

## Protocol (JSONL)
```
{"type":"hello","agent_id":"agent_x","session":"alpha"}
{"type":"msg","to":"agent_y","text":"hi","thread":"t1"}
{"type":"presence","session":"alpha"}
{"type":"pin_set","session":"alpha","meta":{"key":"topic","value":"glossary"}}
{"type":"history","session":"alpha","thread":"t1","meta":{"last":"10"}}
```

## Development

Format:
```
gofmt -w .
```

Test:
```
go test ./...
```

Lint:
```
golangci-lint run
```

## License
MIT (see `LICENSE`).
