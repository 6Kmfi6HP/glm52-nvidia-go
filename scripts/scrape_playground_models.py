#!/usr/bin/env python3
"""Scrape build.nvidia.com for which /v1/models entries have a chat Playground
and record their NVCF namespace + function id.

Why two sources:
  - https://integrate.api.nvidia.com/v1/models  lists EVERY model id (the
    anonymous catalog). Returns OpenAI-style {"data":[{"id":...}]}.
  - https://build.nvidia.com/{model}/playground  is the chat UI page. Next.js
    server-renders the model descriptor as *escaped* JSON inside the HTML, e.g.
        ...(\"namespace\":\"qc69jvmznzxy\",...,\"nvcfFunctionId\":\"<uuid>\")...

A model whose /playground page contains an \"nvcfFunctionId\":\"<uuid>\" is a
real chat playground model; one whose value is the literal string \"None\" or
that omits the key is either not a chat playground (no namespace) or is a
playground whose function id is resolved at runtime rather than inlined.

Known runtime-resolved function ids (not inlined in HTML) are supplied via
OVERRIDES below — verified by actually calling the playground (BoxPwnr does
this for moonshotai/kimi-k2.6).

Output (stdout): a JSON object with _meta and a sorted `models` array.
Output (stderr): summary + skipped list.
"""
import json
import re
import sys
import urllib.request
import concurrent.futures as cf

V1 = "https://integrate.api.nvidia.com/v1/models"
PAGE = "https://build.nvidia.com/{m}/playground"

# Models whose /playground HTML renders nvcfFunctionId as "None" — the real
# function id is only discoverable by driving the page at runtime. Verified
# values go here so the registry can still route them.
OVERRIDES = {
    "moonshotai/kimi-k2.6": "23d4f03a-b8a6-4adb-a183-7daa083a09cc",
}

FNID_RE = re.compile(r'"nvcfFunctionId\\?":\\?"([^"\\]{1,40})\\?"')
NS_RE = re.compile(r'"namespace\\?":\\?"([0-9a-z]+)\\?"')


def fetch(url, timeout=25, tries=2):
    last = None
    for _ in range(tries):
        try:
            req = urllib.request.Request(url, headers={"User-Agent": "Mozilla/5.0"})
            with urllib.request.urlopen(req, timeout=timeout) as r:
                return r.read().decode("utf-8", "replace")
        except Exception as e:  # noqa: BLE001
            last = e
    raise last


def probe(model):
    try:
        html = fetch(PAGE.format(m=model))
    except Exception as e:  # noqa: BLE001
        return {"model": model, "ok": False, "reason": str(e)}
    fn = FNID_RE.search(html)
    ns_m = NS_RE.search(html)
    ns = ns_m.group(1) if ns_m else ""
    slug = model.split("/", 1)[-1]

    if model in OVERRIDES:
        return {"model": model, "slug": slug, "namespace": ns or "qc69jvmznzxy",
                "function_id": OVERRIDES[model], "ok": True, "source": "override"}

    if not fn:
        return {"model": model, "ok": False, "reason": "no nvcfFunctionId key"}
    val = fn.group(1)
    if re.fullmatch(r"[0-9a-f-]{36}", val):
        return {"model": model, "slug": slug, "namespace": ns, "function_id": val,
                "ok": True, "source": "html"}
    # val == "None" (or anything non-uuid): playground exists but function id
    # is runtime-resolved and we have no override for it.
    return {"model": model, "slug": slug, "namespace": ns, "ok": False,
            "reason": f"nvcfFunctionId={val!r} (runtime-resolved, no override)"}


def main():
    raw = fetch(V1)
    ids = [m["id"] for m in json.loads(raw)["data"]]
    print(f"# {len(ids)} models from {V1}", file=sys.stderr)

    results = []
    with cf.ThreadPoolExecutor(max_workers=12) as ex:
        for r in ex.map(probe, ids):
            results.append(r)

    ok = [r for r in results if r.get("ok")]
    bad = [r for r in results if not r.get("ok")]
    ok.sort(key=lambda r: r["model"])
    nss = sorted({r["namespace"] for r in ok if r.get("namespace")})
    print(f"# {len(ok)} playground chat models / {len(results)} total", file=sys.stderr)
    print(f"# namespaces: {nss}", file=sys.stderr)
    print("# skipped:", file=sys.stderr)
    for r in bad:
        print(f"#   {r['model']}: {r.get('reason')}", file=sys.stderr)

    print(json.dumps({"_meta": {"count": len(ok), "namespaces": nss}, "models": ok},
                     indent=2))


if __name__ == "__main__":
    main()
