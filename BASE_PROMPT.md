# Base Prompt for Agents

Paste this into a new agent session to enable `agentzap` relay chat.

---
You can chat with other agents via `agentzap` relay. Messages are queued for offline agents and delivered when they join.

If I ask you to “start a conversation” or “start a new conversation so another agent can join,” you must:
1) Create a new session and thread (or reuse the most recent active one if I said “continue”).
2) Tell me the session, thread, and your agent ID.
3) Send an opening message to the target agent (or `all`) via `agentzap send`.
4) Provide a short, copy‑paste command for the other agent to join immediately.
5) Immediately wait for a response.

Join (register) to a session:
- One‑shot wait (also registers):
```
agentzap wait --id <your_id> --session <session> --thread <thread> --timeout 300s
```
- Or persistent client:
```
agentzap client --id <your_id> --session <session>
```

Send a message:
```
agentzap send --from <your_id> --to <other_id|all> --session <session> --thread <thread> --text "<message>"
```

ID / Session / Thread selection (auto):
- If I explicitly provide `id`, `session`, or `thread`, use those exactly.
- Otherwise:
  - **session**: reuse the most recent active session you used with me; if none, use `zap-<YYYYMMDD>`.
  - **thread**: if this is a continuation, reuse the last thread with the same counterpart; otherwise generate `thread-<short-rand>` and mention it in your first message.
  - **agent id**: choose a stable short id for this session (e.g., `opus`, `codex`, `agent-x`). Reuse it for all messages in the same session.

Rules:
- Use the same `session` for everyone.
- Use the same `thread` when expecting a reply.
- If you generate a new session/thread, state it once in your first relay message.
- If you hit “target not online,” tell me the exact join command for the other agent.
- After sending any message, immediately run `agentzap wait`.
- The relay queues messages for offline agents, so you can send first; the other agent will receive on join.
- If you need machine-readable output, use `--json`.

If relay address is configured in `~/.agentzap/config.yaml`, omit `--addr`.
---
