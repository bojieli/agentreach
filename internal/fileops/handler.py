# reach tier-2 handler.
#
# This program is never written to the target's disk. reach base64-encodes it,
# sends it as the first line of stdin, and a one-line bootstrap execs it; the
# handler then speaks a framed protocol over the *rest* of stdin. It exists only
# in the memory of a process that dies with the session, which is what lets this
# tier be both batched and trace-free.
#
# Framing, both directions:
#
#     uint32 header_length | header JSON (utf-8) | uint32 payload_length | payload
#
# File content travels in the raw payload, never inside the JSON. That keeps
# binary files binary — no base64 expansion, no encoding that could mangle NUL
# bytes or invalid UTF-8 — and it is the whole performance argument for this
# tier over tier 0.
#
# Only the Python standard library is used, and no module outside it is
# imported, because the premise of this tier is a target with nothing installed
# on it.

import hashlib
import json
import os
import struct
import sys

CHUNK = 1 << 20


def _read_exactly(stream, n):
    buf = bytearray()
    while len(buf) < n:
        part = stream.read(n - len(buf))
        if not part:
            return None
        buf.extend(part)
    return bytes(buf)


def read_frame(stream):
    head = _read_exactly(stream, 4)
    if head is None:
        return None, None
    (hlen,) = struct.unpack(">I", head)
    header = _read_exactly(stream, hlen)
    if header is None:
        return None, None
    plen_raw = _read_exactly(stream, 4)
    if plen_raw is None:
        return None, None
    (plen,) = struct.unpack(">I", plen_raw)
    payload = b"" if plen == 0 else _read_exactly(stream, plen)
    if payload is None:
        return None, None
    return json.loads(header.decode("utf-8")), payload


def write_frame(stream, header, payload=b""):
    body = json.dumps(header).encode("utf-8")
    stream.write(struct.pack(">I", len(body)))
    stream.write(body)
    stream.write(struct.pack(">I", len(payload)))
    if payload:
        stream.write(payload)
    stream.flush()


def file_info(path, st, name=None):
    mode = st.st_mode
    return {
        "name": name if name is not None else os.path.basename(path),
        "path": path,
        "size": st.st_size,
        "mode": mode & 0o7777,
        "mtime": int(st.st_mtime),
        "is_dir": (mode & 0o170000) == 0o040000,
        "is_link": (mode & 0o170000) == 0o120000,
    }


def op_read(req, _payload):
    path = req["path"]
    off = int(req.get("offset", 0) or 0)
    limit = int(req.get("limit", 0) or 0)
    with open(path, "rb") as fh:
        if off:
            fh.seek(off)
        if limit > 0:
            data = _read_exactly_file(fh, limit)
        else:
            data = fh.read()
    return {}, data


def _read_exactly_file(fh, limit):
    out = bytearray()
    while len(out) < limit:
        part = fh.read(min(CHUNK, limit - len(out)))
        if not part:
            break
        out.extend(part)
    return bytes(out)


def op_write(req, payload):
    # Same-directory temporary plus rename: a reader sees the old file or the
    # new one, never a partial one, and a failed write leaves the original
    # untouched. os.replace is atomic within a filesystem.
    path = req["path"]
    mode = int(req.get("mode", 0o644) or 0o644)
    directory = os.path.dirname(path) or "."
    # The `.reach.tmp.` prefix is a contract shared with every other tier, not a
    # local choice: anything an interrupted write leaves behind has to be
    # identifiable as reach's, and the conformance suite proves no tier leaves
    # debris by looking for exactly this prefix. This handler spelled it
    # `.waldo.tmp.` — reach's former name — so its leftovers were unattributable
    # and that assertion could never fail for the tier reach negotiates by
    # default. See internal/reach/tempfile.go, which states the contract.
    tmp = os.path.join(directory, ".reach.tmp.%d.%d" % (os.getpid(), req.get("id", 0)))
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
        try:
            written = 0
            while written < len(payload):
                written += os.write(fd, payload[written : written + CHUNK])
        finally:
            os.close(fd)
        os.chmod(tmp, mode)
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise
    return {"bytes": len(payload)}, b""


def op_stat(req, _payload):
    path = req["path"]
    st = os.lstat(path)
    info = file_info(path, st)
    if info["is_link"]:
        try:
            info["link_target"] = os.readlink(path)
        except OSError:
            pass
    return {"info": info}, b""


def op_list(req, _payload):
    path = req["path"]
    out = []
    with os.scandir(path) as it:
        for entry in it:
            try:
                st = entry.stat(follow_symlinks=False)
            except OSError:
                continue
            out.append(file_info(os.path.join(path, entry.name), st, entry.name))
    return {"entries": out}, b""


def op_mkdir(req, _payload):
    mode = int(req.get("mode", 0o755) or 0o755)
    os.makedirs(req["path"], mode=mode, exist_ok=True)
    return {}, b""


def op_remove(req, _payload):
    path = req["path"]
    if req.get("recursive"):
        _rmtree(path)
        return {}, b""
    if os.path.isdir(path) and not os.path.islink(path):
        os.rmdir(path)
    else:
        os.unlink(path)
    return {}, b""


def _rmtree(path):
    # shutil is stdlib, but walking explicitly keeps the handler's imports to
    # the minimum a hardened or stripped Python still has.
    if os.path.islink(path) or not os.path.isdir(path):
        os.unlink(path)
        return
    for name in os.listdir(path):
        _rmtree(os.path.join(path, name))
    os.rmdir(path)


def op_rename(req, _payload):
    os.replace(req["from"], req["to"])
    return {}, b""


def op_hash(req, _payload):
    digest = hashlib.sha256()
    with open(req["path"], "rb") as fh:
        while True:
            part = fh.read(CHUNK)
            if not part:
                break
            digest.update(part)
    return {"digest": digest.hexdigest()}, b""


def op_ping(req, _payload):
    return {"version": req.get("version", ""), "python": sys.version.split()[0]}, b""


OPS = {
    "read": op_read,
    "write": op_write,
    "stat": op_stat,
    "list": op_list,
    "mkdir": op_mkdir,
    "remove": op_remove,
    "rename": op_rename,
    "hash": op_hash,
    "ping": op_ping,
}


def classify(exc):
    if isinstance(exc, FileNotFoundError):
        return "notfound"
    if isinstance(exc, PermissionError):
        return "permission"
    if isinstance(exc, NotADirectoryError):
        return "notdir"
    if isinstance(exc, IsADirectoryError):
        return "isdir"
    return "error"


def main():
    stdin = sys.stdin.buffer
    stdout = sys.stdout.buffer
    while True:
        req, payload = read_frame(stdin)
        if req is None:
            return 0
        rid = req.get("id", 0)
        handler = OPS.get(req.get("op"))
        if handler is None:
            write_frame(stdout, {"id": rid, "ok": False, "kind": "error",
                                 "error": "unknown op %r" % (req.get("op"),)})
            continue
        try:
            header, out = handler(req, payload)
        except BaseException as exc:  # noqa: BLE001 - every failure is a value
            # A handler that dies takes the whole session's file access with it,
            # so every exception becomes a response the client can reason about.
            write_frame(stdout, {"id": rid, "ok": False, "kind": classify(exc),
                                 "error": "%s: %s" % (type(exc).__name__, exc)})
            continue
        header["id"] = rid
        header["ok"] = True
        write_frame(stdout, header, out)


if __name__ == "__main__":
    sys.exit(main() or 0)
