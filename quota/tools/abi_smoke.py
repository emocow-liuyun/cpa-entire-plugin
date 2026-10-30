"""Native C ABI smoke test for the Entire quota plugin.

Loads the c-shared library with ctypes, initializes ABI v1, and drives the
quota RPC family through a mock host. The mock host implements only
`host.http.do` and `host.log`; the token and the billing fixture are synthetic.

Usage: python3 abi_smoke.py <path-to-libentire-quota.so>
Exit code 0 means every assertion passed.
"""
import base64
import ctypes
import json
import sys
from ctypes import POINTER, Structure, c_char_p, c_int, c_size_t, c_uint32, c_void_p


class Buffer(Structure):
    _fields_ = [("ptr", c_void_p), ("len", c_size_t)]


# NOTE: every function pointer inside cliproxy_host_api / cliproxy_plugin_api is
# declared as a plain c_void_p. CFUNCTYPE inside a Structure field is interpreted
# by ctypes as an embedded callback, which corrupts the layout handed to C. The
# real prototypes are applied with ctypes.cast only at the call site.
HOST_CALL_FN = ctypes.CFUNCTYPE(c_int, c_void_p, c_char_p, POINTER(ctypes.c_uint8), c_size_t, POINTER(Buffer))
HOST_FREE_FN = ctypes.CFUNCTYPE(None, c_void_p, c_size_t)
PLUGIN_CALL_FN = ctypes.CFUNCTYPE(c_int, c_char_p, POINTER(ctypes.c_uint8), c_size_t, POINTER(Buffer))
PLUGIN_FREE_FN = ctypes.CFUNCTYPE(None, c_void_p, c_size_t)
PLUGIN_SHUTDOWN_FN = ctypes.CFUNCTYPE(None)

HOST_CALL = c_void_p
HOST_FREE = c_void_p
PLUGIN_CALL = c_void_p
PLUGIN_FREE = c_void_p
PLUGIN_SHUTDOWN = c_void_p


class HostApi(Structure):
    _fields_ = [("abi_version", c_uint32), ("host_ctx", c_void_p), ("call", HOST_CALL), ("free_buffer", HOST_FREE)]


class PluginApi(Structure):
    _fields_ = [
        ("abi_version", c_uint32),
        ("call", PLUGIN_CALL),
        ("free_buffer", PLUGIN_FREE),
        ("shutdown", PLUGIN_SHUTDOWN),
    ]


SYNTHETIC_TOKEN = "entire-synthetic-token-abc123456"
BILLING_DOC = {
    "state": "active",
    "currency": "usd",
    "balance": {
        "total_cents": 7500,
        "trial_cents": 5000,
        "granted_cents": 0,
        "purchased_cents": 2500,
        "currency": "usd",
    },
    "automatic_top_up": {"enabled": True, "threshold_cents": 1000, "target_cents": 5000, "currency": "usd"},
    "overage": {"amount_cents": 0, "currency": "usd"},
    "runs_paused": {"paused": False},
    "trial": {"state": "active", "currency": "usd"},
}


class MockHost:
    """In-process host that answers host.http.do / host.log."""

    def __init__(self):
        self.requests = []
        self.logs = []
        self.billing_status = 200
        self.billing_body = json.dumps(BILLING_DOC).encode()
        self._keep = []
        self.call_cb = ctypes.cast(HOST_CALL_FN(self._call), c_void_p)
        self.free_cb = ctypes.cast(HOST_FREE_FN(self._free), c_void_p)
        self.api = HostApi(abi_version=1, host_ctx=None, call=self.call_cb, free_buffer=self.free_cb)

    def _free(self, ptr, length):
        pass  # buffers are kept alive below; the process is short-lived

    def _call(self, host_ctx, method, request, request_len, response):
        name = method.decode() if method else ""
        payload = ctypes.string_at(request, request_len) if request_len else b""
        if name == "host.http.do":
            req = json.loads(payload or b"{}")
            self.requests.append(req)
            # pluginapi.HTTPResponse.Body is a Go []byte, which marshals to base64.
            body = json.dumps(
                {
                    "StatusCode": self.billing_status,
                    "Headers": {"Content-Type": ["application/json"]},
                    "Body": base64.b64encode(self.billing_body).decode(),
                }
            ).encode()
            self._respond(response, {"ok": True, "result": json.loads(body)})
            return 0
        if name == "host.log":
            self.logs.append(json.loads(payload or b"{}"))
            self._respond(response, {"ok": True, "result": {}})
            return 0
        self._respond(response, {"ok": False, "error": {"code": "unsupported", "message": name}})
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
    # The host marshals Go []byte fields as base64, so config_yaml travels that
    # way too; mirror the real wire format instead of sending raw YAML.
    raw = json.dumps(payload or {}).encode()
    request = (ctypes.c_uint8 * len(raw)).from_buffer_copy(raw)
    out = Buffer()
    call_fn = ctypes.cast(plugin_api.call, PLUGIN_CALL_FN)
    rc = call_fn(method.encode(), request, c_size_t(len(raw)), ctypes.byref(out))
    body = ctypes.string_at(out.ptr, out.len).decode() if out.ptr else ""
    if out.ptr:
        lib.cliproxyPluginFree(out.ptr, out.len)
    return rc, json.loads(body) if body else {}


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
    # Tokens are per-credential: they ride on quota.fetch via auth metadata,
    # never in the plugin config (a global token would override every
    # credential's own token on multi-credential installs).
    config_yaml = 'enabled: true\npriority: 1\n'
    rc, env = rpc(
        lib,
        plugin_api,
        "plugin.register",
        {"config_yaml": base64.b64encode(config_yaml.encode()).decode(), "schema_version": 8},
    )
    results.append(("plugin.register", rc, env))
    assert env["ok"], env
    assert env["result"]["capabilities"] == {"quota_provider": True, "auth_provider": True, "model_provider": True}, env["result"]["capabilities"]

    rc, env = rpc(lib, plugin_api, "quota.identifier")
    results.append(("quota.identifier", rc, env))
    assert env["result"]["identifier"] == "entire", env

    rc, env = rpc(lib, plugin_api, "quota.describe", {})
    results.append(("quota.describe", rc, env))
    assert env["result"]["supports_reset"] is False, env["result"]

    fetch_req = lambda cb, metadata=True: {
        "auth_index": "1", "auth_id": "a", "provider": "entire", "host_callback_id": cb,
        **({"metadata": {"entire_token": SYNTHETIC_TOKEN}} if metadata else {}),
    }
    rc, env = rpc(lib, plugin_api, "quota.fetch", fetch_req("7"))
    results.append(("quota.fetch (balance)", rc, env))
    assert env["ok"], env
    summary = {m["key"]: m["value"] for m in env["result"]["summary"]}
    assert summary["entire_credit_balance"] == 75.0, summary
    assert host.requests and host.requests[0]["url"] == "https://us.console.entire.io/api/v1/me/billing", host.requests
    assert host.requests[0]["headers"]["Authorization"] == ["Bearer " + SYNTHETIC_TOKEN], host.requests[0]["headers"]

    host.billing_body = json.dumps(
        {
            "state": "active",
            "balance": {"total_cents": 0, "currency": "usd"},
            "automatic_top_up": {"enabled": True, "threshold_cents": 1000, "target_cents": 5000},
            "overage": {"amount_cents": 0},
            "runs_paused": {"paused": False},
        }
    ).encode()
    rc, env = rpc(lib, plugin_api, "quota.fetch", fetch_req("8"))
    results.append(("quota.fetch (zero)", rc, env))
    assert env["ok"], env
    assert env["result"]["summary"][0]["value"] == 0, env["result"]["summary"]

    host.billing_body = json.dumps({"state": "active", "unavailable": ["balance"]}).encode()
    rc, env = rpc(lib, plugin_api, "quota.fetch", fetch_req("9"))
    results.append(("quota.fetch (unreadable)", rc, env))
    assert rc == 1 and not env["ok"], env
    assert env["error"]["code"] == "entire_billing_unreadable", env["error"]

    host.billing_status = 401
    rc, env = rpc(lib, plugin_api, "quota.fetch", fetch_req("10"))
    results.append(("quota.fetch (401)", rc, env))
    assert rc == 1 and env["error"]["http_status"] == 401, env
    host.billing_status = 200

    rc, env = rpc(lib, plugin_api, "quota.reset", {})
    results.append(("quota.reset", rc, env))
    assert env["result"]["success"] is False, env["result"]

    rc, env = rpc(lib, plugin_api, "executor.execute", {})
    results.append(("executor.execute (rejected)", rc, env))
    assert rc == 1 and env["error"]["code"] == "unknown_method", env

    # Credential hygiene: neither a host log nor an error envelope may carry the token.
    for name, _, envelope in results:
        assert SYNTHETIC_TOKEN not in json.dumps(envelope), f"token leaked in {name}"
    for entry in host.logs:
        assert SYNTHETIC_TOKEN not in json.dumps(entry), f"token leaked in host.log: {entry}"

    # Headers are only allowed to carry the token on the upstream billing request
    # itself; nothing else may echo it.
    for entry in host.requests:
        if SYNTHETIC_TOKEN in json.dumps(entry.get("headers", {})):
            assert entry["url"].startswith("https://us.console.entire.io/api/v1/"), entry["url"]
            assert entry["headers"]["Authorization"] == ["Bearer " + SYNTHETIC_TOKEN], entry["headers"]

    print(json.dumps({"abi": 1, "results": [{"method": n, "rc": rc, "ok": body.get("ok")} for n, rc, body in results]}, indent=1))
    print(f"host.http.do calls: {len(host.requests)}, host.log calls: {len(host.logs)}, token leaks: 0")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
