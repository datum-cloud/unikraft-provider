#!/usr/bin/env python3
"""Datum log-metadata reconciler.

Joins node-local ukpd microVMs to their Datum compute Instance identity and
maintains an identity-encoded symlink tree that the OpenTelemetry filelog
receiver tails. It never touches a log byte — it only manages symlinks.

The join, per instance, is:

  vm.log dir name (ukpd uuid)                       -- /var/lib/ukp/data/platform/<uuid>/
    -> guest IP from vmm.json boot_args (netdev.ip) -- node-local, no ukpd token
    -> provider Pod with that podIP                 -- meta.datumapis.com/upstream-{name,namespace}
    -> that Pod's namespace                         -- <project-label> on the Namespace

producing the symlink

  <tree>/project=<p>/ns=<ns>/instance=<name>/uuid=<uuid>/vm.log -> the real vm.log

which the stock filelog receiver turns into resource attributes via a path
regex. Kubernetes is read through the in-cluster ServiceAccount (urllib only,
no kubectl / client-go).
"""
import glob
import json
import os
import re
import ssl
import time
import urllib.parse
import urllib.request

PLATFORM = os.environ.get("UKP_PLATFORM_DIR", "/var/lib/ukp/data/platform")
TREE = os.environ.get("UKP_LOG_TREE", "/var/log/ukp-logs")
PROJECT_LABEL = os.environ.get("PROJECT_LABEL", "resourcemanager.miloapis.com/project-name")
POD_SELECTOR = os.environ.get("POD_SELECTOR", "managed-by=infra-provider-unikraft")
POLL = int(os.environ.get("POLL_SECONDS", "5"))

_SA = "/var/run/secrets/kubernetes.io/serviceaccount"
_API = os.environ.get("KUBERNETES_API", "https://kubernetes.default.svc")
_TOKEN = open(f"{_SA}/token").read().strip()
_CTX = ssl.create_default_context(cafile=f"{_SA}/ca.crt")
_IP_RE = re.compile(r'netdev\.ip=["\\]*([0-9.]+)')


def _api_get(path):
    req = urllib.request.Request(_API + path, headers={"Authorization": f"Bearer {_TOKEN}"})
    with urllib.request.urlopen(req, context=_CTX, timeout=10) as resp:
        return json.load(resp)


def _ip_to_identity():
    """podIP -> (project, namespace, instance) from the provider's Pods."""
    sel = urllib.parse.quote(POD_SELECTOR)
    pods = _api_get(f"/api/v1/pods?labelSelector={sel}")
    ns_project, ip_map = {}, {}
    for pod in pods.get("items", []):
        ip = (pod.get("status") or {}).get("podIP")
        ann = (pod.get("metadata") or {}).get("annotations") or {}
        instance = ann.get("meta.datumapis.com/upstream-name")
        namespace = ann.get("meta.datumapis.com/upstream-namespace")
        if not (ip and instance and namespace):
            continue
        if namespace not in ns_project:
            try:
                labels = (_api_get(f"/api/v1/namespaces/{namespace}").get("metadata") or {}).get("labels") or {}
            except Exception:
                labels = {}
            ns_project[namespace] = labels.get(PROJECT_LABEL, "unknown")
        ip_map[ip] = (ns_project[namespace], namespace, instance)
    return ip_map


def _link_instances(ip_map):
    """Create a symlink per running instance whose IP maps to a Datum Pod."""
    for inst_dir in glob.glob(os.path.join(PLATFORM, "*", "")):
        uuid = os.path.basename(inst_dir.rstrip("/"))
        vmlog = os.path.join(inst_dir, "vm.log")
        vmm = os.path.join(inst_dir, "vmm.json")
        if not (os.path.exists(vmlog) and os.path.exists(vmm)):
            continue
        try:
            match = _IP_RE.search(open(vmm).read())
        except OSError:
            continue
        if not match or match.group(1) not in ip_map:
            continue
        project, namespace, instance = ip_map[match.group(1)]
        link_dir = os.path.join(
            TREE, f"project={project}", f"ns={namespace}", f"instance={instance}", f"uuid={uuid}"
        )
        os.makedirs(link_dir, exist_ok=True)
        link = os.path.join(link_dir, "vm.log")
        if not os.path.islink(link):
            try:
                os.symlink(vmlog, link)
            except FileExistsError:
                pass


def _prune():
    """Drop symlinks whose target vm.log is gone (instance removed by ukpd)."""
    for link in glob.glob(os.path.join(TREE, "**", "vm.log"), recursive=True):
        if not (os.path.islink(link) and not os.path.exists(os.path.realpath(link))):
            continue
        try:
            os.unlink(link)
            parent = os.path.dirname(link)
            while os.path.realpath(parent) != os.path.realpath(TREE) and os.path.isdir(parent) and not os.listdir(parent):
                os.rmdir(parent)
                parent = os.path.dirname(parent)
        except OSError:
            pass


def reconcile():
    ip_map = _ip_to_identity()
    _link_instances(ip_map)
    _prune()
    return len(ip_map)


if __name__ == "__main__":
    os.makedirs(TREE, exist_ok=True)
    while True:
        try:
            print(f"reconciled {reconcile()} datum instance(s)", flush=True)
        except Exception as exc:  # keep the loop alive across transient API errors
            print(f"reconcile error: {exc}", flush=True)
        time.sleep(POLL)
