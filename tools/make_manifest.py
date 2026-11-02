#!/usr/bin/env python3
"""Write the release manifest that Kestrel.exe embeds and checks files against.

Usage:
  make_manifest.py --dist DIR --tag TAG --repo OWNER/NAME \
      --main MODEL.gguf --helper MODEL.gguf --zip A.zip --zip B.zip > manifest.json

The main model is expected as MODEL.gguf.part01, .part02, ... in DIR (split so
each file stays under GitHub's 2 GiB release asset limit).
"""
import argparse
import hashlib
import json
import os
import sys


def sha256(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def part(dist, name):
    p = os.path.join(dist, name)
    return {"name": name, "size": os.path.getsize(p), "sha256": sha256(p)}


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dist", required=True)
    ap.add_argument("--tag", required=True)
    ap.add_argument("--repo", required=True)
    ap.add_argument("--main", required=True)
    ap.add_argument("--helper", required=True)
    ap.add_argument("--zip", action="append", default=[])
    a = ap.parse_args()

    limit = 2 * 1024**3
    items = []

    parts = sorted(n for n in os.listdir(a.dist) if n.startswith(a.main + ".part"))
    if not parts:
        sys.exit(f"no {a.main}.partNN files in {a.dist}")
    whole = hashlib.sha256()
    size = 0
    plist = []
    for n in parts:
        p = os.path.join(a.dist, n)
        h = hashlib.sha256()
        with open(p, "rb") as f:
            for chunk in iter(lambda: f.read(8 << 20), b""):
                h.update(chunk)
                whole.update(chunk)
        s = os.path.getsize(p)
        if s >= limit:
            sys.exit(f"{n} is {s} bytes, over the 2 GiB release asset limit")
        size += s
        plist.append({"name": n, "size": s, "sha256": h.hexdigest()})
    items.append({"id": "main-model", "kind": "model", "target": a.main,
                  "size": size, "sha256": whole.hexdigest(), "parts": plist})

    hp = part(a.dist, a.helper)
    items.append({"id": "helper-model", "kind": "model", "target": a.helper,
                  "size": hp["size"], "sha256": hp["sha256"], "parts": [hp]})

    for i, z in enumerate(a.zip):
        zp = part(a.dist, z)
        items.append({"id": f"engine-{i + 1}", "kind": "zip", "target": "bin",
                      "size": zp["size"], "sha256": zp["sha256"], "parts": [zp]})

    json.dump({"tag": a.tag, "repo": a.repo, "items": items}, sys.stdout, indent=2)
    sys.stdout.write("\n")


if __name__ == "__main__":
    main()
