#!/usr/bin/env python3
"""Subscribe over graphql-transport-ws and print the first update (standard library only).

  ws-subscribe.py URL TOKEN QUERY [TIMEOUT_SECONDS]

URL is the GraphQL endpoint as ws:// or wss://. Prints "ready" once the
server acknowledged the connection and the subscription was sent, then the
first "next" payload as JSON (exit 0). If the server closes the connection
first it prints "closed <code> <reason>" (exit 2); on a timeout it prints
"timeout" (exit 1). verify.sh uses it for the live-update checks.
"""
import base64
import json
import os
import socket
import ssl
import struct
import sys
import time
from urllib.parse import urlsplit

TEXT, CLOSE, PING, PONG = 0x1, 0x8, 0x9, 0xA


def connect(url, timeout):
    u = urlsplit(url)
    secure = u.scheme == "wss"
    port = u.port or (443 if secure else 80)
    sock = socket.create_connection((u.hostname, port), timeout=timeout)
    if secure:
        sock = ssl.create_default_context().wrap_socket(sock, server_hostname=u.hostname)
    key = base64.b64encode(os.urandom(16)).decode()
    path = (u.path or "/") + (("?" + u.query) if u.query else "")
    sock.sendall((
        f"GET {path} HTTP/1.1\r\nHost: {u.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
        f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
        "Sec-WebSocket-Protocol: graphql-transport-ws\r\n\r\n").encode())
    head = b""
    while b"\r\n\r\n" not in head:
        chunk = sock.recv(1)
        if not chunk:
            raise ConnectionError("connection closed during the handshake")
        head += chunk
    status = head.split(b"\r\n", 1)[0].decode()
    if " 101 " not in status + " ":
        raise ConnectionError(f"handshake refused: {status}")
    return sock


def send(sock, opcode, payload=b""):
    mask = os.urandom(4)  # clients must mask every frame
    n = len(payload)
    if n < 126:
        header = struct.pack("!BB", 0x80 | opcode, 0x80 | n)
    elif n < 1 << 16:
        header = struct.pack("!BBH", 0x80 | opcode, 0x80 | 126, n)
    else:
        header = struct.pack("!BBQ", 0x80 | opcode, 0x80 | 127, n)
    sock.sendall(header + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))


def send_json(sock, msg):
    send(sock, TEXT, json.dumps(msg).encode())


def read_exact(sock, n):
    buf = b""
    while len(buf) < n:
        chunk = sock.recv(n - len(buf))
        if not chunk:
            raise ConnectionError("connection closed")
        buf += chunk
    return buf


def read_frame(sock):
    b0, b1 = read_exact(sock, 2)
    n = b1 & 0x7F
    if n == 126:
        n = struct.unpack("!H", read_exact(sock, 2))[0]
    elif n == 127:
        n = struct.unpack("!Q", read_exact(sock, 8))[0]
    return b0 & 0x0F, read_exact(sock, n)


def messages(sock):
    """Yields protocol messages; answers pings; returns (code, reason) on close."""
    while True:
        opcode, data = read_frame(sock)
        if opcode == PING:
            send(sock, PONG, data)
        elif opcode == CLOSE:
            code = struct.unpack("!H", data[:2])[0] if len(data) >= 2 else 1005
            return code, data[2:].decode(errors="replace")
        elif opcode == TEXT:
            msg = json.loads(data)
            if msg.get("type") == "ping":
                send_json(sock, {"type": "pong"})
                continue
            yield msg


def run(url, token, query, timeout):
    sock = connect(url, timeout)
    deadline = time.monotonic() + timeout
    send_json(sock, {"type": "connection_init", "payload": {"Authorization": f"Bearer {token}"}})
    stream = messages(sock)
    try:
        while True:
            sock.settimeout(max(0.1, deadline - time.monotonic()))
            msg = next(stream)
            if msg.get("type") == "connection_ack":
                send_json(sock, {"id": "1", "type": "subscribe", "payload": {"query": query}})
                print("ready", flush=True)
            elif msg.get("type") in ("next", "error"):
                print(json.dumps(msg.get("payload")), flush=True)
                return 0
    except StopIteration as stop:
        code, reason = stop.value
        print(f"closed {code} {reason}", flush=True)
        return 2
    except ConnectionError as err:
        print(f"closed 1006 {err}", flush=True)
        return 2
    except (socket.timeout, TimeoutError):
        print("timeout", flush=True)
        return 1


if __name__ == "__main__":
    if len(sys.argv) not in (4, 5):
        sys.exit(__doc__)
    sys.exit(run(sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4]) if len(sys.argv) == 5 else 30))
