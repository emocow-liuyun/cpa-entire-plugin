"""Native C ABI smoke test for the Entire executor plugin.

Loads the c-shared library with ctypes, initializes ABI v1, and drives the
executor RPC family through a mock host. The mock host implements only
`host.log`; the token and every payload below are synthetic, and no network
request is ever made.

Usage: python3 abi_smoke.py <path-to-libentire_executor.so>
Exit code 0 means every assertion passed.
"""
import base64
import ctypes
import json
import sys
from ctypes import POINTER, Structure, c_char_p, c_int, c_size_t, c_uint32, c_void_p

SYNTHETIC_TOKEN = "entire-synthetic-token-abc123456"
MODEL = "entire-review-review"


class Buffer(Structure):
    _fields_ = [("ptr", c_void_p), ("len", c_size_t)]


# Every function pointer inside cliproxy_host_api / cliproxy_plugin_api is held
# as a plain c_void_p. CFUNCTYPE inside a Structure field is interpreted by
# ctypes as an embedded callback and corrupts the layout handed to C; the real
# prototypes are applied with ctypes.cast at the call site instead.
HOST_CALL_FN = ctypes.CFUNCTYPE(c_int, c_void_p, c_char_p, POINTER(ctypes.c_uint8), c_size_t, POINTER(Buffer))
HOST_FREE_FN = ctypes.CFUNCTYPE(None, c_void_p, c_size_t)
PLUGIN_CALL_FN = ctypes.CFUNCTYPE(c_int, c_char_p, POINTER(ctypes.c_uint8), c_size_t, POINTER(Buffer))


class HostApi(Structure):
    _fields_ = [
        ("abi_version", c_uint32),
        ("host_ctx", c_void_p),
        ("call", c_void_p),
        ("free_buffer", c_void_p),
    ]


class PluginApi(Structure):
    _fields_ = [
        ("abi_version", c_uint32),
        ("call", c_void_p),
        ("free_buffer", c_void_p),
        ("shutdown", c_void_p),
    ]


class MockHost:
    """In-process host answering host.log and refusing everything else."""

    def __init__(self):
        self.logs = []
        self.other_calls = []
        self._keep = []
        self.call_cb = ctypes.cast(HOST_CALL_FN(self._call), c_void_p)
        self.free_cb = ctypes.cast(HOST_FREE_FN(self._free), c_void_p)
        self.api = HostApi(abi_version=1, host_ctx=None, call=self.call_cb, free_buffer=self.free_cb)

    def _free(self, ptr, length):
        pass  # buffers stay alive below; the process is short-lived

    def _call(self, host_ctx, method, request, request_len, response):
        name = method.decode() if method else ""
        payload = ctypes.string_at(request, request_len) if request_len else b""
        if name == "host.log":
            self.logs.append(json.loads(payload or b"{}"))
            self._respond(response, {"ok": True, "result": {}})
            return 0
        # Nothing else is expected: this executor must never reach the network.
        self.other_calls.append((name, payload))
        self._respond(response, {"ok": False, "error": {"code": "unexpected", "message": name}})
        return 1

    def _respond(self, response, envelope):
        raw = json.dumps(envelope).encode()
        buf = ctypes.create_string_buffer(raw, len(raw))
        self._keep.append(buf)
        response.contents.ptr = ctypes.cast(buf, c_void_p)
        response.contents.len = len(raw)


def load(path):
    lib = ctypes.CDLL(path)
    init = lib.cliproxy_plugin_init
    init.argtypes = [POINTER(HostApi), POINTER(PluginApi)]
    init.restype = c_int
    return lib, init


def rpc(lib, plugin_api, method, payload=None):
    raw = json.dumps(payload or {}).encode()
    request = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw)
    out = Buffer()
    call_fn = ctypes.cast(plugin_api.call, PLUGIN_CALL_FN)
    rc = call_fn(method.encode(), request, c_size_t(len(raw)), ctypes.byref(out))
    body = ctypes.string_at(out.ptr, out.len).decode() if out.ptr else ""
    if out.ptr:
        lib.cliproxyPluginFree(out.ptr, out.len)
    return rc, (json.loads(body) if body else {})


def b64(text):
    return base64.b64encode(text.encode()).decode()


def main(argv):
    if len(argv) != 2:
        print("usage: abi_smoke.py <path-to-.so>")
        return 2

    host = MockHost()
    lib, init = load(argv[1])
    plugin_api = PluginApi()
    rc = init(ctypes.byref(host.api), ctypes.byref(plugin_api))
    assert rc == 0, f"init rc={rc}"
    assert plugin_api.abi_version == 1, f"abi_version={plugin_api.abi_version}"

    results = []

    # 1. Registration: executor capability only, gates documented as false.
    config_yaml = (
        "enabled: true\n"
        "priority: 1\n"
        f'token: "{SYNTHETIC_TOKEN}"\n'
    )
    rc, env = rpc(lib, plugin_api, "plugin.register",
                  {"config_yaml": b64(config_yaml), "schema_version": 8})
    results.append(("plugin.register", rc, env))
    assert env["ok"], env
    caps = env["result"]["capabilities"]
    assert caps["executor"] is True, caps
    assert caps["quota_provider"] is False, caps
    assert caps["executor_input_formats"] == ["chat-completions"], caps
    assert caps["executor_output_formats"] == ["chat-completions"], caps
    fields = {f["Name"]: f for f in env["result"]["metadata"]["ConfigFields"]}
    for gate in ("execution-enabled", "auth-subject-verified",
                 "billing-limit-enforced", "allow-unbounded-spend"):
        assert fields[gate]["Default"] is False, fields[gate]
    # Nothing outside host.log was called during registration.
    assert host.other_calls == [], host.other_calls

    # 2. Identifier and catalog.
    rc, env = rpc(lib, plugin_api, "executor.identifier")
    results.append(("executor.identifier", rc, env))
    assert env["result"]["identifier"] == "entire-agent", env

    rc, env = rpc(lib, plugin_api, "model.register", {"Plugin": {"Name": "cpa-entire-executor"}})
    results.append(("model.register", rc, env))
    assert env["ok"] and env["result"]["Provider"] == "entire-agent", env
    assert all(m["ID"].startswith("entire-review-") for m in env["result"]["Models"]), env

    payload = b64(json.dumps({
        "model": MODEL,
        "messages": [{"role": "user", "content": "review the diff"}],
    }))

    # 3. Fail-closed default: execute returns 503 with no content.
    rc, env = rpc(lib, plugin_api, "executor.execute", {
        "Model": MODEL, "Payload": payload, "host_callback_id": "1",
    })
    results.append(("executor.execute (fail-closed)", rc, env))
    assert not env["ok"], env
    assert env["error"]["http_status"] == 503, env["error"]
    assert env["error"]["code"] == "entire_execution_disabled", env["error"]

    # 4. Streaming is refused, not faked.
    rc, env = rpc(lib, plugin_api, "executor.execute_stream", {"Model": MODEL})
    results.append(("executor.execute_stream", rc, env))
    assert rc == 1 and not env["ok"], env
    assert env["error"]["code"] == "entire_stream_unsupported", env["error"]
    assert env["error"]["http_status"] == 400, env["error"]

    # 5. Tools / images / stream flags are refused before any gate is consulted.
    for name, body, want in [
        ("tools", {"model": MODEL,
                   "messages": [{"role": "user", "content": "x"}],
                   "tools": [{"type": "function", "function": {"name": "f"}}]},
         "entire_tools_unsupported"),
        ("image", {"model": MODEL,
                   "messages": [{"role": "user", "content": [
                       {"type": "text", "text": "look"},
                       {"type": "image_url", "image_url": {"url": "https://x.invalid/a.png"}}]}]},
         "entire_image_unsupported"),
        ("stream", {"model": MODEL, "stream": True,
                    "messages": [{"role": "user", "content": "x"}]},
         "entire_stream_unsupported"),
    ]:
        rc, env = rpc(lib, plugin_api, "executor.execute", {
            "Model": MODEL, "Payload": b64(json.dumps(body)), "host_callback_id": "1",
        })
        results.append((f"executor.execute (reject {name})", rc, env))
        assert not env["ok"] and env["error"]["code"] == want, (name, env)
        assert env["error"]["http_status"] == 400, env["error"]

    # 6. Models outside the namespace never reach a gate.
    rc, env = rpc(lib, plugin_api, "executor.execute", {
        "Model": "gpt-4o", "Payload": payload, "host_callback_id": "1",
    })
    results.append(("executor.execute (foreign model)", rc, env))
    assert not env["ok"] and env["error"]["code"] == "entire_model_unsupported", env

    # 7. All gates open: still 503 because no dispatcher is installed.
    gates_yaml = (
        "enabled: true\n"
        f'token: "{SYNTHETIC_TOKEN}"\n'
        "execution-enabled: true\n"
        "auth-subject-verified: true\n"
        "billing-limit-enforced: true\n"
    )
    rc, env = rpc(lib, plugin_api, "plugin.reconfigure",
                  {"config_yaml": b64(gates_yaml), "schema_version": 8})
    results.append(("plugin.reconfigure (three gates, unbounded false)", rc, env))
    assert env["ok"], env
    rc, env = rpc(lib, plugin_api, "executor.execute", {
        "Model": MODEL, "Payload": payload, "host_callback_id": "2",
    })
    results.append(("executor.execute (unbounded spend not acknowledged)", rc, env))
    assert not env["ok"] and env["error"]["code"] == "entire_billing_limit_unavailable", env
    assert env["error"]["http_status"] == 503, env

    gates_yaml += "allow-unbounded-spend: true\nproject-host: gh\nproject: acme/widgets\n"
    gates_yaml += "agent-id: agent-1\ntrail-id: trail-1\nchange-id: change-1\n"
    rc, env = rpc(lib, plugin_api, "plugin.reconfigure",
                  {"config_yaml": b64(gates_yaml), "schema_version": 8})
    results.append(("plugin.reconfigure (all gates)", rc, env))
    assert env["ok"], env
    rc, env = rpc(lib, plugin_api, "executor.execute", {
        "Model": MODEL, "Payload": payload, "host_callback_id": "3",
    })
    results.append(("executor.execute (all gates, no dispatcher)", rc, env))
    assert not env["ok"], env
    assert env["error"]["code"] == "entire_real_execution_not_implemented", env["error"]
    assert env["error"]["http_status"] == 503, env["error"]

    # 8. auth.parse recognises an Entire bearer and normalizes it.
    rc, env = rpc(lib, plugin_api, "auth.parse", {
        "Provider": "entire-agent",
        "FileName": "entire.json",
        "RawJSON": b64(json.dumps({"entire_token": "Bearer " + SYNTHETIC_TOKEN})),
    })
    results.append(("auth.parse", rc, env))
    assert env["ok"] and env["result"]["Handled"] is True, env
    stored = json.loads(base64.b64decode(env["result"]["Auth"]["StorageJSON"]))
    assert stored["entire_token"] == SYNTHETIC_TOKEN, stored

    # 9. count_tokens is local: no upstream, no dispatcher.
    rc, env = rpc(lib, plugin_api, "executor.count_tokens", {"Model": MODEL, "Payload": payload})
    results.append(("executor.count_tokens", rc, env))
    assert env["ok"] and env["result"]["total_tokens"] > 0, env

    # 10. The token never appears in any response body or host log line.
    for name, _, env in results:
        body = json.dumps(env, ensure_ascii=False)
        assert SYNTHETIC_TOKEN not in body, f"token leaked in {name}: {body}"
    for entry in host.logs:
        assert SYNTHETIC_TOKEN not in json.dumps(entry, ensure_ascii=False), entry

    # 11. Only host.log was ever called: no network path was touched.
    assert host.other_calls == [], host.other_calls

    for name, rc, env in results:
        status = env.get("error", {}).get("http_status", 200) if isinstance(env, dict) else "?"
        print(f"  [{'ok' if rc == 0 else 'err'}] {name} -> {status}")
    print("ABI v1 smoke test: all assertions passed")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
