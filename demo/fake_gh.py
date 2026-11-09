#!/usr/bin/env python3
"""A stand-in gh for demo/record.py: answers ccs's PR batch query with the
made-up PRs below, so the demo shows PR states without touching GitHub."""
import json, re, sys

PRS = {  # owner/repo#n: (title, state, mergeStateStatus, reviewDecision, rollup, failed check)
    "example/payments-api#812": ("Sort line items before summing", "OPEN", "CLEAN", "APPROVED", "SUCCESS", None),
    "example/payments-api#808": ("Pin the test database image", "MERGED", "UNKNOWN", "APPROVED", "SUCCESS", None),
    "example/payments-api#790": ("Retry webhooks with backoff", "MERGED", "UNKNOWN", "APPROVED", "SUCCESS", None),
    "example/infra#97": ("Lock timeout for migrations", "OPEN", "BLOCKED", "REVIEW_REQUIRED", "PENDING", None),
    "example/web#41": ("Single-column settings on mobile", "OPEN", "BLOCKED", "REVIEW_REQUIRED", "FAILURE", "lint"),
}

def pr(key):
    if key not in PRS:
        return None
    title, state, merge, review, rollup, failed = PRS[key]
    nodes = [{"name": "test", "conclusion": "SUCCESS" if rollup != "PENDING" else ""}]
    if failed:
        nodes.append({"name": failed, "conclusion": "FAILURE"})
    return {"pullRequest": {
        "number": int(key.split("#")[1]), "title": title, "state": state, "isDraft": False,
        "mergeStateStatus": merge, "reviewDecision": review,
        "reviewThreads": {"totalCount": 0, "nodes": []},
        "commits": {"nodes": [{"commit": {"statusCheckRollup": {"state": rollup, "contexts": {"nodes": nodes}}}}]},
    }}

args = sys.argv[1:]
query = next((a[len("query="):] for a in args if a.startswith("query=")), "")
data = {}
for alias, owner, name, num in re.findall(r'(p\d+):repository\(owner:"([^"]+)",name:"([^"]+)"\)\{pullRequest\(number:(\d+)\)', query):
    data[alias] = pr(f"{owner}/{name}#{num}")
print(json.dumps({"data": data}))
