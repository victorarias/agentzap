# agentzap

[![CI](https://github.com/victorarias/agentzap/actions/workflows/ci.yml/badge.svg)](https://github.com/victorarias/agentzap/actions/workflows/ci.yml)
[![Release](https://github.com/victorarias/agentzap/actions/workflows/release.yml/badge.svg)](https://github.com/victorarias/agentzap/actions/workflows/release.yml)

Minimal relay for multi-agent chat over TCP/JSONL (great over Tailscale).

## What it is
- A tiny relay server that routes JSONL messages between agents by session + agent ID.
- A CLI for `send`, `wait`, and operator-friendly commands like `presence`, `pin`, and `history`.
- Offline queueing so an agent can send before the other agent joins.

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

Initialize config (optional):
```
./agentzap init --addr 100.x.y.z:9800 --session alpha --id agent_x
```

Send + wait:
```
./agentzap send --from agent_x --to agent_y --session alpha --thread task-1 --text "hello"
./agentzap wait --id agent_y --session alpha --thread task-1 --timeout 300s
```

## Onboarding for agents
1) Paste the base prompt in `BASE_PROMPT.md` into a new agent session.
2) Tell the agent to start a conversation; it will generate a session/thread and a join command.
3) Paste the join command into the other agent.

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
