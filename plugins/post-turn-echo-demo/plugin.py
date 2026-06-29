#!/usr/bin/env python3
"""
Demo hook plugin for BkCrab.

On every `post_turn` hook fire, sends a fixed follow-up message back to
the same chat via the `chat.send` notification. Skeleton you can copy
for richer plugins (post-reply audio, translation, summarization, ...).

Protocol:
- JSON-RPC 2.0 over stdin/stdout, NDJSON framing.
- bkcrab -> plugin:  initialize, hook.register, hook.fire (notification).
- plugin -> bkcrab:  result for the synchronous calls, plus chat.send
                       (notification — fire-and-forget).
"""

import json
import sys


def log(msg):
    """All logs to stderr; stdout is reserved for the JSON-RPC stream."""
    print(f"[post-turn-echo-demo] {msg}", file=sys.stderr, flush=True)


def send_response(resp):
    sys.stdout.write(json.dumps(resp, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def send_notification(method, params):
    sys.stdout.write(
        json.dumps(
            {"jsonrpc": "2.0", "method": method, "params": params},
            separators=(",", ":"),
            ensure_ascii=False,
        )
        + "\n"
    )
    sys.stdout.flush()


def handle_initialize(params, req_id):
    log(f"initialize: {params}")
    send_response({"jsonrpc": "2.0", "id": req_id, "result": {"ok": True}})


def handle_hook_register(params, req_id):
    # Tell bkcrab we want to be notified on post_turn. The server will
    # invoke `hook.fire` (as a notification, async) for every agent turn
    # whose HookRegistry has us attached — see registerHookPluginsForAgent
    # in internal/gateway/userspace.go.
    send_response(
        {
            "jsonrpc": "2.0",
            "id": req_id,
            "result": {"points": ["post_turn"]},
        }
    )


def handle_hook_fire(params):
    """Notification — no response expected."""
    point = params.get("point", "")
    if point != "post_turn":
        return

    channel = params.get("channel", "")
    chat_id = params.get("chatId", "")
    account_id = params.get("accountId", "")
    agent_name = params.get("agentName", "")

    if not channel or not chat_id:
        log(f"post_turn missing routing — channel={channel!r} chatId={chat_id!r}; skipping")
        return

    # Push a fixed follow-up message into the same chat. bkcrab's
    # plugin manager turns this into a bus.OutboundMessage and hands
    # it to the channel adapter — same path the agent's own reply
    # took, so the chatter sees a second bubble.
    send_notification(
        "chat.send",
        {
            "channel": channel,
            "accountId": account_id,
            "chatId": chat_id,
            "agentId": agent_name,
            "text": f"📎 [{agent_name}] post-turn echo from plugin.",
        },
    )
    log(f"chat.send dispatched -> {channel}:{chat_id}")


def main():
    log("starting")
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError as e:
            log(f"bad json: {e}")
            continue

        method = msg.get("method", "")
        params = msg.get("params") or {}
        req_id = msg.get("id")

        if req_id is None:
            # Notification — no response.
            if method == "hook.fire":
                handle_hook_fire(params)
            elif method == "shutdown":
                log("shutdown")
                return
            else:
                log(f"unhandled notification: {method}")
            continue

        # Request — must respond.
        if method == "initialize":
            handle_initialize(params, req_id)
        elif method == "hook.register":
            handle_hook_register(params, req_id)
        elif method == "shutdown":
            send_response({"jsonrpc": "2.0", "id": req_id, "result": {"ok": True}})
            return
        else:
            send_response(
                {
                    "jsonrpc": "2.0",
                    "id": req_id,
                    "error": {"code": -32601, "message": f"method not found: {method}"},
                }
            )


if __name__ == "__main__":
    try:
        main()
    except Exception as e:  # noqa: BLE001
        log(f"crashed: {e}")
        sys.exit(1)
