"""A minimal WebSocket client, enough to drive the SOBH protocol."""
import base64, json, os, socket, struct

class WS:
    def __init__(self, token, host="127.0.0.1", port=8080, path="/ws"):
        self.sock = socket.create_connection((host, port), timeout=15)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(
            f"GET {path}?token={token} HTTP/1.1\r\nHost: {host}:{port}\r\n"
            f"Upgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n".encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("connection closed during handshake")
            resp += chunk
        if b"101" not in resp.split(b"\r\n")[0]:
            raise RuntimeError("handshake failed: " + resp[:200].decode(errors="replace"))
        self.buf = resp.split(b"\r\n\r\n", 1)[1]

    def send(self, frame: dict):
        payload = json.dumps(frame).encode()
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        n = len(payload)
        if n < 126:
            header = struct.pack("!BB", 0x81, 0x80 | n)
        elif n < 65536:
            header = struct.pack("!BBH", 0x81, 0x80 | 126, n)
        else:
            header = struct.pack("!BBQ", 0x81, 0x80 | 127, n)
        self.sock.sendall(header + mask + masked)

    def _fill(self, want):
        while len(self.buf) < want:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("connection closed")
            self.buf += chunk

    def recv(self, timeout=10):
        """Returns the next text frame as a dict, or None on timeout."""
        self.sock.settimeout(timeout)
        try:
            while True:
                self._fill(2)
                b0, b1 = self.buf[0], self.buf[1]
                opcode = b0 & 0x0F
                length = b1 & 0x7F
                offset = 2
                if length == 126:
                    self._fill(4); length = struct.unpack("!H", self.buf[2:4])[0]; offset = 4
                elif length == 127:
                    self._fill(10); length = struct.unpack("!Q", self.buf[2:10])[0]; offset = 10
                self._fill(offset + length)
                data = self.buf[offset:offset + length]
                self.buf = self.buf[offset + length:]
                if opcode == 0x9:      # ping -> pong
                    continue
                if opcode == 0x8:
                    return None
                if opcode in (0x1, 0x2):
                    return json.loads(data.decode())
        except (socket.timeout, TimeoutError):
            return None

    def wait_for(self, event, timeout=12):
        """Drains frames until one matches, so heartbeats do not hide a result."""
        import time
        deadline = time.time() + timeout
        while time.time() < deadline:
            frame = self.recv(timeout=max(0.5, deadline - time.time()))
            if frame is None:
                continue
            if frame.get("event") == event:
                return frame
        return None

    def close(self):
        try: self.sock.close()
        except Exception: pass
