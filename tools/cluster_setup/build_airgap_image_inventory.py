#!/usr/bin/env python3
"""Build and validate image metadata for a CPU-only offline AI Tier bundle."""
from __future__ import annotations
import argparse
import hashlib
import json
import re
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

def image_references(path: Path) -> list[str]:
    return sorted({line.strip() for line in path.read_text(encoding="utf-8").splitlines()
                   if line.strip() and not line.lstrip().startswith("#")})

def inspect_image(reference: str) -> dict[str, Any]:
    try:
        raw_result = subprocess.run(["skopeo", "inspect", "--raw", f"docker://{reference}"],
                                    check=True, capture_output=True, text=True)
        raw = raw_result.stdout.encode("utf-8").rstrip(b"\n")
        manifest = json.loads(raw)
        platforms = []
        for item in manifest.get("manifests", []):
            platform = item.get("platform", {})
            if platform.get("os") and platform.get("architecture"):
                entry = {"os": platform["os"], "architecture": platform["architecture"]}
                if item.get("digest"): entry["digest"] = item["digest"]
                if platform.get("variant"): entry["variant"] = platform["variant"]
                platforms.append(entry)
        if not platforms:
            detail = json.loads(subprocess.run(
                ["skopeo", "inspect", f"docker://{reference}"],
                check=True, capture_output=True, text=True).stdout)
            platforms = [{"os": detail["Os"], "architecture": detail["Architecture"]}]
        return {"reference": reference, "digest": "sha256:" + hashlib.sha256(raw).hexdigest(),
                "platforms": platforms}
    except (OSError, subprocess.CalledProcessError, json.JSONDecodeError, KeyError) as exc:
        reason = "registry inspection failed"
        if isinstance(exc, subprocess.CalledProcessError):
            detail = (exc.stderr or "").lower()
            if "unauthorized" in detail or "authentication required" in detail:
                reason = "registry authentication was rejected"
            elif "certificate" in detail or "x509" in detail or "tls" in detail:
                reason = "registry TLS trust verification failed"
        raise RuntimeError(f"{reason} for image {reference}") from exc

def mirror_reference(registry: str, reference: str) -> str | None:
    if not registry:
        return None
    first, separator, remainder = reference.partition("/")
    image_path = remainder if separator and ("." in first or ":" in first or first == "localhost") else reference
    return f"{registry.rstrip('/')}/{image_path}"

def build(args: argparse.Namespace) -> None:
    refs = image_references(args.images_file)
    if not refs: raise ValueError("image list is empty")
    imports, mapping = [], {}
    for list_path in sorted(args.image_lists_dir.glob("*.list")):
        entries = image_references(list_path)
        if not entries: continue
        archive_name = list_path.name.removesuffix(".list") + ".tar"
        if not (list_path.parent / archive_name).is_file():
            raise ValueError(f"image archive missing for import list {list_path.name}")
        archive = f"images/{archive_name}"
        imports.append({"archive": archive, "imageList": f"images/{list_path.name}"})
        for ref in entries: mapping.setdefault(ref, []).append(archive)
    if set(refs) - set(mapping): raise ValueError("image inventory contains refs without an import archive")
    images = []
    for ref in refs:
        item = inspect_image(ref)
        item["importArchives"] = sorted(mapping[ref])
        item["mirrorReference"] = mirror_reference(args.mirror_registry, ref)
        low = ref.lower()
        item["role"] = "las-agent-server" if "langgraph" in low or "las-sample-agent" in low else (
            "ai-tier-operator" if "splunk-ai-operator" in low else (
            "metrics" if "metrics-server" in low or "prometheus" in low else "cluster-or-product"))
        images.append(item)
    result = {"schemaVersion":1,"profile":args.profile,
              "generatedAt":datetime.now(timezone.utc).isoformat(),
              "imageImports":imports,"images":images}
    if args.embedded_chart:
        chart=args.embedded_chart
        result["embeddedDistribution"]={"path":f"charts/{chart.name}",
            "sha256":hashlib.sha256(chart.read_bytes()).hexdigest()}
    args.output.write_text(json.dumps(result,indent=2)+"\n",encoding="utf-8")

def safe_bundle_path(bundle: Path, relative: str) -> Path:
    path=Path(relative)
    if path.is_absolute() or ".." in path.parts: raise ValueError("inventory contains an unsafe bundle path")
    resolved=(bundle/path).resolve()
    if bundle.resolve() not in resolved.parents: raise ValueError("inventory contains an unsafe bundle path")
    return resolved

def validate(args: argparse.Namespace) -> None:
    bundle=args.bundle_dir.resolve()
    inventory=json.loads(args.inventory.read_text(encoding="utf-8"))
    if inventory.get("schemaVersion") != 1: raise ValueError("unsupported image inventory schemaVersion")
    if inventory.get("profile") not in ("cpuOnly","full"): raise ValueError("unsupported image inventory profile")
    images=inventory.get("images")
    if not isinstance(images,list) or not images: raise ValueError("image inventory has no images")
    refs=set()
    for item in images:
        if not isinstance(item,dict): raise ValueError("invalid image inventory entry")
        ref,digest,platforms=item.get("reference"),item.get("digest"),item.get("platforms")
        if not isinstance(ref,str) or not ref: raise ValueError("image inventory entry is missing its reference")
        if not isinstance(digest,str) or not DIGEST_RE.fullmatch(digest): raise ValueError(f"image inventory entry has an invalid digest: {ref}")
        if not isinstance(platforms,list) or not platforms or any(
            not isinstance(p,dict) or not p.get("os") or not p.get("architecture") for p in platforms):
            raise ValueError(f"image inventory entry is missing architecture metadata: {ref}")
        refs.add(ref)
    imports=inventory.get("imageImports")
    if not isinstance(imports,list) or not imports: raise ValueError("image inventory does not list image import archives")
    archives=set()
    for entry in imports:
        if not isinstance(entry,dict) or not entry.get("archive") or not entry.get("imageList"):
            raise ValueError("invalid image import mapping")
        archive=safe_bundle_path(bundle,entry["archive"]); listing=safe_bundle_path(bundle,entry["imageList"])
        if not archive.is_file(): raise ValueError(f"required image archive is missing: {archive.name}")
        if not listing.is_file(): raise ValueError(f"required image list is missing: {listing.name}")
        if not set(image_references(listing)).issubset(refs): raise ValueError(f"image inventory does not cover imports in {listing.name}")
        archives.add(entry["archive"])
    for item in images:
        mapped=item.get("importArchives")
        if not isinstance(mapped,list) or not mapped or not set(mapped).issubset(archives):
            raise ValueError(f"image has no valid import mapping: {item['reference']}")
    distribution=inventory.get("embeddedDistribution")
    if distribution:
        chart=safe_bundle_path(bundle,distribution.get("path",""))
        if not chart.is_file(): raise ValueError("embedded LAS chart artifact is missing")
        if hashlib.sha256(chart.read_bytes()).hexdigest()!=distribution.get("sha256"):
            raise ValueError("embedded LAS chart digest does not match the inventory")

def main() -> int:
    parser=argparse.ArgumentParser(description=__doc__)
    cmds=parser.add_subparsers(dest="command",required=True)
    bp=cmds.add_parser("build")
    bp.add_argument("--images-file",required=True,type=Path)
    bp.add_argument("--image-lists-dir",required=True,type=Path)
    bp.add_argument("--profile",choices=("full","cpuOnly"),required=True)
    bp.add_argument("--mirror-registry",default="")
    bp.add_argument("--embedded-chart",type=Path)
    bp.add_argument("--output",required=True,type=Path)
    bp.set_defaults(func=build)
    vp=cmds.add_parser("validate")
    vp.add_argument("--inventory",required=True,type=Path)
    vp.add_argument("--bundle-dir",required=True,type=Path)
    vp.set_defaults(func=validate)
    args=parser.parse_args()
    try: args.func(args)
    except (RuntimeError,ValueError,OSError,json.JSONDecodeError) as exc:
        print(f"ERROR: {exc}",file=sys.stderr); return 1
    return 0

if __name__=="__main__": raise SystemExit(main())
