#!/usr/bin/env python3
"""JSON-RPC over WebSocket over the Codex daemon control socket.
usage: rpcws.py <mode> <thread_id> [text]   modes: loaded|turns|steer|inject|queue"""
import asyncio, json, os, sys, websockets
SOCK = os.path.expanduser("~/.codex/app-server-control/app-server-control.sock")
mode = sys.argv[1]; tid = sys.argv[2] if len(sys.argv) > 2 else None
text = sys.argv[3] if len(sys.argv) > 3 else "hello"
async def main():
    async with websockets.unix_connect(SOCK, uri="ws://localhost/") as ws:
        nid = 0
        async def call(method, params, timeout=20):
            nonlocal nid; nid += 1; rid = nid
            await ws.send(json.dumps({"jsonrpc": "2.0", "id": rid, "method": method, "params": params}))
            while True:
                m = json.loads(await asyncio.wait_for(ws.recv(), timeout))
                if m.get("id") == rid:
                    print(f"<- {method}:", json.dumps(m)[:1200]); return m
                print("   (notif)", json.dumps(m)[:250])
        await call("initialize", {"clientInfo": {"name": "flopwire-probe", "title": "flopwire probe", "version": "0.0.1"},
                                  "capabilities": {"experimentalApi": True}})
        await ws.send(json.dumps({"jsonrpc": "2.0", "method": "initialized"}))
        await call("thread/loaded/list", {})
        if mode in ("turns", "steer"):
            r = await call("thread/turns/list", {"threadId": tid, "limit": 1})
            data = r.get("result", {}).get("data", [])
            turn = data[0] if data else None
            if turn: print("latest turn:", turn.get("id"), turn.get("status"))
            if mode == "steer" and turn:
                await call("turn/steer", {"threadId": tid, "expectedTurnId": turn["id"],
                                          "input": [{"type": "text", "text": text, "text_elements": []}]})
        if mode == "inject":
            await call("thread/inject_items", {"threadId": tid, "items": [
                {"type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}]})
        if mode == "queue":
            await call("thread/queue/add", {"threadId": tid, "clientUserMessageId": "flopwire-probe-1",
                                            "input": [{"type": "text", "text": text, "text_elements": []}]})
asyncio.run(main())
